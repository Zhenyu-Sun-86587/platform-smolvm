package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"sandbox-platform/internal/placement"
	rt "sandbox-platform/internal/runtime"
)

const reservationFields = `operation_id,attempt,native_attempt,worker_id,runtime_instance_id,bundle_id,control_epoch,request_digest,
	COALESCE(native_digest,''),vcpus,memory_mib,storage_bytes,heavy_slots,phase,updated_at`

func scanPlacementReservation(row scanner) (placement.Reservation, error) {
	var out placement.Reservation
	var updatedAt int64
	err := row.Scan(&out.OperationID, &out.Attempt, &out.NativeAttempt, &out.WorkerID, &out.RuntimeInstanceID, &out.BundleID, &out.ControlEpoch,
		&out.RequestDigest, &out.NativeDigest, &out.Resources.VCPUs, &out.Resources.MemoryMiB, &out.Resources.StorageBytes,
		&out.Resources.HeavySlots, &out.Phase, &updatedAt)
	out.UpdatedAt = time.UnixMilli(updatedAt).UTC()
	return out, notFound(err)
}

func readPlacementReservation(ctx context.Context, q queryer, operation string) (placement.Reservation, error) {
	return scanPlacementReservation(q.QueryRowContext(ctx, `SELECT `+reservationFields+
		` FROM placement_reservations WHERE operation_id=? ORDER BY attempt DESC LIMIT 1`, operation))
}

func (s *Store) GetPlacementReservation(ctx context.Context, operationID string) (placement.Reservation, error) {
	return readPlacementReservation(ctx, s.reader, operationID)
}

func (s *Store) ListPlacementRechecks(ctx context.Context, workerID string) ([]placement.Reservation, error) {
	rows, err := s.reader.QueryContext(ctx, `SELECT `+reservationFields+` FROM placement_reservations
		WHERE worker_id=? AND phase IN ('dispatch_started','unknown','admitted') ORDER BY operation_id,attempt`, workerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []placement.Reservation
	for rows.Next() {
		reservation, err := scanPlacementReservation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, reservation)
	}
	return out, rows.Err()
}

func setReservationPhase(ctx context.Context, tx *sql.Tx, r placement.Reservation, phase string) error {
	if r.Phase == phase {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE placement_reservations SET phase=?,updated_at=? WHERE operation_id=? AND attempt=?`,
		phase, time.Now().UnixMilli(), r.OperationID, r.Attempt)
	return err
}

func dispatchPlacementTx(ctx context.Context, tx *sql.Tx, r placement.Reservation, nativeDigest string) error {
	if nativeDigest == "" {
		return fmt.Errorf("%w: dispatch requires an immutable native request digest", ErrInvalid)
	}
	if r.NativeDigest != "" {
		if r.NativeDigest != nativeDigest {
			return ErrIdempotency
		}
		if oneOf(r.Phase, placement.DispatchStarted, placement.Unknown, placement.Admitted, placement.Covered) {
			return nil
		}
	}
	if r.Phase != placement.Reserved {
		return ErrOperationState
	}
	_, err := tx.ExecContext(ctx, `UPDATE placement_reservations SET phase='dispatch_started',native_digest=?,updated_at=?
		WHERE operation_id=? AND attempt=?`, nativeDigest, time.Now().UnixMilli(), r.OperationID, r.Attempt)
	return err
}

func (s *Store) BeginPlacementDispatch(ctx context.Context, operationID, nativeDigest string) error {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := checkWritable(ctx, tx); err != nil {
		return err
	}
	r, err := readPlacementReservation(ctx, tx, operationID)
	if err != nil {
		return err
	}
	if err := dispatchPlacementTx(ctx, tx, r, nativeDigest); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RecordPlacementUnknown(ctx context.Context, operationID, nativeDigest string) error {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := readPlacementReservation(ctx, tx, operationID)
	if err != nil {
		return err
	}
	if r.NativeDigest != nativeDigest || nativeDigest == "" ||
		!oneOf(r.Phase, placement.DispatchStarted, placement.Unknown, placement.Admitted, placement.Covered) {
		return ErrOperationState
	}
	// Observation can be lost after admission, while the already-established
	// budget coverage remains valid and need not be double counted.
	if r.Phase == placement.DispatchStarted {
		if err := setReservationPhase(ctx, tx, r, placement.Unknown); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RecordPlacementAdmission links a verified, durable broker admission to its
// exact intent. A subsequent on-demand sample must carry a strictly higher
// same-session sequence. WakeDemand acknowledgement is never admission.
func (s *Store) RecordPlacementAdmission(ctx context.Context, proof placement.Admission) error {
	if proof.ReceiptID == "" || proof.NativeDigest == "" || !proof.Session.Valid() ||
		proof.Attempt < 1 || proof.ConfirmedThroughSequence < 0 {
		return fmt.Errorf("%w: incomplete local admission evidence", ErrInvalid)
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := scanPlacementReservation(tx.QueryRowContext(ctx, `SELECT `+reservationFields+
		` FROM placement_reservations WHERE operation_id=? AND attempt=?`, proof.OperationID, proof.Attempt))
	if err != nil {
		return err
	}
	if r.WorkerID != proof.WorkerID || r.RuntimeInstanceID != proof.RuntimeInstanceID || r.BundleID != proof.BundleID ||
		r.NativeDigest != proof.NativeDigest {
		return fmt.Errorf("%w: proof refers to a different placement", ErrConflict)
	}
	var kind string
	if err := tx.QueryRowContext(ctx, "SELECT kind FROM operations WHERE id=?", r.OperationID).Scan(&kind); err != nil {
		return err
	}
	want := placement.AdmissionOperation
	if kind == rt.VMRestore || kind == rt.Wake && (r.Resources.VCPUs != 0 || r.Resources.MemoryMiB != 0 || r.Resources.StorageBytes != 0) {
		want = placement.AdmissionRestore
	}
	if proof.Kind != want {
		return fmt.Errorf("%w: local admission evidence lacks required receipt kind", ErrInvalid)
	}
	sample, err := readWorkerSample(ctx, tx, r.WorkerID)
	if err != nil {
		return err
	}
	control, err := readControl(ctx, tx)
	if err != nil {
		return err
	}
	if proof.ControlEpoch != control.ControlEpoch || sample.ControlEpoch != proof.ControlEpoch ||
		sample.EnrollmentGeneration != proof.EnrollmentGeneration || sample.Session != proof.Session {
		return fmt.Errorf("%w: admission evidence has an obsolete session", ErrStaleObservation)
	}
	if r.ControlEpoch != proof.ControlEpoch {
		var rebound bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM worker_authority_recovery
			WHERE worker_id=? AND control_epoch=? AND rebound_enrollment_generation=?)`,
			r.WorkerID, proof.ControlEpoch, proof.EnrollmentGeneration).Scan(&rebound); err != nil {
			return err
		}
		if !rebound {
			return ErrControlEpoch
		}
	}
	if r.Phase == placement.Covered {
		return nil
	}
	if !oneOf(r.Phase, placement.DispatchStarted, placement.Unknown, placement.Admitted) {
		return ErrOperationState
	}
	if _, err := tx.ExecContext(ctx, `UPDATE placement_reservations SET phase='admitted',admitted_generation=?,
		admitted_host_boot_id=?,admitted_broker_session_id=?,admitted_node_session_id=?,
		admitted_through_sequence=?,admitted_receipt_id=?,updated_at=? WHERE operation_id=? AND attempt=?`,
		proof.EnrollmentGeneration, proof.Session.HostBootID, proof.Session.BrokerSessionID, proof.Session.NodeSessionID,
		proof.ConfirmedThroughSequence, proof.ReceiptID, time.Now().UnixMilli(), r.OperationID, r.Attempt); err != nil {
		return err
	}
	if err := coverReservations(ctx, tx, sample.Sample); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ReleasePlacement(ctx context.Context, operationID string) error {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := readPlacementReservation(ctx, tx, operationID)
	if err != nil {
		return err
	}
	if r.Phase != placement.Reserved && r.Phase != placement.Released {
		return ErrOperationState
	}
	var active bool
	if err := tx.QueryRowContext(ctx, `SELECT state NOT IN ('failed','cancelled','succeeded') FROM operations WHERE id=?`,
		operationID).Scan(&active); err != nil {
		return err
	}
	if active {
		return ErrOperationState
	}
	if err := setReservationPhase(ctx, tx, r, placement.Released); err != nil {
		return err
	}
	return tx.Commit()
}

