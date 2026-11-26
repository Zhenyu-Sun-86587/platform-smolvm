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

type desktopLeasePlan struct {
	ServiceID       string              `json:"service_id"`
	ServiceRevision int64               `json:"service_revision"`
	Generation      int64               `json:"binding_generation"`
	ControlEpoch    string              `json:"control_epoch"`
	ExpiresAt       int64               `json:"expires_at_ms"`
	TTLSeconds      int64               `json:"ttl_seconds"`
	AcceptedPower   registry.PowerState `json:"accepted_power"`
	Upgrade         bool                `json:"upgrade,omitempty"`
}

type desktopLeaseIntent struct {
	id, lease, machine, binding, owner, credential, state string
	scope                                                 workerclient.Execution
	plan                                                  desktopLeasePlan
	secret                                                []byte
}

func (s *Server) readDesktopIntent(ctx context.Context, id string) (desktopLeaseIntent, error) {
	var intent desktopLeaseIntent
	var scope, plan string
	intent.id = id
	err := s.DB.ReadDB().QueryRowContext(ctx, `SELECT lease_id,machine_id,binding_id,principal,credential,state,scope_json,request_json,secret
		FROM lifecycle_lease_intents WHERE id=? AND method=?`, id, desktopIntentMethod).Scan(
		&intent.lease, &intent.machine, &intent.binding, &intent.owner, &intent.credential, &intent.state, &scope, &plan, &intent.secret)
	if err == nil {
		err = json.Unmarshal([]byte(scope), &intent.scope)
	}
	if err == nil {
		err = json.Unmarshal([]byte(plan), &intent.plan)
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

func desktopEnded() error {
	return &lifecycle.Failure{Code: "DESKTOP_SESSION_ENDED", Message: "desktop session binding, authorization or lease changed"}
}

func desktopHasEnded(err error) bool {
	var ended *lifecycle.Failure
	return errors.As(err, &ended) && ended.Code == "DESKTOP_SESSION_ENDED"
}

func (s *Server) admitDesktopCreation(ctx context.Context, id string, p auth.Principal) (desktopSession, error) {
	unlock, err := s.lockLeaseDelivery(ctx, id)
	if err != nil {
		return desktopSession{}, err
	}
	_, err = s.desktopFor(ctx, id, p)
	if errors.Is(err, sql.ErrNoRows) {
		err = s.driveDesktopIntent(ctx, id, p)
	}
	unlock()
	if err != nil {
		return desktopSession{}, err
	}
	return s.admitDesktop(ctx, id, p)
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
		if session.workerRev.Valid {
			return session, rt.ErrUnavailable
		}
		return session, s.verifyDesktop(ctx, id, p)
	}
	if !session.workerRev.Valid {
		if err := s.upgradeDesktop(ctx, id, binding, p); err != nil {
			return session, err
		}
		session, err = s.desktopFor(ctx, id, p)
		if err != nil {
			return session, err
		}
	}
	ack, err := desktopProtectionStatus(ctx, client, lifecycle.WorkerExecution(binding), session.LeaseID, session.leaseOwner)
	if err != nil {
		return session, desktopWorkerError(err)
	}
	if !ack.matches(session.LeaseID, lifecycle.WorkerExecution(binding)) || ack.State != "active" ||
		!session.workerRev.Valid || ack.Revision != session.workerRev.Int64 || ack.Expires <= time.Now().UnixMilli() {
		return session, desktopEnded()
	}
	return session, s.verifyDesktop(ctx, id, p)
}

// Upgrade preserves the ID, deadline and execution of a legacy SQL-only
// desktop session. A session already published on a prior execution is fenced.
func (s *Server) upgradeDesktop(ctx context.Context, id string, binding registry.Binding, p auth.Principal) error {
	unlock, err := s.lockLeaseDelivery(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	session, err := s.desktopFor(ctx, id, p)
	if err != nil || session.workerRev.Valid {
		return err
	}
	if binding.PowerState == registry.PowerHibernated || binding.PowerState == registry.PowerStopped {
		return desktopEnded()
	}
	tx, err := s.DB.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := desktopValid(ctx, tx, id); err != nil {
		return err
	}
	var controlEpoch string
	if err := tx.QueryRowContext(ctx, "SELECT control_epoch FROM desktop_sessions WHERE id=?", id).Scan(&controlEpoch); err != nil {
		return err
	}
	ttl := int64((time.Until(session.Expires) + time.Second - 1) / time.Second)
	if err := s.createDesktopIntent(ctx, tx, session, binding, controlEpoch, ttl, true); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.driveDesktopIntent(ctx, id, p)
}

func desktopProtectionStatus(ctx context.Context, client workerclient.Client, scope workerclient.Execution, lease, principal string) (desktopProtection, error) {
	var ack desktopProtection
	err := client.Lifecycle(ctx, "QueryProtection", "", scope,
		map[string]string{"protection_id": lease, "principal": principal}, &ack)
	return ack, err
}

func desktopWorkerError(err error) error {
	var fault *workerclient.Fault
	if errors.As(err, &fault) && !workerclient.RetryDelivery(err) {
		return leaseFault(fault)
	}
	return errors.Join(rt.ErrUnavailable, errors.New("waiting for Worker desktop protection"))
}

func (s *Server) createDesktopIntent(ctx context.Context, tx *sql.Tx, session desktopSession, binding registry.Binding, controlEpoch string, ttl int64, upgrade bool) error {
	if ttl < 1 || binding.RuntimeInstanceObjectID == "" {
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
	plan := desktopLeasePlan{ServiceID: session.ServiceID, ServiceRevision: session.serviceRev, Generation: session.Generation,
		ControlEpoch: controlEpoch, ExpiresAt: session.Expires.UnixMilli(), TTLSeconds: ttl, AcceptedPower: binding.PowerState, Upgrade: upgrade}
	_, err = tx.ExecContext(ctx, `INSERT INTO lifecycle_lease_intents
		(id,lease_id,machine_id,binding_id,method,principal,credential,scope_json,request_json,secret,lease_revision,state,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, session.ID, session.LeaseID, session.MachineID, session.BindingID,
		desktopIntentMethod, session.leaseOwner, session.credential, string(lifecycle.JSON(lifecycle.WorkerExecution(binding))),
		string(lifecycle.JSON(plan)), secret, session.Revision, desktopPending, time.Now().UnixMilli())
	return err
}

func (s *Server) desktopToken(intent desktopLeaseIntent) ([]byte, error) {
	key, err := s.taskEnvCipher(false)
	if err != nil {
		return nil, err
	}
	if len(intent.secret) < key.NonceSize() {
		return nil, registry.ErrInvalid
	}
	return key.Open(nil, intent.secret[:key.NonceSize()], intent.secret[key.NonceSize():], []byte("desktop/"+intent.id))
}

// target validates the logical binding during wake, then the exact Worker
// execution once the lease request has been dispatched.
func (intent desktopLeaseIntent) target(ctx context.Context, db desktopReader) (registry.Binding, error) {
	b := registry.Binding{ID: intent.binding, MachineID: intent.machine, Generation: intent.plan.Generation}
	now := time.Now().UnixMilli()
	if intent.state == desktopClosed || intent.plan.ExpiresAt <= now {
		return b, desktopEnded()
	}
	err := db.QueryRowContext(ctx, `SELECT COALESCE(b.runtime_instance_id,''),COALESCE(b.runtime_bundle_id,''),
		COALESCE(b.runtime_instance_object_id,''),COALESCE(b.incarnation_id,''),COALESCE(b.execution_epoch,0),b.power_state
		FROM machines m JOIN runtime_bindings b ON b.id=m.current_binding_id
		JOIN services s ON s.machine_id=m.id JOIN meta ctl ON ctl.id=1
		JOIN credentials c ON c.id=? JOIN principals p ON p.id=c.principal_id
		WHERE m.id=? AND b.id=? AND b.generation=? AND m.deleted_at IS NULL
		AND s.id=? AND s.revision=? AND s.enabled=1 AND s.kind='vnc' AND s.public_host IS NULL AND s.public_port IS NULL
		AND c.principal_id=? AND c.revoked_at IS NULL AND (c.expires_at IS NULL OR c.expires_at>?) AND p.disabled=0
		AND ctl.control_epoch=? AND ctl.maintenance=0`, intent.credential, intent.machine, intent.binding, intent.plan.Generation,
		intent.plan.ServiceID, intent.plan.ServiceRevision, intent.owner, now, intent.plan.ControlEpoch).
		Scan(&b.RuntimeInstanceID, &b.RuntimeBundleID, &b.RuntimeInstanceObjectID, &b.IncarnationID, &b.ExecutionEpoch, &b.PowerState)
	if errors.Is(err, sql.ErrNoRows) {
		return b, desktopEnded()
	}
	if err != nil {
		return b, err
	}
	if b.RuntimeInstanceObjectID != intent.scope.InstanceID ||
		b.PowerState == registry.PowerStopped && intent.plan.AcceptedPower != registry.PowerStopped ||
		(intent.plan.Upgrade || intent.state != desktopPending) && lifecycle.WorkerExecution(b) != intent.scope {
		return b, desktopEnded()
	}
	if intent.plan.Upgrade || intent.state == desktopApplied {
		if err := desktopValid(ctx, db, intent.id); err != nil {
			return b, err
		}
	}
	return b, nil
}

func (s *Server) driveDesktopIntent(ctx context.Context, id string, p auth.Principal) error {
	intent, err := s.readDesktopIntent(ctx, id)
	if err != nil {
		return err
	}
	if intent.owner != p.ID || intent.credential != p.CredentialID {
		return auth.ErrForbidden
	}
	if err := p.Authorize(auth.ActionTunnel, intent.machine); err != nil {
		return err
	}
	binding, err := intent.target(ctx, s.DB.ReadDB())
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
	ready, release, err := s.AccessAt(ctx, p, intent.machine, lifecycle.AccessEndpoint{
		Trigger: "service_access", Revision: intent.plan.ServiceRevision, Reauthorize: func(ctx context.Context) error {
			_, err := intent.target(ctx, s.DB.ReadDB())
			return err
		},
	})
	if err != nil {
		var fault *workerclient.Fault
		if errors.As(err, &fault) {
			return desktopWorkerError(err)
		}
		return err
	}
	defer release()
	scope := lifecycle.WorkerExecution(ready)
	if ready.ID != intent.binding || ready.Generation != intent.plan.Generation || scope.IncarnationID == "" || scope.Epoch < 1 {
		return registry.ErrGeneration
	}
	if err := s.markDesktopSent(ctx, &intent, scope); err != nil {
		return err
	}
	token, err := s.desktopToken(intent)
	if err != nil {
		return err
	}
	defer clear(token)
	if err := client.Lifecycle(ctx, "AcquireRunningLease", id+"-acquire", intent.scope, map[string]any{
		"protection_id": intent.lease, "principal": intent.owner, "token": string(token),
		"expected_revision": 0, "semantics_version": 2, "reason": "desktop_session", "ttl_seconds": intent.plan.TTLSeconds,
	}, nil); err != nil {
		return desktopWorkerError(err)
	}
	ack, err := desktopProtectionStatus(ctx, client, intent.scope, intent.lease, intent.owner)
	if err != nil {
		return desktopWorkerError(err)
	}
	if !ack.matches(intent.lease, intent.scope) || ack.State != "active" || ack.Expires <= time.Now().UnixMilli() {
		return desktopEnded()
	}
	return s.publishDesktopProtection(ctx, intent, ack, token)
}

func (s *Server) markDesktopSent(ctx context.Context, intent *desktopLeaseIntent, scope workerclient.Execution) error {
	tx, err := s.DB.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	binding, err := intent.target(ctx, tx)
	if err != nil {
		return err
	}
	if lifecycle.WorkerExecution(binding) != scope || scope.InstanceID != intent.scope.InstanceID {
		return registry.ErrGeneration
	}
	if intent.state == desktopPending {
		if _, err = tx.ExecContext(ctx, "UPDATE lifecycle_lease_intents SET state=?,scope_json=? WHERE id=? AND state=?",
			desktopSent, string(lifecycle.JSON(scope)), intent.id, desktopPending); err != nil {
			return err
		}
		intent.state, intent.scope = desktopSent, scope
	}
	return tx.Commit()
}

func (s *Server) publishDesktopProtection(ctx context.Context, intent desktopLeaseIntent, ack desktopProtection, token []byte) error {
	tx, err := s.DB.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := intent.target(ctx, tx); err != nil {
		return err
	}
	expires := min(intent.plan.ExpiresAt, ack.Expires)
	hash := fmt.Sprintf("%x", sha256.Sum256(token))
	if intent.plan.Upgrade {
		_, err = tx.ExecContext(ctx, `UPDATE leases SET revision=revision+1,semantics_version=2,worker_revision=?,incarnation_id=?,
			execution_epoch=?,expires_at=?,token_hash=? WHERE id=? AND worker_revision IS NULL`,
			ack.Revision, intent.scope.IncarnationID, intent.scope.Epoch, expires, hash, intent.lease)
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO leases(id,machine_id,binding_generation,principal_id,token_hash,reason,expires_at,state,
			semantics_version,worker_revision,incarnation_id,execution_epoch) VALUES(?,?,?,?,?,'Desktop session',?,'active',2,?,?,?)`,
			intent.lease, intent.machine, intent.plan.Generation, intent.owner, hash, expires, ack.Revision, intent.scope.IncarnationID, intent.scope.Epoch)
		if err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO desktop_sessions VALUES(?,?,?,?,?,?,?,?,?)`, intent.id, intent.lease,
				intent.plan.ServiceID, intent.plan.ServiceRevision, intent.binding, intent.scope.Epoch, intent.credential,
				intent.plan.ControlEpoch, time.Now().UnixMilli())
		}
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE lifecycle_lease_intents SET state=?,receipt_json=? WHERE id=? AND state=?",
		desktopApplied, string(lifecycle.JSON(ack)), intent.id, desktopSent); err != nil {
		return err
	}
	if err := registry.AppendEvent(ctx, tx, "lease", intent.machine, "", map[string]string{"lease_id": intent.lease, "state": "active"}, false); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Server) releaseDesktopProtection(ctx context.Context, id string) error {
	unlock, err := s.lockLeaseDelivery(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	intent, err := s.readDesktopIntent(ctx, id)
	if errors.Is(err, sql.ErrNoRows) || err == nil && intent.state == desktopClosed {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := intent.target(ctx, s.DB.ReadDB()); !desktopHasEnded(err) {
		return err
	}
	retire := func(receipt *desktopProtection) error {
		_, err := s.DB.DB().ExecContext(ctx, "UPDATE lifecycle_lease_intents SET state=?,receipt_json=?,secret=x'' WHERE id=? AND state=?",
			desktopClosed, string(lifecycle.JSON(receipt)), id, intent.state)
		return err
	}
	if intent.state == desktopPending {
		return retire(nil)
	}
	binding, err := s.DB.GetBinding(ctx, intent.binding)
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
	ack, err := desktopProtectionStatus(ctx, client, intent.scope, intent.lease, intent.owner)
	var fault *workerclient.Fault
	if errors.As(err, &fault) && fault.Code == "E_NOT_FOUND" {
		// Delivery is serialized with the earlier acquire. The invalidated
		// target fences any subsequent acquire of this same lease.
		return retire(nil)
	}
	if err != nil {
		return desktopWorkerError(err)
	}
	if !ack.matches(intent.lease, intent.scope) {
		return registry.ErrGeneration
	}
	if ack.State != "released" {
		token, err := s.desktopToken(intent)
		if err != nil {
			return err
		}
		defer clear(token)
		if err := client.Lifecycle(ctx, "ReleaseRunningLease", fmt.Sprintf("%s-release-%d", id, ack.Revision), intent.scope,
			map[string]any{"protection_id": intent.lease, "principal": intent.owner, "token": string(token), "expected_revision": ack.Revision}, &ack); err != nil {
			if errors.As(err, &fault) && fault.Code == "E_STALE_EXECUTION" {
				return retire(nil)
			}
			return desktopWorkerError(err)
		}
		if !ack.matches(intent.lease, intent.scope) || ack.State != "released" {
			return registry.ErrConflict
		}
	}
	return retire(&ack)
}

func (s *Server) recoverDesktopLeases(ctx context.Context) {
	rows, err := s.DB.ReadDB().QueryContext(ctx, `SELECT id FROM lifecycle_lease_intents WHERE method=? AND state!=? ORDER BY created_at`,
		desktopIntentMethod, desktopClosed)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		intent, err := s.readDesktopIntent(ctx, id)
		if err != nil {
			continue
		}
		if _, err := intent.target(ctx, s.DB.ReadDB()); desktopHasEnded(err) {
			release, cancel := context.WithTimeout(ctx, 5*time.Second)
			_ = s.releaseDesktopProtection(release, id)
			cancel()
		}
	}
}
