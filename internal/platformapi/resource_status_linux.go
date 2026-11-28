package platformapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
	"sandbox-platform/internal/runtime/workerclient"
)

type resourceIssue struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func resourceFault(err error) *lifecycle.Failure {
	var f *workerclient.Fault
	if !errors.As(err, &f) {
		return nil
	}
	code := strings.TrimPrefix(f.Code, "E_")
	switch f.Code {
	case "E_METHOD_UNSUPPORTED", "E_PROTOCOL_UNSUPPORTED", "E_CAPABILITY_UNSUPPORTED":
		code = "CAPABILITY_UNSUPPORTED"
	case "E_INVALID_REQUEST":
		code = "INVALID_ARGUMENT"
	case "E_RESOURCE_LIMIT":
		code = "RESOURCE_EXHAUSTED"
	case "E_STALE_EXECUTION", "E_FENCE_UNPROVEN":
		code = "GENERATION_MISMATCH"
	}
	return &lifecycle.Failure{Code: code, Message: f.Message}
}

func readIssue(err error) resourceIssue {
	if failure := resourceFault(err); failure != nil {
		return resourceIssue{failure.Code, failure.Message}
	}
	return resourceIssue{"RESOURCE_OBSERVATION_UNAVAILABLE", "the current Worker resource observation could not be verified"}
}

func (s *Server) machineResourceRoutes() {
	s.route("GET /v1/machines/{machine}/resources", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		id := r.PathValue("machine")
		if err := p.Authorize("read", id); err != nil {
			return err
		}
		ctx := r.Context()
		m, err := s.DB.GetMachine(ctx, id)
		if err != nil {
			return err
		}
		var spec struct {
			Resources json.RawMessage `json:"resources"`
		}
		if err := json.Unmarshal(m.Spec, &spec); err != nil {
			return err
		}
		if len(spec.Resources) == 0 || string(spec.Resources) == "null" {
			spec.Resources = json.RawMessage("{}")
		}
		issues := make(map[string]resourceIssue)
		out := map[string]any{"machine_id": id, "binding_generation": m.BindingGeneration, "requested": spec.Resources, "issues": issues}
		b := m.CurrentBinding
		if b == nil {
			return registry.ErrGeneration
		}
		out["power_state"] = b.PowerState
		adapter, err := s.ForBinding(*b)
		if err != nil {
			return err
		}
		ref := lifecycle.WorkerExecution(*b)
		client := workerclient.ForAdapter(adapter, b.RuntimeBundleID)
		switch {
		case client == nil:
			issue := resourceIssue{"CAPABILITY_UNSUPPORTED", "this runtime does not expose Worker resource observations"}
			issues["compute"], issues["cpu_limit"] = issue, issue
		case ref.IncarnationID == "" || ref.Epoch < 1:
			issue := resourceIssue{"MACHINE_NOT_RUNNING", "the machine has no current execution to sample"}
			issues["compute"], issues["cpu_limit"] = issue, issue
		default:
			out["execution"] = ref
			if compute, err := client.ComputeResources(ctx, ref); err == nil {
				out["compute"] = compute
			} else {
				issues["compute"] = readIssue(err)
			}
			if limit, err := client.ManagedCPULimit(ctx, ref); err == nil {
				out["cpu_limit"] = limit
			} else {
				issues["cpu_limit"] = readIssue(err)
			}
			current, err := s.DB.GetMachine(ctx, id)
			if err != nil {
				return err
			}
			if current.CurrentBinding == nil || current.CurrentBinding.ID != b.ID || current.CurrentBinding.ExecutionRef() != b.ExecutionRef() {
				return registry.ErrGeneration
			}
			if current.Revision != m.Revision {
				return registry.ErrRevision
			}
		}
		return write(w, http.StatusOK, out)
	})
}