func placementDispatchDigest(raw json.RawMessage) (string, error) {
	type digest struct {
		RequestDigest string `json:"request_digest"`
	}
	var outgoing struct {
		Native, Launch, Volume, StatePoint *digest
		Fork                               *struct {
			Submission digest `json:"submission"`
		}
	}
	if err := json.Unmarshal(raw, &outgoing); err != nil {
		return "", err
	}
	for _, candidate := range []*digest{outgoing.Native, outgoing.Launch, outgoing.Volume, outgoing.StatePoint} {
		if candidate != nil {
			return candidate.RequestDigest, nil
		}
	}
	if outgoing.Fork != nil {
		return outgoing.Fork.Submission.RequestDigest, nil
	}
	return "", nil
}

func transitionPlacement(ctx context.Context, tx *sql.Tx, op Operation, change TransitionRequest) error {
	r, err := readPlacementReservation(ctx, tx, op.ID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if op.State == RecoveryRequired && r.Phase == placement.DispatchStarted {
		return setReservationPhase(ctx, tx, r, placement.Unknown)
	}
	if op.Terminal() {
		switch {
		case change.PlacementNotAdmitted && rt.IsNativeFork(op.Kind) && r.Attempt > 1 &&
			oneOf(r.Phase, placement.Reserved, placement.DispatchStarted, placement.Unknown):
			return setReservationPhase(ctx, tx, r, placement.Rejected)
		case op.Effect == EffectNone && r.Phase == placement.Reserved:
			return setReservationPhase(ctx, tx, r, placement.Released)
		case op.Effect == EffectNone && oneOf(r.Phase, placement.DispatchStarted, placement.Unknown):
			// The lifecycle transition must already prove a definite no-effect
			// result. A timeout and lookup miss stay recovery_required.
			return setReservationPhase(ctx, tx, r, placement.Rejected)
		case r.Phase == placement.Reserved && r.Resources == (placement.Resources{}):
			return setReservationPhase(ctx, tx, r, placement.Released)
		case op.Kind == rt.Wake && r.Resources.VCPUs == 0 && r.Resources.MemoryMiB == 0 && r.Resources.StorageBytes == 0 &&
			op.Effect == EffectApplied:
			// Confirmed standby wake preserves the existing execution and
			// has no extra admission resources to correlate after completion.
			return setReservationPhase(ctx, tx, r, placement.Released)
		case r.Phase == placement.DispatchStarted:
			return setReservationPhase(ctx, tx, r, placement.Unknown)
		}
		return nil
	}
	if r.Phase == placement.Reserved && len(change.Dispatch) != 0 &&
		(op.State == Dispatching || op.Stage == "worker_fork_retry") {
		digest, err := placementDispatchDigest(change.Dispatch)
		if err != nil || digest == "" {
			return err
		}
		return dispatchPlacementTx(ctx, tx, r, digest)
	}
	return nil
}
