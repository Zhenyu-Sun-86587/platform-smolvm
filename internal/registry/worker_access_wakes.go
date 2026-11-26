package registry

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"sandbox-platform/internal/placement"
	rt "sandbox-platform/internal/runtime"
)

// AccessWakeCheck binds a known protection demand to its original execution
// and to a native operation ID reported by the Worker's protection authority.
type AccessWakeCheck struct {
	WorkerID          string
	RuntimeInstanceID string
	BundleID          string
	HoldID            string
	DemandID          string
	OperationID       string
	ControlEpoch      string
	InstanceID        string
	ExecutionEpoch    int64
	Kind              string
	CaptureID         string
}

// BeginAccessWake records a possible write before RequestWake. Concurrent
// demands for the same hibernated execution share one compute reservation.
func (s *Store) BeginAccessWake(ctx context.Context, demand string, b Binding, expectedEpoch int64, kind string) (bool, error) {
	if demand == "" || expectedEpoch < 0 || b.RuntimeInstanceObjectID == "" ||
		!oneOf(kind, "launch", rt.VMRestore) {
		return false, ErrInvalid
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	worker := b.BundleRoute().Owner()
	control, err := readControl(ctx, tx)
	if err != nil {
		return false, err
	}
	if control.Maintenance {
		return false, ErrMaintenance
	}
	if err := checkWorkerEntrance(ctx, tx, worker, "access"); err != nil {
		return false, err
	}
	managed, err := placementWorkerManaged(ctx, tx, worker)
	if err != nil || !managed {
		return false, err
	}
	var found string
	err = tx.QueryRowContext(ctx, `SELECT d.hold_id FROM worker_access_wake_demands d
		JOIN worker_access_wake_holds h ON h.id=d.hold_id
		WHERE d.demand_id=? AND h.binding_id=? AND h.control_epoch=?`, demand, b.ID, control.ControlEpoch).Scan(&found)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	m, err := readMachine(ctx, tx, b.MachineID)
	if err != nil {
		return false, err
	}
	current := m.CurrentBinding
	if m.DeletedAt != nil || current == nil || current.ID != b.ID || current.ExecutionEpoch != expectedEpoch ||
		current.RuntimeInstanceObjectID != b.RuntimeInstanceObjectID || current.LastCaptureID != b.LastCaptureID {
		return false, ErrGeneration
	}
	capture := ""
	if kind == rt.VMRestore {
		capture = b.LastCaptureID
		if capture == "" {
			return false, ErrInvalid
		}
	}
	err = tx.QueryRowContext(ctx, `SELECT id FROM worker_access_wake_holds
		WHERE binding_id=? AND execution_epoch=? AND native_kind=? AND capture_id=?`,
		b.ID, expectedEpoch, kind, capture).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		capability := rt.SystemBootCapability
		if kind == rt.VMRestore {
			capability = rt.FullStateOperationCapability(rt.VMRestore)
		}
		req, err := ownerPlacement(m.Spec, b.BundleRoute(), worker, capability, 1, false)
		if err != nil {
			return false, err
		}
		// The root and StatePoint already reside on this pinned route.
		req.Resources.StorageBytes = 0
		route, err := selectPlacement(ctx, tx, control.ControlEpoch, rt.Wake, *req, time.Now())
		if err != nil {
			return false, err
		}
		found = demand
		if _, err := tx.ExecContext(ctx, `INSERT INTO worker_access_wake_holds
			(id,binding_id,worker_id,runtime_instance_id,bundle_id,control_epoch,instance_id,execution_epoch,capture_id,native_kind,
			 vcpus,memory_mib,phase) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,'unknown')`, found, b.ID, worker, route.InstanceID, route.BundleID,
			control.ControlEpoch, b.RuntimeInstanceObjectID, expectedEpoch, capture, kind, req.Resources.VCPUs, req.Resources.MemoryMiB); err != nil {
			return false, err
		}
	} else if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO worker_access_wake_demands VALUES(?,?)`, demand, found); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// ObserveAccessWakeDecision links only the original demand to the broker's
// decision ID. It commits no admission and never releases compute.
func (s *Store) ObserveAccessWakeDecision(ctx context.Context, demand, operation string) error {
	if demand == "" || operation == "" {
		return ErrInvalid
	}
	_, err := s.writer.ExecContext(ctx, `INSERT OR IGNORE INTO worker_access_wake_candidates(demand_id,native_operation_id)
		SELECT demand_id,? FROM worker_access_wake_demands WHERE demand_id=?`, operation, demand)
	return err
}

const accessWakeCheckColumns = `h.worker_id,h.runtime_instance_id,h.bundle_id,h.id,d.demand_id,c.native_operation_id,
	h.control_epoch,h.instance_id,h.execution_epoch,h.native_kind,h.capture_id`

func accessWakeCheckQuery(where string) string {
	return `SELECT ` + accessWakeCheckColumns + ` FROM worker_access_wake_holds h
	JOIN worker_access_wake_demands d ON d.hold_id=h.id
	JOIN worker_access_wake_candidates c ON c.demand_id=d.demand_id WHERE ` + where
}

func scanAccessWakeCheck(row scanner) (AccessWakeCheck, error) {
	var out AccessWakeCheck
	err := row.Scan(&out.WorkerID, &out.RuntimeInstanceID, &out.BundleID, &out.HoldID, &out.DemandID, &out.OperationID,
		&out.ControlEpoch, &out.InstanceID, &out.ExecutionEpoch, &out.Kind, &out.CaptureID)
	return out, notFound(err)
}

func (s *Store) ListAccessWakeChecks(ctx context.Context, worker string) ([]AccessWakeCheck, error) {
	rows, err := s.reader.QueryContext(ctx, accessWakeCheckQuery(`h.worker_id=? AND h.phase='unknown'
		AND (h.native_operation_id IS NULL OR h.native_operation_id=c.native_operation_id)
		ORDER BY h.id,d.demand_id,c.native_operation_id`), worker)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccessWakeCheck
	for rows.Next() {
		r, err := scanAccessWakeCheck(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func validAccessWakeReceipt(check AccessWakeCheck, r rt.OperationReceipt) bool {
	digest, err := hex.DecodeString(strings.TrimPrefix(r.RequestDigest, "sha256:"))
	if err != nil || len(digest) != 32 || !strings.HasPrefix(r.RequestDigest, "sha256:") ||
		r.OperationID != check.OperationID || r.Kind != check.Kind || r.InstanceID != check.InstanceID || r.RouteID != "deep" ||
		r.BundleDigest != check.BundleID || r.ExpectedExecutionEpoch != check.ExecutionEpoch || r.Revision <= 0 ||
		r.Phase != "COMPLETED" || r.Execution == nil || r.Execution.InstanceID != check.InstanceID ||
		r.Execution.ExecutionEpoch <= check.ExecutionEpoch {
		return false
	}
	if check.Kind == rt.VMRestore {
		return r.StatePoint != nil && r.StatePoint.CaptureID == check.CaptureID &&
			r.StatePoint.ActivationDecision == "commit" && r.StatePoint.Stage == "released"
	}
	return true
}

// RecordAccessWakeAdmission accepts only the actual resource-admitted launch
// or Restore operation. The covering budget must be read after verifying the
// receipt and belong to the latest same-session budget query.
func (s *Store) RecordAccessWakeAdmission(ctx context.Context, check AccessWakeCheck, receipt rt.OperationReceipt,
	sample placement.Sample, confirmedThrough int64) error {
	if !validAccessWakeReceipt(check, receipt) || confirmedThrough < 0 || sample.Sequence <= confirmedThrough {
		return fmt.Errorf("%w: wake proof lacks matching admitted Restore or launch", ErrInvalid)
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stored, err := scanAccessWakeCheck(tx.QueryRowContext(ctx, accessWakeCheckQuery(`h.id=? AND d.demand_id=? AND c.native_operation_id=?`),
		check.HoldID, check.DemandID, check.OperationID))
	if err != nil {
		return err
	}
	if check != stored || sample.WorkerID != check.WorkerID {
		return ErrConflict
	}
	latest, err := readWorkerSample(ctx, tx, check.WorkerID)
	if err != nil {
		return err
	}
	if latest.ControlEpoch != sample.ControlEpoch || latest.EnrollmentGeneration != sample.EnrollmentGeneration ||
		latest.Session != sample.Session || latest.Sequence < sample.Sequence || time.Since(latest.oldestAt) > placement.DefaultSampleAge {
		return ErrStaleObservation
	}
	if check.ControlEpoch != sample.ControlEpoch {
		var rebound bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM worker_authority_recovery
			WHERE worker_id=? AND control_epoch=? AND rebound_enrollment_generation=?)`,
			check.WorkerID, sample.ControlEpoch, sample.EnrollmentGeneration).Scan(&rebound); err != nil {
			return err
		}
		if !rebound {
			return ErrControlEpoch
		}
	}
	changed, err := tx.ExecContext(ctx, `UPDATE worker_access_wake_holds SET phase='covered',native_operation_id=?,native_digest=?,covered_sequence=?
		WHERE id=? AND (native_operation_id IS NULL OR native_operation_id=?) AND (native_digest IS NULL OR native_digest=?)`,
		receipt.OperationID, receipt.RequestDigest, sample.Sequence, check.HoldID, receipt.OperationID, receipt.RequestDigest)
	if err != nil {
		return err
	}
	if count, err := changed.RowsAffected(); err != nil || count != 1 {
		return ErrOperationState
	}
	return tx.Commit()
}

func (s *Store) HasAccessWakeDemand(ctx context.Context, demand string) (bool, error) {
	var known bool
	err := s.reader.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM worker_access_wake_demands WHERE demand_id=?)`, demand).Scan(&known)
	return known, err
}
