package platformapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
)

type activityPolicy struct {
	AllowActiveTasks bool `json:"allow_active_tasks,omitempty"`
}

type fullStateAction struct {
	ExpectedGeneration int64           `json:"expected_generation"`
	ActivityPolicy     *activityPolicy `json:"activity_policy,omitempty"`
}

type forkBatchRequest struct {
	MemoryForkBackend  string          `json:"memory_fork_backend,omitempty"`
	LifecyclePolicy    json.RawMessage `json:"lifecycle_policy,omitempty"`
	ExpectedGeneration int64           `json:"expected_generation"`
	RequireIndependent bool            `json:"require_independent"`
	Admission          string          `json:"admission,omitempty"`
	Children           []struct {
		Name string `json:"name"`
	} `json:"children"`
	VolumePolicy *struct {
		Default string `json:"default,omitempty"`
	} `json:"volume_policy,omitempty"`
}

func validChildName(name string) bool {
	return strings.TrimSpace(name) != "" && len(name) <= 255 && strings.IndexByte(name, 0) == -1
}

func (s *Server) fullStateRoutes() {
	for _, kind := range []string{rt.Standby, rt.Hibernate} {
		s.route("POST /v1/machines/{machine}/"+kind, func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
			id := r.PathValue("machine")
			if err := p.Authorize("manage", id); err != nil {
				return err
			}
			var v fullStateAction
			if err := decode(r, &v); err != nil {
				return err
			}
			if v.ExpectedGeneration < 1 {
				return registry.ErrInvalid
			}
			if kind == rt.Hibernate {
				if err := s.machineStatePointProfile(r.Context(), id, kind); err != nil {
					return err
				}
			}
			return s.accept(w, r, p, v, registry.AcceptRequest{
				Kind: kind, MachineID: id, ExpectedGeneration: v.ExpectedGeneration, Heavy: kind == rt.Hibernate,
			})
		})
	}
	s.route("POST /v1/machines/{machine}/fork-batches", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		id := r.PathValue("machine")
		if err := p.Authorize("manage", id); err != nil {
			return err
		}
		if p.Scope.MachineID != "" {
			return auth.ErrForbidden
		}
		var v forkBatchRequest
		if err := decode(r, &v); err != nil {
			return err
		}
		if !rt.ValidMemoryForkBackend(v.MemoryForkBackend) || v.ExpectedGeneration < 1 || len(v.Children) == 0 || len(v.Children) > rt.MaxForkChildren ||
			(rt.ForkAdmission(v.Admission) != rt.ForkAllOrReject && v.Admission != rt.ForkBestEffort) ||
			(v.VolumePolicy != nil && v.VolumePolicy.Default != "" && v.VolumePolicy.Default != "clone") {
			return registry.ErrInvalid
		}
		names := make(map[string]bool, len(v.Children))
		children := make([]string, 0, len(v.Children))
		for _, child := range v.Children {
			if !validChildName(child.Name) || names[child.Name] {
				return registry.ErrInvalid
			}
			names[child.Name] = true
			children = append(children, child.Name)
		}
		return s.accept(w, r, p, v, registry.AcceptRequest{
			MemoryForkBackend: v.MemoryForkBackend, Kind: rt.ForkBatch, MachineID: id, ExpectedGeneration: v.ExpectedGeneration, Heavy: true, ForkChildren: children,
		})
	})
}
