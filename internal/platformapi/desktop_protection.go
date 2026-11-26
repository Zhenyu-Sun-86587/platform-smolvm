package platformapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/workerclient"
)

const desktopIntentMethod = "DesktopSessionLease"

const (
	desktopPending = "desktop_pending"
	desktopSent    = "desktop_sent"
	desktopApplied = "desktop_applied"
	desktopClosed  = "desktop_closed"
)

type desktopProtection struct {
	ID          string `json:"protection_id"`
	Instance    string `json:"instance_id"`
	Incarnation string `json:"incarnation_id"`
	Epoch       int64  `json:"execution_epoch"`
	Kind        string `json:"kind"`
	State       string `json:"state"`
	Semantics   int    `json:"semantics_version"`
	Revision    int64  `json:"revision"`
	Expires     int64  `json:"expires_at_unix_ms"`
}

func (ack desktopProtection) matches(lease string, scope workerclient.Execution) bool {
	return ack.ID == lease && ack.Instance == scope.InstanceID && ack.Incarnation == scope.IncarnationID &&
		ack.Epoch == scope.Epoch && ack.Kind == "lease" && ack.Semantics == 2 && ack.Revision > 0
}

type desktopLeaseIntent struct {
	state  string
	scope  workerclient.Execution
	ttl    int64
	secret []byte
}

func (s *Server) readDesktopIntent(ctx context.Context, session desktopSession) (desktopLeaseIntent, error) {
	var intent desktopLeaseIntent
	var raw string
	err := s.DB.ReadDB().QueryRowContext(ctx, `SELECT state,scope_json,CAST(request_json AS INTEGER),secret
		FROM lifecycle_lease_intents WHERE id=? AND method=? AND lease_id=? AND binding_id=? AND principal=? AND credential=?`,
		session.ID, desktopIntentMethod, session.LeaseID, session.BindingID, session.leaseOwner, session.credential).
		Scan(&intent.state, &raw, &intent.ttl, &intent.secret)
	if err == nil {
		err = json.Unmarshal([]byte(raw), &intent.scope)
	}
	return intent, err
}

func (s *Server) desktopWorker(binding registry.Binding) (workerclient.Client, bool, error) {
	adapter, err := s.ForBinding(binding)
	if err != nil {
		return workerclient.Client{}, false, err
	}
	client, managed := lifecycle.PolicyClient(adapter, binding)
	return client, managed, nil
}

func desktopExecutionRequired() error {
	return &lifecycle.Failure{Code: "DESKTOP_EXECUTION_REQUIRED", Message: "start the machine to create a desktop session on its current execution"}
}

func (s *Server) admitDesktop(ctx context.Context, id string, p auth.Principal) (desktopSession, error) {
	session, err := s.desktopFor(ctx, id, p)
	if err != nil {
		return session, err
	}
	if err := desktopValid(ctx, s.DB.ReadDB(), id); err != nil {
		return session, err
	}
	binding, err := s.DB.GetBinding(ctx, session.BindingID)
	if err != nil {
		return session, err
	}
	if binding.ExecutionEpoch != session.Epoch || binding.Generation != session.Generation {
		return session, registry.ErrGeneration
	}
	client, managed, err := s.desktopWorker(binding)
	if err != nil {
		return session, err
	}
	if !managed {
		if session.workerRev.Valid || session.State == "suspect" {
			return session, rt.ErrUnavailable
		}
		return session, s.verifyDesktop(ctx, id, p)
	}
	if !session.workerRev.Valid {
		if binding.PowerState == registry.PowerHibernated || binding.PowerState == registry.PowerStopped {
			return session, desktopExecutionRequired()
		}
		unlock, err := s.lockLeaseDelivery(ctx, session.ID)
		if err != nil {
			return session, err
		}
		defer unlock()
		session, err = s.desktopFor(ctx, id, p)
		if err != nil {
			return session, err
		}
		if !session.workerRev.Valid {
			if err := s.acquireDesktopProtection(ctx, session, binding, client, p); err != nil {
				return session, err
			}
			session, err = s.desktopFor(ctx, id, p)
			if err != nil {
				return session, err
			}
		}
	}
	if err := s.verifyDesktop(ctx, id, p); err != nil {
		return session, err
	}
	scope := lifecycle.WorkerExecution(binding)
	ack, err := desktopProtectionStatus(ctx, client, scope, session)
	if err != nil {
		return session, desktopWorkerError(err)
	}
	if !ack.matches(session.LeaseID, scope) || ack.State != "active" || ack.Revision != session.workerRev.Int64 || ack.Expires <= time.Now().UnixMilli() {
		return session, &lifecycle.Failure{Code: "DESKTOP_SESSION_ENDED", Message: "Worker desktop lease has ended or changed execution"}
	}
	return session, s.verifyDesktop(ctx, id, p)
}

