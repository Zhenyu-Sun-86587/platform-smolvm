package platformapi

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/execbridge"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
)

const terminalProtocol = "sandbox.terminal.v1"

type execRequest struct {
	Argv        []string          `json:"argv"`
	Cwd         string            `json:"cwd,omitempty"`
	User        string            `json:"user,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	TTY         bool              `json:"tty,omitempty"`
	Interactive bool              `json:"interactive,omitempty"`
	Cols        uint16            `json:"cols,omitempty"`
	Rows        uint16            `json:"rows,omitempty"`
	Wait        uint64            `json:"wait_seconds,omitempty"`
	Timeout     uint64            `json:"timeout_seconds,omitempty"`
}

type execRecord struct {
	ID        string      `json:"exec_id"`
	Machine   string      `json:"machine_id"`
	State     string      `json:"state"`
	ExitCode  *int        `json:"exit_code,omitempty"`
	Binding   string      `json:"-"`
	Owner     string      `json:"-"`
	Upstream  string      `json:"-"`
	Runtime   string      `json:"-"`
	Current   string      `json:"-"`
	Bundle    string      `json:"-"`
	BootMode  rt.BootMode `json:"-"`
	OutputEnd uint64      `json:"-"`
}

const execColumns = `x.id,x.machine_id,x.state,x.exit_code,x.binding_id,x.principal_id,x.upstream_exec_id,b.runtime_name,
	coalesce(m.current_binding_id,''),coalesce(b.runtime_bundle_id,''),coalesce(json_extract(m.spec_json,'$.boot_mode'),''),x.output_end`
const execTables = " FROM exec_sessions x JOIN machines m ON x.machine_id=m.id JOIN runtime_bindings b ON b.id=x.binding_id"

type execScanner interface {
	Scan(...any) error
}

func scanExec(row execScanner) (execRecord, error) {
	var x execRecord
	err := row.Scan(&x.ID, &x.Machine, &x.State, &x.ExitCode, &x.Binding, &x.Owner, &x.Upstream, &x.Runtime,
		&x.Current, &x.Bundle, &x.BootMode, &x.OutputEnd)
	return x, err
}

func (s *Server) execRoutes() {
	const machine = "/v1/machines/{machine}"
	const session = "/v1/execs/{exec}"
	s.route("GET "+machine+"/execs", s.listExecs)
	s.route("POST "+machine+"/execs", s.startExec)
	s.route("POST "+machine+"/exec", s.startExec)
	s.route("GET "+session, s.getExec)
	s.route("GET "+session+"/output", s.execOutput)
	s.route("POST "+session+"/stdin", s.execInput)
	s.route("POST "+session+"/resize", s.execResize)
	s.route("DELETE "+session, s.deleteExec)
	s.route("POST "+session+"/attach-ticket", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		x, err := s.authorizedExec(r.Context(), p, r.PathValue("exec"), false)
		if err != nil {
			return err
		}
		return s.issueTicket(w, r, x.ID)
	})
	s.mux.HandleFunc("GET "+session+"/attach", func(w http.ResponseWriter, r *http.Request) {
		if err := s.attachExec(w, r); err != nil {
			fail(w, r, err)
		}
	})
	s.reconcileExecSessions()
}

func (s *Server) reconcileExecSessions() {
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	// A platform process owns the direct Agent socket. The Agent reaps the
	// command when that process exits. Never dispatch it again from SQLite.
	_, _ = s.DB.DB().ExecContext(ctx, `UPDATE exec_sessions SET state='lost' WHERE state='reserved' OR
		(state IN ('starting','running') AND binding_id IN (SELECT id FROM runtime_bindings WHERE coalesce(runtime_bundle_id,'')!=''))`)
	rows, err := s.DB.ReadDB().QueryContext(ctx, "SELECT "+execColumns+execTables+" WHERE x.state NOT IN ('failed','exited','done','lost')")
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		x, err := scanExec(rows)
		if err == nil {
			s.streamFor(x)
		}
	}
}

func (s *Server) authorizedExec(ctx context.Context, p auth.Principal, id string, write bool) (execRecord, error) {
	x, err := scanExec(s.DB.ReadDB().QueryRowContext(ctx, "SELECT "+execColumns+execTables+" WHERE x.id=?", id))
	if err != nil {
		return x, err
	}
	if write && (p.ID != x.Owner || !p.Allows(auth.ActionManage, x.Machine)) {
		return x, auth.ErrForbidden
	}
	return x, p.Authorize(auth.ActionRead, x.Machine)
}

func (s *Server) listExecs(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	machine := r.PathValue("machine")
	if err := p.Authorize(auth.ActionRead, machine); err != nil {
		return err
	}
	ctx := r.Context()
	if _, err := s.DB.GetMachine(ctx, machine); err != nil {
		return err
	}
	rows, err := s.DB.ReadDB().QueryContext(ctx, "SELECT "+execColumns+execTables+" WHERE x.machine_id=? ORDER BY x.rowid DESC", machine)
	if err != nil {
		return err
	}
	defer rows.Close()
	var result []execRecord
	for rows.Next() {
		x, err := scanExec(rows)
		if err != nil {
			return err
		}
		if x.Binding != x.Current && !execTerminal(x.State) {
			x.State = "session_lost"
		}
		result = append(result, x)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return list(w, r, result, func(x execRecord) string { return x.ID })
}

func (s *Server) startExec(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	machine := r.PathValue("machine")
	if err := p.Authorize(auth.ActionManage, machine); err != nil {
		return err
	}
	var spec execRequest
	if err := decode(r, &spec); err != nil {
		return err
	}
	if len(spec.Argv) == 0 || spec.Argv[0] == "" || len(spec.Argv) > 1024 || spec.Wait > 30 || spec.Timeout > 86400 {
		return registry.ErrInvalid
	}
	for _, arg := range spec.Argv {
		if strings.ContainsRune(arg, 0) {
			return registry.ErrInvalid
		}
	}
	if spec.Cwd != "" && (!path.IsAbs(spec.Cwd) || path.Clean(spec.Cwd) != spec.Cwd || strings.ContainsRune(spec.Cwd, 0)) {
		return registry.ErrInvalid
	}
	ctx := r.Context()
	if spec.Cwd == "" {
		if err := s.defaultExecDirectory(ctx, machine, &spec); err != nil {
			return err
		}
	}
	if spec.User == "" || spec.User == "root" {
		spec.User = "0:0"
	}
	if spec.TTY && (spec.Cols == 0 || spec.Rows == 0) {
		spec.Cols, spec.Rows = 80, 24
	}
	env := envelope(r, p, spec)
	var err error
	if r.Header.Get("If-Match") != "" {
		env.ExpectedRevision, err = revision(r)
		if err != nil {
			return err
		}
	}
	// Replays must never wake the old binding or dispatch another command.
	// SQLite first records the exact ID. During a response-loss window
	// callers can separately inspect that ID and its native session.
	id, err := registry.NewID("exec_")
	if err != nil {
		return err
	}
	hash := sha256.Sum256(env.Request)
	raw, replay, err := s.DB.Mutate(ctx, env, func(ctx context.Context, tx *sql.Tx) (registry.MutationResult, error) {
		binding, err := checkAccessRevision(ctx, tx, machine, env.ExpectedRevision, "")
		if err != nil {
			return registry.MutationResult{}, err
		}
		var active int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM exec_sessions WHERE state IN ('reserved','starting','running')").Scan(&active); err != nil {
			return registry.MutationResult{}, err
		}
		if active >= 256 {
			return registry.MutationResult{}, &lifecycle.Failure{Code: "RESOURCE_EXHAUSTED", Message: "too many active native exec sessions"}
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO exec_sessions(id,machine_id,binding_id,upstream_exec_id,principal_id,spec_hash,state) VALUES (?,?,?,?,?,?,?)",
			id, machine, binding, id, p.ID, hex.EncodeToString(hash[:]), "reserved")
		return registry.MutationResult{Response: lifecycle.JSON(map[string]string{"exec_id": id})}, err
	})
	if err != nil {
		return err
	}
	var ref struct {
		ID string `json:"exec_id"`
	}
	if err := json.Unmarshal(raw, &ref); err != nil {
		return err
	}
	x, err := s.authorizedExec(ctx, p, ref.ID, true)
	if err != nil {
		return err
	}
	if !replay {
		x, err = s.dispatchExec(ctx, p, x, spec)
		if err != nil {
			return err
		}
	}
	w.Header().Set("Location", "/v1/execs/"+x.ID)
	return s.execView(w, x, 0, 64<<10)
}

func (s *Server) dispatchExec(ctx context.Context, p auth.Principal, x execRecord, spec execRequest) (execRecord, error) {
	if x.Bundle != "" && (x.BootMode != rt.BootSystem || spec.User != "0:0") {
		s.saveExec(x.ID, execbridge.Snapshot{State: "failed"})
		return x, rt.ErrUnsupported
	}
	b, release, err := s.Access(ctx, p, x.Machine)
	if err != nil {
		s.saveExec(x.ID, execbridge.Snapshot{State: "failed"})
		return x, err
	}
	if b.ID != x.Binding {
		release()
		s.saveExec(x.ID, execbridge.Snapshot{State: "failed"})
		return x, registry.ErrGeneration
	}
	adapter, err := s.ForBinding(b)
	if err != nil {
		release()
		s.saveExec(x.ID, execbridge.Snapshot{State: "failed"})
		return x, err
	}
	stream := execbridge.New(0, "starting")
	s.execStreams.Store(x.ID, stream)
	updated, err := s.DB.DB().ExecContext(ctx, "UPDATE exec_sessions SET state='starting' WHERE id=? AND state='reserved'", x.ID)
	if err == nil {
		var count int64
		count, err = updated.RowsAffected()
		if err == nil && count != 1 {
			err = registry.ErrGeneration
		}
	}
	if err != nil {
		s.execStreams.CompareAndDelete(x.ID, stream)
		release()
		return x, err
	}
	request := rt.ExecSessionRequest{
		ExecRequest: rt.ExecRequest{Command: spec.Argv, Cwd: spec.Cwd, User: spec.User, Env: spec.Env, TimeoutSeconds: spec.Timeout},
		ID:          x.Upstream, TTY: spec.TTY, Interactive: spec.Interactive, Cols: spec.Cols, Rows: spec.Rows,
		WaitSeconds: spec.Wait, MaxBytes: 64 << 10,
	}
	if x.Bundle != "" {
		return x, s.dispatchGuestExec(ctx, p, x, b, adapter, request, stream, release)
	}
	native, err := adapter.StartExec(ctx, x.Runtime, request)
	if err != nil {
		if !errors.Is(err, rt.ErrUncertain) {
			release()
			s.saveExec(x.ID, execbridge.Snapshot{State: "failed"})
			s.execStreams.CompareAndDelete(x.ID, stream)
			return x, err
		}
		// The native ID was already recorded. Query that ID once the
		// connection is restored even when StartExec lost its response.
		s.runExec(x, stream, nil, release)
		return x, &lifecycle.Failure{Code: "OUTCOME_UNKNOWN", Message: "inspect native exec " + x.ID + " before starting another command"}
	}
	s.runExec(x, stream, &native, release)
	x.State = native.State
	return x, nil
}

func execTerminal(state string) bool {
	switch state {
	case "failed", "exited", "done", "lost", "session_lost":
		return true
	default:
		return false
	}
}

func (s *Server) saveExec(id string, o execbridge.Snapshot) {
	state := o.State
	if state == "session_lost" {
		state = "lost"
	}
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	_, _ = s.DB.DB().ExecContext(ctx, "UPDATE exec_sessions SET state=?,exit_code=?,output_start=?,output_end=?,last_observed_at=? WHERE id=? AND state!='lost'",
		state, o.ExitCode, o.RetainedFrom, o.End, time.Now().UnixMilli(), id)
}

func (s *Server) watchExec(x execRecord, initial *rt.ExecSession, release func()) *execbridge.Stream {
	stream := execbridge.New(x.OutputEnd, x.State)
	existing, loaded := s.execStreams.LoadOrStore(x.ID, stream)
	if loaded {
		release()
		return existing.(*execbridge.Stream)
	}
	s.runExec(x, stream, initial, release)
	return stream
}

func (s *Server) runExec(x execRecord, stream *execbridge.Stream, initial *rt.ExecSession, release func()) {
	if initial != nil {
		stream.Observe(*initial)
		view, _ := stream.Read(0, 0)
		s.saveExec(x.ID, view)
	}
	go func() {
		defer func() {
			time.AfterFunc(5*time.Minute, func() { s.execStreams.CompareAndDelete(x.ID, stream) })
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		adapter, err := s.execRuntime(ctx, x)
		cancel()
		if err != nil {
			stream.Lost("native exec session is unavailable")
			view, _ := stream.Read(0, 0)
			s.saveExec(x.ID, view)
			release()
			return
		}
		stream.Run(context.Background(), func(ctx context.Context, read rt.ExecRead) (rt.ExecSession, error) {
			return adapter.ReadExec(ctx, x.Runtime, x.Upstream, read)
		}, func(view execbridge.Snapshot) {
			s.saveExec(x.ID, view)
		}, release)
	}()
}

func (s *Server) execRuntime(ctx context.Context, x execRecord) (rt.Adapter, error) {
	b, err := s.DB.GetBinding(ctx, x.Binding)
	if err != nil {
		return nil, err
	}
	if b.MachineID != x.Machine || b.RuntimeName != x.Runtime || b.Status != "current" {
		return nil, registry.ErrStaleObservation
	}
	return s.ForBinding(b)
}

func (s *Server) streamFor(x execRecord) *execbridge.Stream {
	if existing, ok := s.execStreams.Load(x.ID); ok {
		stream := existing.(*execbridge.Stream)
		if x.Bundle != "" && (x.Binding != x.Current || x.State == "lost") {
			if value, ok := s.guestExecs.Load(x.ID); ok {
				value.(*guestExec).session.Close("Guest exec execution is no longer current")
			}
			stream.Lost("Guest exec execution is no longer current")
		}
		return stream
	}
	if x.State == "reserved" && x.Binding == x.Current {
		return execbridge.New(0, "starting")
	}
	if x.Binding != x.Current || x.State == "lost" || x.Bundle != "" && !execTerminal(x.State) {
		stream := execbridge.New(x.OutputEnd, "session_lost")
		stream.Lost("native exec session is unavailable")
		if !execTerminal(x.State) {
			s.saveExec(x.ID, execbridge.Snapshot{State: "session_lost", End: x.OutputEnd})
		}
		return stream
	}
	if execTerminal(x.State) {
		stream := execbridge.New(x.OutputEnd, x.State)
		stream.Observe(rt.ExecSession{State: x.State, Done: true, ExitCode: x.ExitCode})
		return stream
	}
	return s.watchExec(x, nil, func() {})
}

func (s *Server) getExec(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	x, err := s.authorizedExec(r.Context(), p, r.PathValue("exec"), false)
	if err != nil {
		return err
	}
	return s.execView(w, x, 0, 0)
}

func (s *Server) execOutput(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	x, err := s.authorizedExec(r.Context(), p, r.PathValue("exec"), false)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	cursor, limit := uint64(0), 64<<10
	if q.Has("cursor") {
		cursor, err = strconv.ParseUint(q.Get("cursor"), 10, 63)
		if err != nil {
			return registry.ErrInvalid
		}
	}
	if q.Has("limit") {
		limit, err = strconv.Atoi(q.Get("limit"))
		if err != nil || limit < 1 || limit > 512<<10 {
			return registry.ErrInvalid
		}
	}
	return s.execView(w, x, cursor, limit)
}

type execResponse struct {
	ID      string `json:"exec_id"`
	Machine string `json:"machine_id"`
	execbridge.Snapshot
}

func (s *Server) execView(w http.ResponseWriter, x execRecord, cursor uint64, limit int) error {
	view, _ := s.streamFor(x).Read(cursor, limit)
	return write(w, 200, execResponse{ID: x.ID, Machine: x.Machine, Snapshot: view})
}

type terminalInput struct {
	Type string `json:"type,omitempty"`
	Seq  uint64 `json:"seq,omitempty"`
	Data []byte `json:"data_base64,omitempty"`
	EOF  bool   `json:"eof,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
}

