package registry

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"sandbox-platform/internal/placement"
	rt "sandbox-platform/internal/runtime"
)

type placementCandidate struct {
	route      rt.BundleRoute
	profile    placement.RuntimeProfile
	registered bool
}

type workerPlacement struct {
	id, mode, workerStatus, maintenancePhase string
	routes                                   []placementCandidate
}

// placementWorkers reads registration and security facts from the acceptance
// transaction. Sample profiles can restrict these facts, never replace them.
func placementWorkers(ctx context.Context, tx *sql.Tx) ([]workerPlacement, error) {
	rows, err := tx.QueryContext(ctx, `SELECT w.id, w.desired_mode, w.observed_status, COALESCE(m.phase,''),
		COALESCE(i.id,''), COALESCE(i.local_instance_id,''), COALESCE(i.runtime_bundle_id,''),
		COALESCE(json_extract(b.manifest_json,'$.contracts.state_reader_id'),''),
		COALESCE(i.status='registered' AND b.security_status IN ('unknown','active','legacy-allowed','ready'),0)
		FROM workers w LEFT JOIN worker_maintenance m ON m.worker_id=w.id
		LEFT JOIN runtime_instances i ON i.worker_id=w.id
		LEFT JOIN runtime_bundles b ON b.id=i.runtime_bundle_id
		ORDER BY w.id, i.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var workers []workerPlacement
	for rows.Next() {
		var w workerPlacement
		var c placementCandidate
		if err := rows.Scan(&w.id, &w.mode, &w.workerStatus, &w.maintenancePhase, &c.route.InstanceID, &c.route.LocalInstanceID,
			&c.route.BundleID, &c.profile.Reader, &c.registered); err != nil {
			return nil, err
		}
		c.route.WorkerID = w.id
		if len(workers) == 0 || workers[len(workers)-1].id != w.id {
			workers = append(workers, w)
		}
		if c.route.InstanceID != "" {
			workers[len(workers)-1].routes = append(workers[len(workers)-1].routes, c)
		}
	}
	return workers, rows.Err()
}

func workerPlacementRejection(ctx context.Context, tx *sql.Tx, worker workerPlacement, epoch, kind string, req placement.Request, now time.Time) (workerSample, string, error) {
	switch {
	case req.Mode == placement.Pinned && req.WorkerID != worker.id:
		return workerSample{}, "different_worker", nil
	case req.OwnerWorkerID != "" && req.OwnerWorkerID != worker.id:
		return workerSample{}, "owner_mismatch", nil
	case worker.maintenancePhase == "activating" || !workerModeAllows(worker.mode, kind):
		return workerSample{}, "maintenance", nil
	case worker.workerStatus != "ready":
		return workerSample{}, "worker_not_ready", nil
	}
	generation, err := workerGeneration(ctx, tx, worker.id, epoch)
	if errors.Is(err, ErrNotFound) {
		return workerSample{}, "enrollment_missing", nil
	}
	if err != nil {
		return workerSample{}, "", err
	}
	sample, err := readWorkerSample(ctx, tx, worker.id)
	if errors.Is(err, ErrNotFound) {
		return workerSample{}, "budget_missing", nil
	}
	if err != nil {
		return workerSample{}, "", err
	}
	maxAge := req.MaxSampleAge
	if maxAge == 0 {
		maxAge = placement.DefaultSampleAge
	}
	if sample.ControlEpoch != epoch || sample.EnrollmentGeneration != generation ||
		now.Before(sample.oldestAt) || now.Sub(sample.oldestAt) > maxAge {
		return workerSample{}, "budget_stale", nil
	}
	if sample.Pressure == nil {
		return workerSample{}, "pressure_unknown", nil
	}
	if sample.Pressure.MemoryStalled || sample.Pressure.DiskStalled {
		return workerSample{}, "worker_pressure", nil
	}
	return sample, "", nil
}

func candidateRejection(req placement.Request, candidate placementCandidate, sample workerSample) (placement.RuntimeProfile, string) {
	route := candidate.route
	if req.RuntimeInstanceID != "" && req.RuntimeInstanceID != route.InstanceID ||
		req.EligibleRuntimeIDs != nil && !slices.Contains(req.EligibleRuntimeIDs, route.InstanceID) {
		return placement.RuntimeProfile{}, "runtime_mismatch"
	}
	if !candidate.registered {
		return placement.RuntimeProfile{}, "runtime_unavailable"
	}
	for _, profile := range sample.Profiles {
		if profile.RuntimeInstanceID != route.InstanceID {
			continue
		}
		if profile.LocalInstanceID != route.LocalInstanceID || profile.BundleID != route.BundleID {
			return placement.RuntimeProfile{}, "runtime_identity_mismatch"
		}
		if req.Reader != "" && req.Reader != candidate.profile.Reader {
			return placement.RuntimeProfile{}, "reader_incompatible"
		}
		return profile, req.ProfileRejection(profile)
	}
	return placement.RuntimeProfile{}, "runtime_unobserved"
}

func placementAvailability(ctx context.Context, tx *sql.Tx, worker string, sample workerSample, req placement.Request) (placement.Resources, string, error) {
	available := sample.Budget.Capacity.Subtract(sample.Budget.Allocated)
	// Physical pressure limits are independent of the admission budget, so a
	// host reporting free physical resources cannot enlarge that budget.
	available.MemoryMiB = min(available.MemoryMiB, sample.Pressure.AvailableMemoryMiB)
	available.StorageBytes = min(available.StorageBytes, sample.Pressure.DiskFreeBytes)
	rows, err := tx.QueryContext(ctx, `SELECT vcpus,memory_mib,storage_bytes,heavy_slots
		FROM placement_reservations WHERE worker_id=? AND phase IN ('reserved_not_sent','dispatch_started','unknown','admitted')
		UNION ALL SELECT vcpus,memory_mib,0,0 FROM worker_access_wake_holds WHERE worker_id=? AND phase='unknown'`, worker, worker)
	if err != nil {
		return available, "", err
	}
	defer rows.Close()
	for rows.Next() {
		var held placement.Resources
		if err := rows.Scan(&held.VCPUs, &held.MemoryMiB, &held.StorageBytes, &held.HeavySlots); err != nil {
			return available, "", err
		}
		available = available.Subtract(held)
	}
	if err := rows.Err(); err != nil {
		return available, "", err
	}
	if !req.Headroom.Fits(available) {
		return available, "budget_headroom", nil
	}
	available = available.Subtract(req.Headroom)
	if req.Resources.HeavySlots > available.HeavySlots {
		return available, "heavy_slots_busy", nil
	}
	if !req.Resources.Fits(available) {
		return available, "budget_insufficient", nil
	}
	return available, "", nil
}

func selectPlacement(ctx context.Context, tx *sql.Tx, controlEpoch, kind string, req placement.Request, now time.Time) (rt.BundleRoute, error) {
	if err := req.Validate(); err != nil {
		return rt.BundleRoute{}, fmt.Errorf("%w: %s", ErrInvalid, err)
	}
	workers, err := placementWorkers(ctx, tx)
	if err != nil {
		return rt.BundleRoute{}, err
	}
	rejected := &placement.Rejections{}
	var chosen rt.BundleRoute
	var chosenScore float64
	var chosenCached bool
	for _, worker := range workers {
		sample, reason, err := workerPlacementRejection(ctx, tx, worker, controlEpoch, kind, req, now)
		if err != nil {
			return rt.BundleRoute{}, err
		}
		if reason != "" || len(worker.routes) == 0 {
			if reason == "" {
				reason = "runtime_missing"
			}
			rejected.Candidates = append(rejected.Candidates, placement.Rejection{WorkerID: worker.id, Reason: reason})
			continue
		}
		var available placement.Resources
		var resourceReason string
		var readAvailable bool
		for _, candidate := range worker.routes {
			profile, reason := candidateRejection(req, candidate, sample)
			if reason == "" {
				if !readAvailable {
					available, resourceReason, err = placementAvailability(ctx, tx, worker.id, sample, req)
					if err != nil {
						return rt.BundleRoute{}, err
					}
					readAvailable = true
				}
				reason = resourceReason
			}
			if reason != "" {
				rejected.Candidates = append(rejected.Candidates, placement.Rejection{
					WorkerID: worker.id, RuntimeInstanceID: candidate.route.InstanceID, Reason: reason,
				})
				continue
			}
			capacity := sample.Budget.Capacity
			budgetFree := capacity.Subtract(sample.Budget.Allocated)
			pressurePenalty := placement.Score(capacity, placement.Resources{
				MemoryMiB:    max(0, budgetFree.MemoryMiB-sample.Pressure.AvailableMemoryMiB),
				StorageBytes: max(0, budgetFree.StorageBytes-sample.Pressure.DiskFreeBytes),
			})
			score := placement.Score(capacity, available.Subtract(req.Resources)) + 2*pressurePenalty
			cached := req.ArtifactDigest != "" && slices.Contains(profile.CachedArtifacts, req.ArtifactDigest)
			if chosen.InstanceID == "" || score < chosenScore || score == chosenScore && cached && !chosenCached {
				chosen, chosenScore, chosenCached = candidate.route, score, cached
			}
		}
	}
	if chosen.InstanceID == "" {
		return rt.BundleRoute{}, rejected
	}
	return chosen, nil
}

func insertPlacementReservation(ctx context.Context, tx *sql.Tx, op Operation, route rt.BundleRoute, req placement.Request, attempt, nativeAttempt int64) error {
	// This digest identifies the central acceptance intent. The native wire
	// digest is added independently when the outgoing mutation is journaled.
	intent, err := json.Marshal(struct {
		Operation, Kind, Epoch string
		Route                  rt.BundleRoute
		Request                json.RawMessage
		Requirements           placement.Request
	}{op.ID, op.Kind, op.ControlEpoch, route, op.Request, req})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO placement_reservations
		(operation_id,attempt,native_attempt,worker_id,runtime_instance_id,bundle_id,control_epoch,request_digest,
		 vcpus,memory_mib,storage_bytes,heavy_slots,phase,updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,'reserved_not_sent',?)`,
		op.ID, attempt, nativeAttempt, route.WorkerID, route.InstanceID, route.BundleID, op.ControlEpoch, fmt.Sprintf("sha256:%x", sha256.Sum256(intent)),
		req.Resources.VCPUs, req.Resources.MemoryMiB, req.Resources.StorageBytes, req.Resources.HeavySlots, time.Now().UnixMilli())
	return err
}

