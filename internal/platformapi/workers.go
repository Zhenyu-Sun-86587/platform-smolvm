package platformapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/placement"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
)

func (s *Server) remoteRuntime(id string) bool {
	if id == "" {
		return false
	}
	for _, n := range s.Config.Nodes {
		for _, r := range n.Routes {
			if r.RuntimeInstanceID == id {
				return true
			}
		}
	}
	return false
}

func (s *Server) preparePlacement(input *lifecycle.Create, accept *registry.AcceptRequest, digest string) error {
	choice := input.Placement
	if choice == nil {
		if len(s.Config.Nodes) == 0 || input.RuntimeInstanceID != "" && !s.remoteRuntime(input.RuntimeInstanceID) {
			return nil
		}
		choice = &lifecycle.Placement{Mode: placement.Auto}
		input.Placement = choice
	}
	req := placement.Request{Mode: choice.Mode, WorkerID: choice.WorkerID, RuntimeInstanceID: input.RuntimeInstanceID,
		Architecture: "x86_64", Hypervisor: "kvm", Capabilities: []string{rt.SystemBootCapability}, ArtifactDigest: digest}
	start := input.Start
	if start {
		req.Resources = placement.Resources{VCPUs: int64(input.Resources.CPUs), MemoryMiB: int64(input.Resources.Memory), HeavySlots: 1}
		req.Headroom = placement.Resources{MemoryMiB: 128, StorageBytes: 64 << 20}
	}
	if input.LiveComputeGrowth {
		req.Capabilities = append(req.Capabilities, rt.LiveComputeGrowthCapability)
	}
	if input.MemoryForkProfile != "" {
		req.Capabilities = append(req.Capabilities, rt.KernelCowLaunchCapability)
	}
	if err := req.Validate(); err != nil {
		return errors.Join(registry.ErrInvalid, err)
	}
	accept.Placement = &req
	// Candidate exclusions and local root sizes are observed facts. Exclude them
	// from the request fingerprint so a replay can recover its original owner.
	accept.ResolvePlacement = func(ctx context.Context, tx *sql.Tx, accepted json.RawMessage) (*placement.Request, error) {
		resolved := req
		var spec struct {
			Network *rt.LaunchNetwork `json:"network"`
		}
		if err := json.Unmarshal(accepted, &spec); err != nil {
			return nil, err
		}
		if spec.Network != nil {
			if spec.Network.Mode != "" && spec.Network.Mode != rt.NativeNetwork && spec.Network.Mode != rt.FixedInternalNetwork {
				return nil, registry.ErrInvalid
			}
			resolved.NetworkID = spec.Network.NetworkID
			resolved.NeedsDefaultNetwork = spec.Network.NetworkID == ""
		}
		resolved.EligibleRuntimeIDs = []string{}
		if start {
			resolved.StorageByRuntimeID = map[string]int64{}
		}
		for _, n := range s.Config.Nodes {
			for _, r := range n.Routes {
				if _, ok := r.Artifacts[digest]; !ok {
					continue
				}
				var size int64
				err := tx.QueryRowContext(ctx, `SELECT a.capacity_bytes FROM artifact_placements a
					JOIN runtime_instances i ON i.id=a.runtime_instance_id WHERE a.runtime_instance_id=? AND a.content_digest=?
					AND a.local_generation_id=? AND i.worker_id=? AND i.runtime_bundle_id=?`,
					r.RuntimeInstanceID, digest, r.Artifacts[digest], n.ID, r.BundleDigest).Scan(&size)
				if errors.Is(err, sql.ErrNoRows) {
					continue
				}
				if err != nil {
					return nil, err
				}
				resolved.EligibleRuntimeIDs = append(resolved.EligibleRuntimeIDs, r.RuntimeInstanceID)
				if start {
					resolved.StorageByRuntimeID[r.RuntimeInstanceID] = size
				}
			}
		}
		return &resolved, nil
	}
	accept.AdmitRuntime = func(route rt.BundleRoute, kind string) error {
		// The selection transaction has verified the fresh Worker profile.
		// Resolver admission also requires an installed native launcher.
		return s.AdmitRuntime(route, "start")
	}
	return nil
}

type workerView struct {
	registry.Worker
	EnrollmentGeneration int64                      `json:"enrollment_generation,omitempty"`
	EnrollmentStatus     string                     `json:"enrollment_status,omitempty"`
	Maintenance          registry.WorkerMaintenance `json:"maintenance"`
	Sample               *placement.Sample          `json:"sample,omitempty"`
	SampledAt            *time.Time                 `json:"sampled_at,omitempty"`
	Fresh                bool                       `json:"fresh_budget"`
}