func (s *Server) execInput(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	x, err := s.authorizedExec(r.Context(), p, r.PathValue("exec"), true)
	if err != nil {
		return err
	}
	var in terminalInput
	if err := decode(r, &in); err != nil {
		return err
	}
	seq, err := s.applyExecInput(r.Context(), x, in)
	if err != nil {
		return err
	}
	return write(w, 200, map[string]uint64{"stdin_seq": seq})
}

func (s *Server) applyExecInput(ctx context.Context, x execRecord, in terminalInput) (uint64, error) {
	if x.Binding != x.Current || execTerminal(x.State) {
		return 0, execLost()
	}
	if x.Bundle != "" {
		session, err := s.currentGuestExec(ctx, x)
		if err != nil {
			return 0, err
		}
		seq, err := session.Input(ctx, in.Seq, in.Data, in.EOF)
		return seq, guestExecError(err)
	}
	adapter, err := s.execRuntime(ctx, x)
	if err != nil {
		return 0, err
	}
	seq, err := adapter.WriteExec(ctx, x.Runtime, x.Upstream, in.Seq, in.Data, in.EOF)
	if err == nil {
		s.streamFor(x).Applied(seq)
	}
	return seq, err
}

func (s *Server) execResize(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	x, err := s.authorizedExec(r.Context(), p, r.PathValue("exec"), true)
	if err != nil {
		return err
	}
	var in terminalInput
	if err := decode(r, &in); err != nil {
		return err
	}
	if err := s.resizeExec(r.Context(), x, in); err != nil {
		return err
	}
	return write(w, 200, map[string]bool{"resized": true})
}