// operationPlacementRequest scopes every existing-resource action to the
// persisted owner and derives source compute for native Fork and Wake.
func operationPlacementRequest(ctx context.Context, tx *sql.Tx, r AcceptRequest, prior Machine, route rt.BundleRoute, request json.RawMessage) (*placement.Request, error) {
	worker := route.Owner()
	if r.Kind != "create" || r.Placement == nil {
		if err := checkWorkerEntrance(ctx, tx, worker, r.Kind); err != nil {
			return nil, err
		}
	}
	if r.Placement != nil || r.ResolvePlacement != nil {
		if r.Placement == nil || !oneOf(r.Kind, "create", rt.VMRestore) {
			return nil, ErrInvalid
		}
		requirements := *r.Placement
		if r.ResolvePlacement != nil {
			resolved, err := r.ResolvePlacement(ctx, tx, request)
			if err != nil {
				return nil, err
			}
			if resolved == nil {
				return nil, ErrInvalid
			}
			if resolved.Mode != requirements.Mode || resolved.WorkerID != requirements.WorkerID ||
				(requirements.RuntimeInstanceID != "" && resolved.RuntimeInstanceID != requirements.RuntimeInstanceID) {
				return nil, ErrConflict
			}
			requirements = *resolved
		}
		if route.InstanceID != "" {
			if requirements.RuntimeInstanceID != "" && requirements.RuntimeInstanceID != route.InstanceID {
				return nil, ErrConflict
			}
			requirements.RuntimeInstanceID = route.InstanceID
			if r.Kind == rt.VMRestore {
				requirements.OwnerWorkerID = worker
			}
		}
		return &requirements, nil
	}
	if route.InstanceID == "" || !rt.IsNativeFork(r.Kind) && !oneOf(r.Kind, "start", rt.Wake) {
		return nil, nil
	}
	if prior.CurrentBinding == nil || (r.Kind == "start" && prior.CurrentBinding.PowerState == PowerRunning) {
		return nil, nil
	}
	managed, err := placementWorkerManaged(ctx, tx, worker)
	if err != nil || !managed {
		return nil, err
	}
	count := int64(1)
	capability := rt.SystemBootCapability
	if rt.IsNativeFork(r.Kind) {
		count = int64(len(r.ForkChildren))
		capability = rt.FullStateOperationCapability(r.Kind)
	} else if r.Kind == rt.Wake {
		capability = rt.NativePowerCapability(r.Kind)
	} else if r.Kind == "start" && prior.CurrentBinding.PowerState == PowerHibernated {
		capability = rt.FullStateOperationCapability(rt.VMRestore)
	}
	req, err := ownerPlacement(prior.Spec, route, worker, capability, count, r.Heavy)
	if err != nil {
		return nil, err
	}
	if r.Kind == rt.Wake {
		// Explicit standby wake retains the same resident execution. The
		// access path reserves separately for hibernate Restore and launch.
		req.Resources = placement.Resources{HeavySlots: req.Resources.HeavySlots}
	}
	return req, nil
}

