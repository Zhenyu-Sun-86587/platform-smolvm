package platformapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
	"sandbox-platform/internal/runtime/workerclient"
)

type leaseRequest struct {
	Reason  string `json:"reason,omitempty"`
	TTL     int64  `json:"ttl_seconds,omitempty"`
	Token   string `json:"lease_token,omitempty"`
	Version int    `json:"semantics_version,omitempty"`
}
type leaseWork struct {
	ID                string `json:"intent_id"`
	Lease             string `json:"lease_id"`
	Method            string
	Reason            string
	TTL               int64
	Semantics         int
	ExpectedRevision  int64
	WorkerRevision    int64
	BindingGeneration int64
	Migrated          bool
	AwaitRunning      bool       `json:"await_running,omitempty"`
	ActorScope        auth.Scope `json:"actor_scope,omitempty"`
	AcceptedPower     string     `json:"accepted_power,omitempty"`
}

func (s *Server) workerLease(w http.ResponseWriter, r *http.Request, p auth.Principal, b registry.Binding, client workerclient.Client, id string, revision int64, v leaseRequest) error {
	if v.Version < 0 || v.Version > 2 {
		return registry.ErrInvalid
	}
	if _, err := s.Engine.ApplyMachinePolicy(r.Context(), b, client); err != nil {
		return err
	}
	raw, err := s.applyMutation(r, p, v, func(ctx context.Context, tx *sql.Tx) (registry.MutationResult, error) {
		task := leaseWork{Lease: id, Reason: v.Reason, TTL: v.TTL, BindingGeneration: b.Generation, Semantics: v.Version, ExpectedRevision: revision}
		var err error
		task.ID, err = registry.NewID("lease-intent-")
		if err != nil {
			return registry.MutationResult{}, err
		}
		token := v.Token
		var generation, rev int64
		var locked bool
		err = tx.QueryRowContext(ctx, "SELECT binding_generation,revision,EXISTS(SELECT 1 FROM operation_locks WHERE machine_id=machines.id) FROM machines WHERE id=? AND current_binding_id=? AND deleted_at IS NULL", b.MachineID, b.ID).Scan(&generation, &rev, &locked)
		if err != nil {
			return registry.MutationResult{}, err
		}
		if generation != b.Generation || id != "" && (b.IncarnationID == "" || b.ExecutionEpoch < 1) {
			return registry.MutationResult{}, registry.ErrGeneration
		}
		if id == "" {
			if revision != rev {
				return registry.MutationResult{}, registry.ErrRevision
			}
			if locked {
				return registry.MutationResult{}, registry.ErrOperationActive
			}
			if task.Reason == "" || len(task.Reason) > 200 {
				return registry.MutationResult{}, registry.ErrInvalid
			}
			task.Method = "AcquireRunningLease"
			task.AwaitRunning, task.ActorScope, task.AcceptedPower = true, p.Scope, string(b.PowerState)
			if task.Semantics == 0 {
				task.Semantics = 1
			}
			task.Lease, err = registry.NewID("lease_")
			if err != nil {
				return registry.MutationResult{}, err
			}
			var bytes [32]byte
			if _, err = rand.Read(bytes[:]); err != nil {
				return registry.MutationResult{}, err
			}
			token = base64.RawURLEncoding.EncodeToString(bytes[:])
		} else {
			task.Method = "RenewRunningLease"
			if r.Method == "DELETE" {
				task.Method = "ReleaseRunningLease"
			}
			var stored, state, owner string
			var epoch, workerRevision sql.NullInt64
			var leaseRevision int64
			err = tx.QueryRowContext(ctx, "SELECT principal_id,token_hash,revision,state,semantics_version,worker_revision,execution_epoch,reason FROM leases WHERE id=? AND machine_id=? AND binding_generation=?", id, b.MachineID, b.Generation).
				Scan(&owner, &stored, &leaseRevision, &state, &task.Semantics, &workerRevision, &epoch, &task.Reason)
			if err != nil {
				return registry.MutationResult{}, err
			}
			hash := sha256.Sum256([]byte(token))
			if owner != p.ID || subtle.ConstantTimeCompare([]byte(stored), []byte(hex.EncodeToString(hash[:]))) != 1 {
				return registry.MutationResult{}, auth.ErrForbidden
			}
			if revision != leaseRevision || state == "released" || v.Version != 0 && v.Version != task.Semantics {
				return registry.MutationResult{}, registry.ErrRevision
			}
			if epoch.Valid && epoch.Int64 != b.ExecutionEpoch {
				return registry.MutationResult{}, registry.ErrGeneration
			}
			task.WorkerRevision = workerRevision.Int64
			task.Migrated = !workerRevision.Valid
		}
		key, err := s.taskEnvCipher(true)
		if err != nil {
			return registry.MutationResult{}, err
		}
		nonce := make([]byte, key.NonceSize())
		if _, err = rand.Read(nonce); err != nil {
			return registry.MutationResult{}, err
		}
		sealed := key.Seal(nonce, nonce, []byte(token), []byte("lease/"+task.ID))
		_, err = tx.ExecContext(ctx, `INSERT INTO lifecycle_lease_intents(id,lease_id,machine_id,binding_id,method,principal,credential,scope_json,
            request_json,secret,lease_revision,created_at)VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, task.ID, task.Lease, b.MachineID, b.ID, task.Method, p.ID, p.CredentialID,
			string(lifecycle.JSON(lifecycle.WorkerExecution(b))), string(lifecycle.JSON(task)), sealed, revision, time.Now().UnixMilli())
		return registry.MutationResult{Response: lifecycle.JSON(map[string]string{"intent_id": task.ID})}, err
	})
	if err != nil {
		return err
	}
	var ref struct {
		ID string `json:"intent_id"`
	}
	if err = json.Unmarshal(raw, &ref); err != nil {
		return err
	}
	lease, err := s.deliverWorkerLease(r.Context(), client, ref.ID, true)
	if err != nil {
		return err
	}
	return write(w, 200, lease)
}

func (s *Server) deliverWorkerLease(ctx context.Context, client workerclient.Client, id string, disclose bool) (Lease, error) {
	var l Lease
	var machine, bindingID, principal, credential, state, raw, scopeRaw string
	var rejected *string
	var secret []byte
	if err := s.DB.ReadDB().QueryRowContext(ctx, "SELECT machine_id,binding_id,principal,credential,state,request_json,scope_json,secret,receipt_json FROM lifecycle_lease_intents WHERE id=?", id).
		Scan(&machine, &bindingID, &principal, &credential, &state, &raw, &scopeRaw, &secret, &rejected); err != nil {
		return l, err
	}
	if state == "rejected" && rejected != nil {
		var fault workerclient.Fault
		if err := json.Unmarshal([]byte(*rejected), &fault); err != nil {
			return l, err
		}
		return l, leaseFault(&fault)
	}
	var task leaseWork
	var scope workerclient.Execution
	if err := json.Unmarshal([]byte(raw), &task); err != nil {
		return l, err
	}
	if err := json.Unmarshal([]byte(scopeRaw), &scope); err != nil {
		return l, err
	}
	if state == "pending" && task.AwaitRunning {
		actor := auth.Principal{ID: principal, CredentialID: credential, Scope: task.ActorScope}
		checkBinding := func(ctx context.Context) error {
			m, err := s.DB.GetMachine(ctx, machine)
			if err != nil {
				return err
			}
			if m.DeletedAt != nil || m.CurrentBinding == nil || m.CurrentBinding.ID != bindingID ||
				m.BindingGeneration != task.BindingGeneration || m.CurrentBinding.RuntimeInstanceObjectID != scope.InstanceID {
				return registry.ErrGeneration
			}
			if m.CurrentBinding.PowerState == registry.PowerStopped && task.AcceptedPower != string(registry.PowerStopped) {
				return registry.ErrGeneration
			}
			return actor.Authorize(auth.ActionManage, machine)
		}
		var b registry.Binding
		var release func()
		err := checkBinding(ctx)
		if err == nil {
			b, release, err = s.Engine.AccessAt(ctx, actor, machine,
				lifecycle.AccessEndpoint{Trigger: "api_access", Revision: 1, Reauthorize: checkBinding})
		}
		if err != nil {
			var fault *workerclient.Fault
			var failure *lifecycle.Failure
			switch {
			case errors.Is(err, auth.ErrForbidden), errors.Is(err, auth.ErrUnauthenticated):
				fault = &workerclient.Fault{Code: "E_PERMISSION_DENIED", Message: "lease authorization changed during wake"}
			case errors.Is(err, registry.ErrGeneration), errors.Is(err, registry.ErrNotFound):
				fault = &workerclient.Fault{Code: "E_STALE_EXECUTION", Message: "lease target changed during wake"}
			case errors.As(err, &failure) && failure.Code == "SERVICE_NOT_READY":
				fault = &workerclient.Fault{Code: failure.Code, Message: failure.Message}
			case !errors.As(err, &fault) || workerclient.RetryDelivery(err):
				return l, err
			}
			result, saveErr := s.DB.DB().ExecContext(ctx, "UPDATE lifecycle_lease_intents SET state='rejected',receipt_json=?,secret=x'' WHERE id=? AND state='pending' AND request_json=?", string(lifecycle.JSON(fault)), id, raw)
			if saveErr != nil {
				return l, saveErr
			}
			if count, err := result.RowsAffected(); err != nil {
				return l, err
			} else if count == 0 {
				return s.deliverWorkerLease(ctx, client, id, disclose)
			}
			return l, leaseFault(fault)
		}
		defer release()
		task.AwaitRunning = false
		scope = lifecycle.WorkerExecution(b)
		result, err := s.DB.DB().ExecContext(ctx, `UPDATE lifecycle_lease_intents SET request_json=?,scope_json=?
			WHERE id=? AND state='pending' AND request_json=? AND binding_id=(SELECT current_binding_id FROM machines WHERE id=?)`,
			string(lifecycle.JSON(task)), string(lifecycle.JSON(scope)), id, raw, machine)
		if err != nil {
			return l, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return l, err
		}
		if n == 0 {
			var saved string
			if err = s.DB.ReadDB().QueryRowContext(ctx, "SELECT scope_json FROM lifecycle_lease_intents WHERE id=? AND binding_id=? AND json_extract(request_json,'$.await_running') IS NULL", id, bindingID).Scan(&saved); err != nil {
				return l, err
			}
			var committed workerclient.Execution
			if err = json.Unmarshal([]byte(saved), &committed); err != nil {
				return l, err
			}
			if committed != scope {
				return l, registry.ErrGeneration
			}
		}
	}
	key, err := s.taskEnvCipher(false)
	if err != nil {
		return l, err
	}
	var token []byte
	if len(secret) > 0 {
		if len(secret) < key.NonceSize() {
			return l, registry.ErrInvalid
		}
		token, err = key.Open(nil, secret[:key.NonceSize()], secret[key.NonceSize():], []byte("lease/"+id))
		if err != nil {
			return l, err
		}
		defer clear(token)
	}
	if state == "pending" {
		var ack struct {
			State    string `json:"state"`
			Revision int64  `json:"revision"`
			Expires  int64  `json:"expires_at_unix_ms"`
		}
		method := task.Method
		if task.Migrated && method == "RenewRunningLease" {
			method = "AcquireRunningLease"
		}
		if task.Migrated && method == "ReleaseRunningLease" {
			ack.State = "released"
			ack.Revision = 0
		} else {
			payload := map[string]any{"protection_id": task.Lease, "principal": principal, "token": string(token), "expected_revision": task.WorkerRevision}
			if method == "AcquireRunningLease" {
				payload["semantics_version"] = task.Semantics
				payload["reason"] = task.Reason
			}
			if method != "ReleaseRunningLease" {
				payload["ttl_seconds"] = task.TTL
			}
			if err = client.Lifecycle(ctx, method, id, scope, payload, &ack); err != nil {
				var fault *workerclient.Fault
				if errors.As(err, &fault) && !workerclient.RetryDelivery(err) {
					_, saveErr := s.DB.DB().ExecContext(ctx, "UPDATE lifecycle_lease_intents SET state='rejected',receipt_json=?,secret=x'' WHERE id=? AND state='pending'", string(lifecycle.JSON(fault)), id)
					if saveErr != nil {
						return l, saveErr
					}
					return l, leaseFault(fault)
				}
				return l, err
			}
		}
		if ack.State != "released" && ack.State != "active" {
			return l, registry.ErrConflict
		}
		expires := ack.Expires
		if expires == 0 {
			expires = time.Now().Add(time.Duration(task.TTL) * time.Second).UnixMilli()
		}
		tx, err := s.DB.DB().BeginTx(ctx, nil)
		if err != nil {
			return l, err
		}
		defer tx.Rollback()
		res, err := tx.ExecContext(ctx, "UPDATE lifecycle_lease_intents SET state='applied',receipt_json=? WHERE id=? AND state='pending'", string(lifecycle.JSON(ack)), id)
		if err != nil {
			return l, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return l, err
		}
		if n > 0 {
			if task.Method == "AcquireRunningLease" {
				digest := sha256.Sum256(token)
				_, err = tx.ExecContext(ctx, `INSERT INTO leases(id,machine_id,binding_generation,principal_id,token_hash,reason,expires_at,state,semantics_version,
                    execution_epoch,incarnation_id,worker_revision)VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, task.Lease, machine, task.BindingGeneration, principal, hex.EncodeToString(digest[:]),
					task.Reason, expires, ack.State, task.Semantics, scope.Epoch, scope.IncarnationID, ack.Revision)
			} else {
				var result sql.Result
				result, err = tx.ExecContext(ctx, `UPDATE leases SET state=?,expires_at=CASE WHEN ?='released' THEN expires_at ELSE ? END,revision=revision+1,
                    worker_revision=?,execution_epoch=?,incarnation_id=? WHERE id=? AND revision=?`, ack.State, ack.State, expires, ack.Revision, scope.Epoch, scope.IncarnationID, task.Lease, task.ExpectedRevision)
				if err == nil {
					var count int64
					count, err = result.RowsAffected()
					if err == nil && count != 1 {
						return l, registry.ErrRevision
					}
				}
			}
			if err != nil {
				return l, err
			}
			if err = registry.AppendEvent(ctx, tx, "lease", machine, "", map[string]string{"lease_id": task.Lease, "state": ack.State}, false); err != nil {
				return l, err
			}
		}
		if err = tx.Commit(); err != nil {
			return l, err
		}
	}
	l, err = scanLease(s.DB.ReadDB().QueryRowContext(ctx, "SELECT "+leaseCols+" FROM leases WHERE id=?", task.Lease))
	if err != nil {
		return l, err
	}
	if disclose && task.Method == "AcquireRunningLease" {
		res, err := s.DB.DB().ExecContext(ctx, "UPDATE lifecycle_lease_intents SET token_delivered=1,secret=x'' WHERE id=? AND token_delivered=0", id)
		if err != nil {
			return l, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return l, err
		}
		if n == 1 {
			l.Token = string(token)
		}
	} else if task.Method != "AcquireRunningLease" {
		_, err = s.DB.DB().ExecContext(ctx, "UPDATE lifecycle_lease_intents SET secret=x'' WHERE id=? AND length(secret)>0", id)
	}
	return l, err
}

