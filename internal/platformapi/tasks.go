package platformapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
	"sandbox-platform/internal/tasks"
)

type taskRequest struct {
	tasks.Spec
	TaskKey            string `json:"task_key,omitempty"`
	ExpectedGeneration int64  `json:"expected_generation,omitempty"`
}

func (s *Server) taskRoutes() {
	s.Extra["task_start"] = s.startTask
	s.Extra["task_cancel"] = s.cancelTask
	s.route("POST /v1/machines/{machine}/tasks", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		id := r.PathValue("machine")
		if err := p.Authorize("manage", id); err != nil {
			return err
		}
		var v taskRequest
		if err := decode(r, &v); err != nil {
			return err
		}
		if len(v.Argv) == 0 {
			return registry.ErrInvalid
		}
		if v.TaskKey == "" {
			v.TaskKey = r.Header.Get("Idempotency-Key")
		}
		return s.accept(w, r, p, v, registry.AcceptRequest{Kind: "task_start", MachineID: id, ExpectedGeneration: v.ExpectedGeneration})
	})
	s.route("GET /v1/machines/{machine}/tasks", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		id := r.PathValue("machine")
		if err := p.Authorize("read", id); err != nil {
			return err
		}
		rows, err := s.DB.ReadDB().QueryContext(r.Context(), "SELECT result_json FROM tasks WHERE machine_id=? ORDER BY id", id)
		if err != nil {
			return err
		}
		defer rows.Close()
		out := []tasks.Task{}
		for rows.Next() {
			var raw []byte
			var t tasks.Task
			if err = rows.Scan(&raw); err != nil {
				return err
			}
			if err = json.Unmarshal(raw, &t); err != nil {
				return err
			}
			out = append(out, t)
		}
		if err = rows.Err(); err != nil {
			return err
		}
		return list(w, r, out, func(t tasks.Task) string { return t.ID })
	})
	for _, suffix := range []string{"", "/output", "/cancel"} {
		method := "GET "
		if suffix == "/cancel" {
			method = "POST "
		}
		s.route(method+"/v1/tasks/{task}"+suffix, func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
			id := r.PathValue("task")
			var mid string
			var gen int64
			var raw []byte
			err := s.DB.ReadDB().QueryRowContext(r.Context(), "SELECT machine_id,binding_generation,result_json FROM tasks WHERE id=?", id).Scan(&mid, &gen, &raw)
			if err != nil {
				return err
			}
			action := "read"
			if suffix == "/cancel" {
				action = "manage"
			}
			if err = p.Authorize(action, mid); err != nil {
				return err
			}
			if suffix == "/cancel" {
				var body struct {
					Grace *int `json:"grace_seconds"`
				}
				if err := decode(r, &body); err != nil {
					return err
				}
				return s.accept(w, r, p, tasks.Request{Method: "cancel", TaskID: id, GraceSeconds: body.Grace}, registry.AcceptRequest{Kind: "task_cancel", MachineID: mid, ExpectedGeneration: gen})
			}
			m, err := s.DB.GetMachine(r.Context(), mid)
			if err != nil {
				return err
			}
			if m.CurrentBinding == nil || m.BindingGeneration != gen || m.ObservedState != "running" || m.ActiveOperationID != "" {
				if suffix == "" {
					return write(w, 200, json.RawMessage(raw))
				}
				return &lifecycle.Failure{Code: "SERVICE_NOT_READY", Message: "task output currently unavailable"}
			}
			req := tasks.Request{Method: "status", TaskID: id}
			if suffix == "/output" {
				req.Method = "output"
				req.Stream = r.URL.Query().Get("stream")
				if req.Stream == "" {
					req.Stream = "stdout"
				}
				req.Limit = 65536
				if t := r.URL.Query().Get("limit"); t != "" {
					req.Limit, err = strconv.Atoi(t)
					if err != nil || req.Limit < 1 || req.Limit > 65536 {
						return registry.ErrInvalid
					}
				}
				if t := r.URL.Query().Get("offset"); t != "" {
					req.Offset, err = strconv.ParseInt(t, 10, 64)
					if err != nil || req.Offset < 0 {
						return registry.ErrInvalid
					}
				}
			}
			res, err := s.Call(r.Context(), *m.CurrentBinding, req)
			if err != nil {
				if suffix == "" {
					return write(w, 200, json.RawMessage(raw))
				}
				return err
			}
			if suffix == "" {
				if res.Task == nil {
					return errors.New("missing guest task record")
				}
				if err = s.observeTask(r.Context(), m.CurrentBinding.ID, *res.Task); err != nil {
					return err
				}
				return write(w, 200, res.Task)
			}
			o := res.Output
			if o == nil {
				return errors.New("missing guest output record")
			}
			return write(w, 200, map[string]any{"data_base64": o.Data, "start": o.Start, "end": o.End, "next": o.Next, "gap": o.Gap, "complete": o.Complete, "dropped": o.Dropped})
		})
	}
}
func (s *Server) startTask(ctx context.Context, j *lifecycle.Job) error {
	var req taskRequest
	if err := json.Unmarshal(j.Op.Request, &req); err != nil {
		return err
	}
	m, err := s.DB.GetMachine(ctx, j.Op.MachineID)
	if err != nil {
		return err
	}
	if m.CurrentBinding == nil || m.ObservedState != "running" {
		return &lifecycle.Failure{Code: "SERVICE_NOT_READY", Message: "task start needs a running managed workspace"}
	}
	if j.Binding.GuestManagementStatus != "ready" {
		return &lifecycle.Failure{Code: "CAPABILITY_NOT_VERIFIED", Message: "guest manager is unavailable"}
	}
	id := "task_" + j.Op.ID[3:]
	specHash, err := tasks.HashSpec(req.Spec)
	if err != nil {
		return err
	}
	if err = j.Stage(ctx, "guest_start", ""); err != nil {
		return err
	}
	res, err := s.Call(ctx, j.Binding, tasks.Request{Method: "start", StartToken: req.TaskKey, TaskID: id, Spec: &req.Spec, SpecHash: specHash, OperationID: j.Op.ID})
	if err != nil {
		var rpc *tasks.RPCError
		if !errors.As(err, &rpc) || rpc.Code == "OUTCOME_UNKNOWN" {
			j.Written = true
		}
		return err
	}
	j.Written = true
	if res.Task == nil || res.Task.Namespace != j.Binding.TaskNamespace || res.Task.MachineID != m.ID {
		return errors.New("guest task identity mismatch")
	}
	t := *res.Task
	return j.Done(ctx, registry.TransitionRequest{Effect: "applied", Result: lifecycle.JSON(map[string]string{"task_id": t.ID}), Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO tasks(id,machine_id,binding_generation,task_namespace,guest_task_id,start_token,spec_hash,spec_json,state,exit_code,observed_at,result_json)
 VALUES(?,?,?,?,?,?,?,'{}',?,?,?,?) ON CONFLICT(id) DO UPDATE SET state=excluded.state,result_json=excluded.result_json,exit_code=excluded.exit_code,observed_at=excluded.observed_at WHERE tasks.task_namespace=excluded.task_namespace AND tasks.start_token=excluded.start_token AND tasks.spec_hash=excluded.spec_hash`, t.ID, m.ID, j.Binding.Generation, t.Namespace, t.ID, req.TaskKey, specHash, t.State, t.ExitCode, t.ObservedAt.UnixMilli(), string(lifecycle.JSON(t)))
		if err != nil {
			return err
		}
		return registry.AppendEvent(ctx, tx, "task", m.ID, j.Op.ID, map[string]string{"task_id": t.ID, "state": t.State}, false)
	}})
}
func (s *Server) cancelTask(ctx context.Context, j *lifecycle.Job) error {
	var req tasks.Request
	if err := json.Unmarshal(j.Op.Request, &req); err != nil {
		return err
	}
	req.OperationID = j.Op.ID
	if err := j.Stage(ctx, "guest_cancel", ""); err != nil {
		return err
	}
	res, err := s.Call(ctx, j.Binding, req)
	if err != nil {
		j.Written = true
		return err
	}
	j.Written = true
	if res.Task == nil {
		return errors.New("guest did not return cancellation state")
	}
	until := time.Now().Add(15 * time.Second)
	if req.GraceSeconds != nil {
		until = time.Now().Add(time.Duration(*req.GraceSeconds+6) * time.Second)
	}
	for res.Task.State == "cancelling" && time.Now().Before(until) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
		res, err = s.Call(ctx, j.Binding, tasks.Request{Method: "status", TaskID: req.TaskID})
		if err != nil {
			return err
		}
		if res.Task == nil {
			return errors.New("missing cancellation receipt")
		}
	}
	if !res.Task.Terminal() {
		return errors.New("guest cancellation outcome requires observation")
	}
	if err := s.observeTask(ctx, j.Binding.ID, *res.Task); err != nil {
		return err
	}
	return j.Done(ctx, registry.TransitionRequest{Effect: "applied", Result: lifecycle.JSON(res.Task)})
}
func (s *Server) observeTask(ctx context.Context, bid string, t tasks.Task) error {
	_, err := s.DB.DB().ExecContext(ctx, `UPDATE tasks SET result_json=?, observed_at=?,exit_code=?,state=? WHERE id=? AND task_namespace=? AND observed_at<=? AND EXISTS(SELECT 1 FROM machines WHERE id=? AND current_binding_id=?)`, string(lifecycle.JSON(t)), t.ObservedAt.UnixMilli(), t.ExitCode, t.State, t.ID, t.Namespace, t.ObservedAt.UnixMilli(), t.MachineID, bid)
	return err
}