func ownerPlacement(spec json.RawMessage, route rt.BundleRoute, worker, capability string, count int64, heavy bool) (*placement.Request, error) {
	var source struct {
		Resources struct {
			CPUs    uint8  `json:"cpu_count"`
			Memory  uint32 `json:"memory_mib"`
			Storage uint32 `json:"storage_gib"`
			Overlay uint32 `json:"overlay_gib"`
		}
	}
	if err := json.Unmarshal(spec, &source); err != nil {
		return nil, err
	}
	if count < 1 || count > 1024 || source.Resources.CPUs == 0 || source.Resources.Memory == 0 {
		return nil, fmt.Errorf("%w: source resources are required for Worker admission", ErrInvalid)
	}
	resources := placement.Resources{
		VCPUs:     int64(source.Resources.CPUs) * count,
		MemoryMiB: int64(source.Resources.Memory) * count,
	}
	storageGiB := int64(source.Resources.Storage) + int64(source.Resources.Overlay)
	if storageGiB > (1<<33-1)/count {
		return nil, ErrInvalid
	}
	resources.StorageBytes = storageGiB * count << 30
	if heavy {
		resources.HeavySlots = 1
	}
	return &placement.Request{Mode: placement.Pinned, WorkerID: worker, OwnerWorkerID: worker,
		RuntimeInstanceID: route.InstanceID, BundleID: route.BundleID, Capabilities: []string{capability},
		Resources: resources}, nil
}