func (s *Server) recoverWorkerLeases(ctx context.Context, b registry.Binding, client workerclient.Client) {
	rows, err := s.DB.ReadDB().QueryContext(ctx, "SELECT id FROM lifecycle_lease_intents WHERE binding_id=? AND state='pending' ORDER BY created_at", b.ID)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) != nil {
			rows.Close()
			return
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		_, _ = s.deliverWorkerLease(ctx, client, id, false)
	}
	rows, err = s.DB.ReadDB().QueryContext(ctx, "SELECT id,principal_id,revision,worker_revision FROM leases WHERE machine_id=? AND worker_revision IS NOT NULL AND state!='released'", b.MachineID)
	if err != nil {
		return
	}
	type leaseRef struct {
		id, p       string
		rev, worker int64
	}
	var refs []leaseRef
	for rows.Next() {
		var r leaseRef
		if rows.Scan(&r.id, &r.p, &r.rev, &r.worker) != nil {
			rows.Close()
			return
		}
		refs = append(refs, r)
	}
	rows.Close()
	for _, r := range refs {
		var ack struct {
			State    string `json:"state"`
			Revision int64  `json:"revision"`
			Reason   string `json:"reason"`
		}
		err = client.Lifecycle(ctx, "QueryProtection", "", lifecycle.WorkerExecution(b), map[string]any{"protection_id": r.id, "principal": r.p}, &ack)
		if err == nil && ack.Revision > r.worker && (ack.State == "released" || ack.State == "suspect") {
			_, _ = s.DB.DB().ExecContext(ctx, "UPDATE leases SET state=?,release_reason=?,revision=revision+1,worker_revision=? WHERE id=? AND revision=? AND worker_revision=?", ack.State, ack.Reason, ack.Revision, r.id, r.rev, r.worker)
		}
	}
}

func leaseFault(f *workerclient.Fault) error {
	switch f.Code {
	case "E_STALE_REVISION":
		return registry.ErrRevision
	case "E_STALE_EXECUTION":
		return registry.ErrGeneration
	case "E_IDEMPOTENCY_CONFLICT":
		return registry.ErrIdempotency
	case "E_PERMISSION_DENIED":
		return auth.ErrForbidden
	case "E_NOT_FOUND":
		return registry.ErrNotFound
	case "E_INVALID_REQUEST", "E_INVALID_ARGUMENT":
		return registry.ErrInvalid
	default:
		return &lifecycle.Failure{Code: f.Code, Message: f.Message}
	}
}