func (s *Server) workerView(ctx context.Context, w registry.Worker) (workerView, error) {
	v := workerView{Worker: w}
	v.Endpoint = ""
	err := s.DB.ReadDB().QueryRowContext(ctx, `SELECT generation,status FROM worker_enrollments WHERE worker_id=? ORDER BY generation DESC LIMIT 1`, w.ID).Scan(&v.EnrollmentGeneration, &v.EnrollmentStatus)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return v, err
	}
	v.Maintenance, err = s.DB.GetWorkerMaintenance(ctx, w.ID)
	if err != nil {
		return v, err
	}
	sample, at, err := s.DB.GetWorkerSample(ctx, w.ID)
	if errors.Is(err, registry.ErrNotFound) {
		return v, nil
	}
	if err == nil {
		v.Sample = &sample
		v.SampledAt = &at
		v.Fresh = w.ObservedStatus == "ready" && time.Since(at) >= 0 && time.Since(at) <= placement.DefaultSampleAge
	}
	return v, err
}

func (s *Server) workerRoutes() {
	s.workerEnrollmentRoutes()
	s.route("GET /v1/workers", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		if err := p.Authorize(auth.ActionRead, ""); err != nil {
			return err
		}
		workers, err := s.DB.ListWorkers(r.Context())
		if err != nil {
			return err
		}
		views := make([]workerView, 0, len(workers))
		for _, worker := range workers {
			v, err := s.workerView(r.Context(), worker)
			if err != nil {
				return err
			}
			views = append(views, v)
		}
		return list(w, r, views, func(v workerView) string { return v.ID })
	})
	s.route("GET /v1/workers/{worker}", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		if err := p.Authorize(auth.ActionRead, ""); err != nil {
			return err
		}
		worker, err := s.DB.GetWorker(r.Context(), r.PathValue("worker"))
		if err != nil {
			return err
		}
		v, err := s.workerView(r.Context(), worker)
		if err != nil {
			return err
		}
		return write(w, 200, v)
	})
	s.route("PATCH /v1/workers/{worker}/maintenance", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		if err := p.Authorize(auth.ActionAdmin, ""); err != nil {
			return err
		}
		if r.PathValue("worker") == rt.LocalWorkerID {
			return fmt.Errorf("%w: Worker maintenance requires a NodeGateway gate", rt.ErrUnsupported)
		}
		var change struct {
			Mode   string `json:"mode"`
			Reason string `json:"reason"`
		}
		if err := decode(r, &change); err != nil {
			return err
		}
		rev, err := revision(r)
		if err != nil {
			return err
		}
		return s.mutate(w, r, p, change, func(ctx context.Context, tx *sql.Tx) (registry.MutationResult, error) {
			state, err := s.DB.SetWorkerMaintenanceTx(ctx, tx, r.PathValue("worker"), rev, change.Mode, change.Reason)
			if err != nil {
				return registry.MutationResult{}, err
			}
			response, err := json.Marshal(state)
			return registry.MutationResult{Response: response}, err
		})
	})
	s.route("POST /v1/workers/{worker}/enrollment/revoke", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		if err := p.Authorize(auth.ActionAdmin, ""); err != nil {
			return err
		}
		var req struct {
			Generation int64  `json:"enrollment_generation"`
			Reason     string `json:"reason"`
		}
		if err := decode(r, &req); err != nil {
			return err
		}
		rev, err := revision(r)
		if err != nil {
			return err
		}
		return s.mutate(w, r, p, req, func(ctx context.Context, tx *sql.Tx) (registry.MutationResult, error) {
			state, err := s.DB.RevokeWorkerEnrollmentTx(ctx, tx, r.PathValue("worker"), rev, req.Generation, req.Reason)
			if err != nil {
				return registry.MutationResult{}, err
			}
			response, err := json.Marshal(map[string]any{"worker_id": state.ID, "revision": state.Revision, "enrollment_generation": req.Generation, "enrollment_status": "revoked"})
			return registry.MutationResult{Response: response}, err
		})
	})
	s.route("GET /v1/operations/{operation}/placement", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		op, err := s.DB.GetOperation(r.Context(), r.PathValue("operation"))
		if err != nil {
			return err
		}
		if err = p.Authorize(auth.ActionRead, op.MachineID); err != nil {
			return err
		}
		v, err := s.DB.GetPlacementReservation(r.Context(), op.ID)
		if err != nil {
			return err
		}
		return write(w, 200, v)
	})
}