func reserveForkRetry(ctx context.Context, tx *sql.Tx, op Operation) error {
	last, err := readPlacementReservation(ctx, tx, op.ID)
	if errors.Is(err, ErrNotFound) {
		worker := op.WorkerID
		if worker == "" {
			worker = "local-worker"
		}
		return checkWorkerEntrance(ctx, tx, worker, "fork_retry")
	}
	if err != nil {
		return err
	}
	control, err := readControl(ctx, tx)
	if err != nil {
		return err
	}
	if err := checkWritable(ctx, tx); err != nil {
		return err
	}
	if control.ControlEpoch != op.ControlEpoch {
		return ErrControlEpoch
	}
	if oneOf(last.Phase, placement.DispatchStarted, placement.Unknown, placement.Admitted, placement.Reserved) {
		return fmt.Errorf("%w: prior Worker reservation has not reconciled", ErrOperationState)
	}
	source, err := readMachine(ctx, tx, op.MachineID)
	if err != nil {
		return err
	}
	route, err := loadRuntimeRoute(ctx, tx, last.RuntimeInstanceID)
	if err != nil {
		return err
	}
	if route.WorkerID != last.WorkerID || route.BundleID != last.BundleID {
		return ErrGeneration
	}
	var count, nativeAttempt int64
	for _, child := range op.Children {
		if child.State == "quarantined" && child.ErrorCode == "NATIVE_CHILD_ABORTED" {
			count++
			nativeAttempt = max(nativeAttempt, child.Attempt+1)
		}
	}
	req, err := ownerPlacement(source.Spec, route, route.WorkerID, rt.FullStateOperationCapability(op.Kind), count, op.Heavy)
	if err != nil {
		return err
	}
	route, err = selectPlacement(ctx, tx, op.ControlEpoch, "fork_retry", *req, time.Now())
	if err != nil {
		return err
	}
	return insertPlacementReservation(ctx, tx, op, route, *req, last.Attempt+1, nativeAttempt)
}

func placementWorkerManaged(ctx context.Context, q queryer, worker string) (bool, error) {
	var managed bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM worker_samples WHERE worker_id=?)
		OR EXISTS(SELECT 1 FROM worker_enrollments WHERE worker_id=?)`, worker, worker).Scan(&managed)
	return managed, err
}