func (s *Server) resizeExec(ctx context.Context, x execRecord, in terminalInput) error {
	if x.Binding != x.Current || execTerminal(x.State) {
		return execLost()
	}
	if x.Bundle != "" {
		session, err := s.currentGuestExec(ctx, x)
		if err != nil {
			return err
		}
		return guestExecError(session.Resize(ctx, in.Cols, in.Rows))
	}
	adapter, err := s.execRuntime(ctx, x)
	if err != nil {
		return err
	}
	return adapter.ResizeExec(ctx, x.Runtime, x.Upstream, in.Cols, in.Rows)
}

func (s *Server) deleteExec(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	x, err := s.authorizedExec(r.Context(), p, r.PathValue("exec"), true)
	if err != nil {
		return err
	}
	if x.Binding == x.Current && !execTerminal(x.State) {
		if x.Bundle != "" {
			if value, ok := s.guestExecs.Load(x.ID); ok {
				value.(*guestExec).session.Close("Guest exec session terminated")
			} else if x.State == "reserved" || x.State == "starting" {
				return registry.ErrOperationActive
			}
			return s.execView(w, x, 0, 0)
		}
		adapter, err := s.execRuntime(r.Context(), x)
		if err != nil {
			return err
		}
		err = adapter.DeleteExec(r.Context(), x.Runtime, x.Upstream)
		if err != nil && !errors.Is(err, rt.ErrNotFound) {
			return err
		}
		stream := s.streamFor(x)
		stream.Lost("native exec session terminated")
	}
	return s.execView(w, x, 0, 0)
}