func desktopProtectionStatus(ctx context.Context, client workerclient.Client, scope workerclient.Execution, session desktopSession) (desktopProtection, error) {
	var ack desktopProtection
	err := client.Lifecycle(ctx, "QueryProtection", "", scope,
		map[string]string{"protection_id": session.LeaseID, "principal": session.leaseOwner}, &ack)
	return ack, err
}

func desktopWorkerError(err error) error {
	var fault *workerclient.Fault
	if errors.As(err, &fault) && !workerclient.RetryDelivery(err) {
		return leaseFault(fault)
	}
	return errors.Join(rt.ErrUnavailable, errors.New("waiting for Worker desktop protection"))
}

func (s *Server) createDesktopIntent(ctx context.Context, session desktopSession, scope workerclient.Execution) error {
	ttl := int64((time.Until(session.Expires) + time.Second - 1) / time.Second)
	if ttl < 1 {
		return registry.ErrConflict
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	token := []byte(base64.RawURLEncoding.EncodeToString(random[:]))
	defer clear(token)
	key, err := s.taskEnvCipher(true)
	if err != nil {
		return err
	}
	nonce := make([]byte, key.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	secret := key.Seal(nonce, nonce, token, []byte("desktop/"+session.ID))
	_, err = s.DB.DB().ExecContext(ctx, `INSERT INTO lifecycle_lease_intents
		(id,lease_id,machine_id,binding_id,method,principal,credential,scope_json,request_json,secret,lease_revision,state,created_at)
		SELECT d.id,d.lease_id,l.machine_id,d.binding_id,?,l.principal_id,d.credential_id,?,?,?,?,?,?
		FROM desktop_sessions d JOIN leases l ON l.id=d.lease_id WHERE d.id=? AND l.state!='released'
		ON CONFLICT(id) DO NOTHING`, desktopIntentMethod, string(lifecycle.JSON(scope)), fmt.Sprint(ttl), secret,
		session.Revision, desktopPending, time.Now().UnixMilli(), session.ID)
	return err
}

func (s *Server) desktopToken(session desktopSession, intent desktopLeaseIntent) ([]byte, error) {
	key, err := s.taskEnvCipher(false)
	if err != nil {
		return nil, err
	}
	if len(intent.secret) < key.NonceSize() {
		return nil, registry.ErrInvalid
	}
	return key.Open(nil, intent.secret[:key.NonceSize()], intent.secret[key.NonceSize():], []byte("desktop/"+session.ID))
}

func (s *Server) acquireDesktopProtection(ctx context.Context, session desktopSession, binding registry.Binding, client workerclient.Client, p auth.Principal) error {
	scope := lifecycle.WorkerExecution(binding)
	if scope.IncarnationID == "" {
		return registry.ErrGeneration
	}
	intent, err := s.readDesktopIntent(ctx, session)
	if errors.Is(err, sql.ErrNoRows) {
		if err := s.createDesktopIntent(ctx, session, scope); err != nil {
			return err
		}
		intent, err = s.readDesktopIntent(ctx, session)
	}
	if err != nil {
		return err
	}
	if intent.scope != scope || intent.state == desktopClosed {
		return registry.ErrGeneration
	}
	reauthorize := func(ctx context.Context) error { return desktopValid(ctx, s.DB.ReadDB(), session.ID) }
	ready, release, err := s.AccessAt(ctx, p, session.MachineID, lifecycle.AccessEndpoint{
		Trigger: "service_access", Revision: session.serviceRev, Reauthorize: reauthorize,
	})
	if err != nil {
		return err
	}
	defer release()
	if ready.ID != binding.ID || ready.Generation != binding.Generation || lifecycle.WorkerExecution(ready) != scope {
		return registry.ErrGeneration
	}
	token, err := s.desktopToken(session, intent)
	if err != nil {
		return err
	}
	defer clear(token)
	if err := s.markDesktopSent(ctx, session.ID); err != nil {
		return err
	}
	if err := client.Lifecycle(ctx, "AcquireRunningLease", session.ID+"-acquire", scope, map[string]any{
		"protection_id": session.LeaseID, "principal": session.leaseOwner, "token": string(token),
		"expected_revision": 0, "semantics_version": 2, "reason": "desktop_session", "ttl_seconds": intent.ttl,
	}, nil); err != nil {
		return desktopWorkerError(err)
	}
	ack, err := desktopProtectionStatus(ctx, client, scope, session)
	if err != nil {
		return desktopWorkerError(err)
	}
	if !ack.matches(session.LeaseID, scope) || ack.State != "active" || ack.Expires <= time.Now().UnixMilli() {
		return registry.ErrConflict
	}
	return s.publishDesktopProtection(ctx, session, scope, ack, token)
}

func (s *Server) markDesktopSent(ctx context.Context, id string) error {
	tx, err := s.DB.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = desktopValid(ctx, tx, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE lifecycle_lease_intents SET state=? WHERE id=? AND state=?", desktopSent, id, desktopPending); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Server) publishDesktopProtection(ctx context.Context, session desktopSession, scope workerclient.Execution, ack desktopProtection, token []byte) error {
	tx, err := s.DB.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = desktopValid(ctx, tx, session.ID); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE leases SET state='active',revision=revision+1,semantics_version=2,worker_revision=?,incarnation_id=?,
		execution_epoch=?,expires_at=MIN(expires_at,?),token_hash=? WHERE id=? AND worker_revision IS NULL`,
		ack.Revision, scope.IncarnationID, scope.Epoch, ack.Expires, fmt.Sprintf("%x", sha256.Sum256(token)), session.LeaseID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 1 {
		if _, err = tx.ExecContext(ctx, "UPDATE lifecycle_lease_intents SET state=?,receipt_json=? WHERE id=? AND state=?", desktopApplied,
			string(lifecycle.JSON(ack)), session.ID, desktopSent); err != nil {
			return err
		}
		if err = registry.AppendEvent(ctx, tx, "lease", session.MachineID, "", map[string]string{"lease_id": session.LeaseID, "state": "active"}, false); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Server) releaseDesktopProtection(ctx context.Context, session desktopSession) error {
	unlock, err := s.lockLeaseDelivery(ctx, session.ID)
	if err != nil {
		return err
	}
	defer unlock()
	intent, err := s.readDesktopIntent(ctx, session)
	if errors.Is(err, sql.ErrNoRows) || err == nil && intent.state == desktopClosed {
		return nil
	}
	if err != nil {
		return err
	}
	retire := func(receipt *desktopProtection) error {
		_, err := s.DB.DB().ExecContext(ctx, "UPDATE lifecycle_lease_intents SET state=?,receipt_json=?,secret=x'' WHERE id=? AND state=?",
			desktopClosed, string(lifecycle.JSON(receipt)), session.ID, intent.state)
		return err
	}
	if intent.state == desktopPending {
		return retire(nil)
	}
	binding, err := s.DB.GetBinding(ctx, session.BindingID)
	if err != nil {
		return err
	}
	client, managed, err := s.desktopWorker(binding)
	if err != nil {
		return err
	}
	if !managed {
		return rt.ErrUnavailable
	}
	ack, err := desktopProtectionStatus(ctx, client, intent.scope, session)
	var fault *workerclient.Fault
	if errors.As(err, &fault) && fault.Code == "E_NOT_FOUND" {
		// Delivery is serialized with the earlier acquire and the released
		// registry row fences any subsequent acquire of this same lease.
		return retire(nil)
	}
	if err != nil {
		return desktopWorkerError(err)
	}
	if !ack.matches(session.LeaseID, intent.scope) {
		return registry.ErrGeneration
	}
	if ack.State != "released" {
		token, err := s.desktopToken(session, intent)
		if err != nil {
			return err
		}
		defer clear(token)
		if err := client.Lifecycle(ctx, "ReleaseRunningLease", fmt.Sprintf("%s-release-%d", session.ID, ack.Revision), intent.scope,
			map[string]any{"protection_id": session.LeaseID, "principal": session.leaseOwner, "token": string(token), "expected_revision": ack.Revision}, &ack); err != nil {
			if errors.As(err, &fault) && fault.Code == "E_STALE_EXECUTION" {
				return retire(nil)
			}
			return desktopWorkerError(err)
		}
		if !ack.matches(session.LeaseID, intent.scope) || ack.State != "released" {
			return registry.ErrConflict
		}
	}
	return retire(&ack)
}

func (s *Server) recoverDesktopLeases(ctx context.Context) {
	rows, err := s.DB.ReadDB().QueryContext(ctx, desktopSessions+` JOIN lifecycle_lease_intents i ON i.id=d.id
		WHERE l.state='released' AND i.method=? AND i.state!=? ORDER BY d.created_at`, desktopIntentMethod, desktopClosed)
	if err != nil {
		return
	}
	var sessions []desktopSession
	for rows.Next() {
		session, err := readDesktopSession(rows)
		if err == nil {
			sessions = append(sessions, session)
		}
	}
	rows.Close()
	for _, session := range sessions {
		if ctx.Err() != nil {
			return
		}
		release, cancel := context.WithTimeout(ctx, 5*time.Second)
		_ = s.releaseDesktopProtection(release, session)
		cancel()
	}
}