func (s *Server) attachExec(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("exec")
	p, err := s.streamPrincipal(r, id)
	if err != nil {
		return err
	}
	x, err := s.authorizedExec(r.Context(), p, id, false)
	if err != nil {
		return err
	}
	stream := s.streamFor(x)
	initial, _ := stream.Read(0, 0)
	cursor := uint64(0)
	if r.URL.Query().Has("cursor") {
		cursor, err = strconv.ParseUint(r.URL.Query().Get("cursor"), 10, 63)
		if err != nil || cursor > initial.End {
			return registry.ErrInvalid
		}
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{terminalProtocol}})
	if err != nil {
		return nil
	}
	defer conn.CloseNow()
	conn.SetReadLimit(96 << 10)
	ctx, stop := context.WithCancel(r.Context())
	defer stop()
	var recheck <-chan time.Time
	if x.Bundle != "" {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		recheck = ticker.C
	}
	if err := wsjson.Write(ctx, conn, map[string]any{"type": "ready", "stdin_seq": initial.StdinApplied}); err != nil {
		return nil
	}
	go func() {
		defer stop()
		for {
			var frame terminalInput
			if err := wsjson.Read(ctx, conn, &frame); err != nil {
				return
			}
			if x.Bundle != "" && s.CheckAccessPrincipal(ctx, p, x.Machine) != nil {
				return
			}
			live, err := s.authorizedExec(ctx, p, id, true)
			if err == nil {
				switch frame.Type {
				case "stdin":
					_, err = s.applyExecInput(ctx, live, frame)
				case "resize":
					err = s.resizeExec(ctx, live, frame)
				default:
					err = registry.ErrInvalid
				}
			}
			if err != nil {
				if err := wsjson.Write(ctx, conn, map[string]any{"type": "error", "message": err.Error()}); err != nil {
					return
				}
			}
		}
	}()
	for {
		if x.Bundle != "" && s.CheckAccessPrincipal(ctx, p, x.Machine) != nil {
			return nil
		}
		view, changed := stream.Read(cursor, 64<<10)
		if view.Gap != nil {
			if err := wsjson.Write(ctx, conn, map[string]any{"type": "gap", "from": view.Gap.From, "to": view.Gap.To}); err != nil {
				return nil
			}
		}
		for _, chunk := range view.Output {
			writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := conn.Write(writeCtx, websocket.MessageBinary, chunk.Data)
			cancel()
			if err != nil {
				return nil
			}
		}
		cursor = view.Next
		if cursor < view.End {
			continue
		}
		if view.Done {
			if view.Error != "" {
				_ = wsjson.Write(ctx, conn, map[string]any{"type": "error", "message": view.Error})
			}
			_ = wsjson.Write(ctx, conn, map[string]any{"type": "exit", "exit_code": view.ExitCode, "state": view.State})
			_ = conn.Close(websocket.StatusNormalClosure, "")
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-changed:
		case <-recheck:
		}
	}
}

func checkAccessRevision(ctx context.Context, tx *sql.Tx, machine string, revision int64, expectedBinding string) (string, error) {
	var binding string
	var actual int64
	err := tx.QueryRowContext(ctx, "SELECT coalesce(current_binding_id,''),revision FROM machines WHERE id=? AND deleted_at IS NULL", machine).Scan(&binding, &actual)
	if err != nil {
		return "", err
	}
	if binding == "" {
		return "", registry.ErrNotFound
	}
	if revision != 0 && revision != actual {
		return "", registry.ErrRevision
	}
	if expectedBinding != "" && binding != expectedBinding {
		return "", registry.ErrGeneration
	}
	return binding, nil
}
