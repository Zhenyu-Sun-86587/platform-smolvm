package platformapi

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/diagnostics"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/workerclient"
)

type settings struct {
	Lifecycle         json.RawMessage `json:"default_lifecycle_policy,omitempty"`
	LifecycleRevision int64           `json:"default_lifecycle_revision,omitempty"`
	Revision          int64           `json:"revision"`
	IdleSeconds       int             `json:"default_idle_seconds"`
	BootMode          rt.BootMode     `json:"default_boot_mode,omitempty"`
	NetworkMode       string          `json:"default_network_mode,omitempty"`
	NetworkID         string          `json:"default_network_id,omitempty"`
}

func (s *Server) settings(ctx context.Context) (settings, error) {
	return s.readSettings(ctx, s.DB.ReadDB())
}

func (s *Server) readSettings(ctx context.Context, db interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (settings, error) {
	var raw string
	v := settings{Revision: 1, IdleSeconds: s.Config.Lifecycle.IdleSeconds, BootMode: rt.BootOCI}
	err := db.QueryRowContext(ctx, "SELECT settings_json FROM meta WHERE id=1").Scan(&raw)
	if err != nil {
		return v, err
	}
	err = json.Unmarshal([]byte(raw), &v)
	return v, err
}

var imageRef = regexp.MustCompile(`^[a-z0-9][a-z0-9./:_-]*@sha256:[0-9a-f]{64}$`)

func (s *Server) settingsRoutes() {
	s.machineResourceRoutes()
	s.policyApplicationRoutes()
	s.route("GET /v1/system/diagnostics", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		if err := p.Authorize("admin", ""); err != nil {
			return err
		}
		return write(w, 200, diagnostics.Collect(r.Context(), s.Config))
	})
	s.route("GET /v1/system/settings", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		if err := p.Authorize("admin", ""); err != nil {
			return err
		}
		v, err := s.settings(r.Context())
		if err != nil {
			return err
		}
		mode := v.NetworkMode
		if mode == "" {
			mode = rt.NativeNetwork
		}
		policy, err := rt.DefaultLifecyclePolicy().WithPatch(v.Lifecycle)
		if err != nil {
			return err
		}
		if v.LifecycleRevision == 0 {
			v.LifecycleRevision = 1
		}
		return write(w, 200, map[string]any{"default_lifecycle_policy": policy, "default_lifecycle_revision": v.LifecycleRevision, "revision": v.Revision, "default_idle_seconds": v.IdleSeconds, "default_network_mode": mode, "default_boot_mode": v.BootMode,
			"default_network_id": v.NetworkID, "public_origin": s.Config.API.PublicOrigin, "apps_domain": s.Config.Gateway.AppsDomain,
			"tcp_mtls_port_range": s.Config.Gateway.TCPRange, "max_heavy_operations": s.Config.Lifecycle.Heavy, "runtime_socket": s.Config.Runtime.Socket})
	})
	s.route("PATCH /v1/system/settings", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		if err := p.Authorize("admin", ""); err != nil {
			return err
		}
		var body struct {
			Lifecycle   json.RawMessage `json:"default_lifecycle_policy,omitempty"`
			Idle        *int            `json:"default_idle_seconds,omitempty"`
			BootMode    *rt.BootMode    `json:"default_boot_mode,omitempty"`
			NetworkMode *string         `json:"default_network_mode,omitempty"`
			NetworkID   *string         `json:"default_network_id,omitempty"`
		}
		if err := decode(r, &body); err != nil {
			return err
		}
		if (body.Lifecycle == nil && body.Idle == nil && body.BootMode == nil && body.NetworkMode == nil && body.NetworkID == nil) ||
			(body.BootMode != nil && *body.BootMode != rt.BootOCI && *body.BootMode != rt.BootSystem) ||
			(body.Idle != nil && (*body.Idle < 1 || *body.Idle > 31536000)) ||
			(body.NetworkMode != nil && *body.NetworkMode != rt.NativeNetwork && *body.NetworkMode != rt.FixedInternalNetwork) {
			return registry.ErrInvalid
		}
		rev, err := revision(r)
		if err != nil {
			return err
		}
		return s.mutate(w, r, p, body, func(ctx context.Context, tx *sql.Tx) (registry.MutationResult, error) {
			v, err := s.readSettings(ctx, tx)
			if err != nil {
				return registry.MutationResult{}, err
			}
			if body.BootMode != nil {
				v.BootMode = *body.BootMode
			}
			if body.Lifecycle != nil {
				prior, err := rt.DefaultLifecyclePolicy().WithPatch(v.Lifecycle)
				if err != nil {
					return registry.MutationResult{}, err
				}
				value, err := prior.WithPatch(body.Lifecycle)
				if err != nil {
					return registry.MutationResult{}, registry.ErrInvalid
				}
				v.Lifecycle = lifecycle.JSON(value)
				if v.LifecycleRevision == 0 {
					v.LifecycleRevision = 1
				}
				v.LifecycleRevision++
			}
			if body.Idle != nil {
				v.IdleSeconds = *body.Idle
			}
			if body.NetworkMode != nil {
				v.NetworkMode = *body.NetworkMode
			}
			if body.NetworkID != nil {
				if *body.NetworkID != "" {
					found, err := s.hasPrivateNetwork(ctx, *body.NetworkID)
					if err != nil {
						return registry.MutationResult{}, err
					}
					if !found {
						return registry.MutationResult{}, registry.ErrInvalid
					}
				}
				v.NetworkID = *body.NetworkID
			}
			v.Revision = rev + 1
			raw := lifecycle.JSON(v)
			res, err := tx.ExecContext(ctx, "UPDATE meta SET settings_json=? WHERE id=1 AND COALESCE(json_extract(settings_json,'$.revision'),1)=?", string(raw), rev)
			if err != nil {
				return registry.MutationResult{}, err
			}
			n, err := res.RowsAffected()
			if err != nil || n != 1 {
				return registry.MutationResult{}, registry.ErrRevision
			}
			return registry.MutationResult{Response: raw}, registry.AppendEvent(ctx, tx, "maintenance", "", "", v, true)
		})
	})
	s.route("POST /v1/templates", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		if err := p.Authorize("admin", ""); err != nil {
			return err
		}
		var v struct {
			ID      string `json:"template_id"`
			Name    string `json:"name"`
			Version int    `json:"version"`
			Image   string `json:"image_digest"`
		}
		if err := decode(r, &v); err != nil {
			return err
		}
		if !imageRef.MatchString(v.Image) || v.ID == "" || v.Version < 1 || v.Name == "" {
			return registry.ErrInvalid
		}
		return s.mutate(w, r, p, v, func(ctx context.Context, tx *sql.Tx) (registry.MutationResult, error) {
			_, err := tx.ExecContext(ctx, "INSERT INTO templates(id,name,version,image_digest,spec_json,capabilities_json) VALUES(?,?,?,?,'{}','{}')", v.ID, v.Name, v.Version, v.Image)
			return registry.MutationResult{Response: lifecycle.JSON(v)}, err
		})
	})
	s.route("PATCH /v1/machines/{machine}", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		id := r.PathValue("machine")
		if err := p.Authorize("manage", id); err != nil {
			return err
		}
		var v patchMachine
		if err := decode(r, &v); err != nil {
			return err
		}
		if v.Idle != nil && v.Policy != nil {
			return registry.ErrInvalid
		}
		if v.Policy != nil {
			saved, err := s.DB.LifecyclePolicy(r.Context(), id)
			if err != nil {
				return err
			}
			if _, err = saved.Desired.WithPatch(v.Policy); err != nil {
				return registry.ErrInvalid
			}
			if v.ExpectedPolicyRevision < 1 {
				return registry.ErrRevision
			}
		}
		if v.Idle != nil && (v.Idle.Timeout < 1 && v.Idle.Enabled) {
			return registry.ErrInvalid
		}
		if v.Name != nil && strings.TrimSpace(*v.Name) == "" {
			return registry.ErrInvalid
		}
		if v.Resources != nil {
			if _, err := v.Resources.cpuLimit(); err != nil {
				return err
			}
		}
		return s.accept(w, r, p, v, registry.AcceptRequest{Kind: "configure", MachineID: id, ExpectedGeneration: v.Generation, Heavy: v.Resources != nil})
	})
	s.Extra["configure"] = s.configure
	s.publicCredentials()
}

type patchMachine struct {
	Policy                 json.RawMessage       `json:"lifecycle_policy,omitempty"`
	ExpectedPolicyRevision int64                 `json:"expected_policy_revision,omitempty"`
	Name                   *string               `json:"name,omitempty"`
	Idle                   *lifecycle.IdlePolicy `json:"idle_policy,omitempty"`
	Keep                   *bool                 `json:"keep_running,omitempty"`
	Generation             int64                 `json:"expected_generation,omitempty"`
	Resources              *machineResourcePatch `json:"resources,omitempty"`
}

type machineResourcePatch struct {
	CPU     uint8           `json:"cpu_count,omitempty"`
	Memory  uint32          `json:"memory_mib,omitempty"`
	Storage uint64          `json:"storage_gib,omitempty"`
	Overlay uint64          `json:"overlay_gib,omitempty"`
	Limit   json.RawMessage `json:"cpu_limit_millicpus,omitempty"`
	Balloon *uint32         `json:"balloon_target_mib,omitempty"`
}

func (r *machineResourcePatch) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for field, raw := range fields {
		switch field {
		case "cpu_limit_millicpus":
			if string(raw) == "null" {
				continue
			}
		case "cpu_count", "memory_mib", "storage_gib", "overlay_gib", "balloon_target_mib":
		default:
			return fmt.Errorf("unknown resource setting %q", field)
		}
		var n *uint64
		if err := json.Unmarshal(raw, &n); err != nil || n == nil || (*n == 0 && field != "balloon_target_mib") {
			return fmt.Errorf("invalid integer for %s", field)
		}
	}
	type wire machineResourcePatch
	return json.Unmarshal(data, (*wire)(r))
}

func (r machineResourcePatch) cpuLimit() (*uint32, error) {
	var limit *uint32
	if len(r.Limit) != 0 {
		if err := json.Unmarshal(r.Limit, &limit); err != nil || limit != nil && *limit == 0 {
			return nil, registry.ErrInvalid
		}
	}
	return limit, nil
}

func (r machineResourcePatch) resizesMachine() bool {
	return r.CPU > 0 || r.Memory > 0 || r.Storage > 0 || r.Overlay > 0
}

// Dispatching remains durable while the Worker is unavailable. The same
// policy revision, expected execution and request key are retried on restart.
func policyDeliveryError(err error) error {
	if workerclient.RetryDelivery(err) {
		return nil
	}
	return err
}

func (s *Server) configure(ctx context.Context, j *lifecycle.Job) error {
	var v patchMachine
	if err := json.Unmarshal(j.Op.Request, &v); err != nil {
		return err
	}
	resourceDispatch := j.Intent.Stage == "managed_resource_cpu_limit" || j.Intent.Stage == "managed_resources"
	if client, ok := lifecycle.PolicyClient(j.Runtime, j.Binding); ok && (v.Policy != nil || v.Idle != nil || v.Keep != nil) && !resourceDispatch {
		if j.Intent.Stage != "apply_lifecycle_policy" {
			if err := j.Stage(ctx, "apply_lifecycle_policy", ""); err != nil {
				return err
			}
		}
		prior, err := s.Engine.ApplyMachinePolicy(ctx, j.Binding, client)
		if err != nil {
			return policyDeliveryError(err)
		}
		override, source, err := registry.LifecycleOverrides(j.Op.Request)
		if err != nil {
			return registry.ErrInvalid
		}
		rev := v.ExpectedPolicyRevision
		if rev == 0 {
			rev = prior.DesiredRevision
		}
		if _, err = s.DB.SetLifecycleDesired(ctx, j.Op.MachineID, j.Op.ID, source, rev, prior.DefaultRevision, override); err != nil {
			return err
		}
		if _, err = s.Engine.ApplyMachinePolicy(ctx, j.Binding, client); err != nil {
			return policyDeliveryError(err)
		}
	} else if v.Policy != nil && !resourceDispatch {
		return &lifecycle.Failure{Code: "CAPABILITY_UNSUPPORTED", Message: "this binding has no Worker lifecycle controller"}
	}
	var obs *registry.Observation
	status := "applied"
	result := map[string]any{"machine_id": j.Op.MachineID}
	r := v.Resources
	var limit *uint32
	var err error
	if r != nil {
		if limit, err = r.cpuLimit(); err != nil {
			return err
		}
	}
	worker := workerclient.ForAdapter(j.Runtime, j.Binding.RuntimeBundleID)
	if r != nil && worker != nil && (r.resizesMachine() || r.Balloon != nil) {
		report, err := s.configureManagedResources(ctx, j, *r, limit, worker)
		if err != nil || report == nil {
			return managedSettingsError(ctx, j, err)
		}
		for k, val := range report {
			result[k] = val
		}
	} else if r != nil && r.Balloon != nil {
		return &lifecycle.Failure{Code: "CAPABILITY_UNSUPPORTED", Message: "the selected runtime does not support managed memory ballooning"}
	} else if r != nil && len(r.Limit) != 0 && worker != nil {
		receipt, err := managedCPULimit(ctx, j, limit, worker)
		if err != nil {
			return managedSettingsError(ctx, j, err)
		}
		result["cpu_limit"] = receipt
		j.Written = true
	} else if r != nil {
		o, err := j.Runtime.Observe(ctx, j.Binding.RuntimeName)
		if err != nil {
			return err
		}
		if o.Labels["platform.binding"] != j.Binding.ID {
			return registry.ErrGeneration
		}
		if (r.Storage > 0 && r.Storage < o.StorageGB) || (r.Overlay > 0 && r.Overlay < o.OverlayGB) {
			return &lifecycle.Failure{Code: "INVALID_ARGUMENT", Message: "disk shrink is unsupported"}
		}
		if r.resizesMachine() {
			if o.State != "running" || !o.Identity.Complete() {
				status = "requires_restart"
			} else {
				if err = j.Stage(ctx, "resize", ""); err != nil {
					return err
				}
				o, err = j.Runtime.Resize(ctx, j.Binding.RuntimeName, rt.ResizeRequest{OperationID: j.Op.ID, ExpectedRuntime: o.Identity, CPUs: r.CPU, MemoryMB: r.Memory, StorageGB: r.Storage, OverlayGB: r.Overlay})
				if err != nil {
					return err
				}
				j.Written = true
			}
		}
		if len(r.Limit) != 0 {
			if err = j.Stage(ctx, "cpu_limit", ""); err != nil {
				return err
			}
			o, err = j.Runtime.SetCPULimit(ctx, j.Binding.RuntimeName, limit)
			if err != nil {
				return err
			}
			j.Written = true
		}
		x := lifecycle.Evidence(o)
		obs = &x
	}
	result["status"] = status
	return j.Done(ctx, registry.TransitionRequest{Effect: "applied", Observation: obs, Result: lifecycle.JSON(result), Apply: func(ctx context.Context, tx *sql.Tx) error {
		var idle, res any
		if v.Idle != nil {
			idle = string(lifecycle.JSON(v.Idle))
		}
		if v.Resources != nil && status == "applied" {
			res = string(lifecycle.JSON(v.Resources))
		}
		_, err := tx.ExecContext(ctx, `UPDATE machines SET name=COALESCE(?,name),keep_running=COALESCE(?,keep_running),idle_policy_json=COALESCE(?,idle_policy_json),spec_json=CASE WHEN ? IS NULL THEN spec_json ELSE json_set(spec_json,'$.resources',json_patch(COALESCE(json_extract(spec_json,'$.resources'),'{}'),?)) END WHERE id=?`, v.Name, v.Keep, idle, res, res, j.Op.MachineID)
		return err
	}})
}
func (s *Server) publicCredentials() {
	s.route("POST /v1/ssh-keys", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		if err := p.Authorize("admin", ""); err != nil {
			return err
		}
		var v struct {
			Label string `json:"label"`
			Key   string `json:"public_key"`
		}
		if err := decode(r, &v); err != nil {
			return err
		}
		key, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(v.Key))
		if err != nil || len(options) != 0 || len(rest) != 0 || len(v.Label) > 120 {
			return registry.ErrInvalid
		}
		if _, ok := key.(*ssh.Certificate); ok {
			return registry.ErrInvalid
		}
		pub := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
		digest := sha256.Sum256(key.Marshal())
		return s.mutate(w, r, p, v, func(ctx context.Context, tx *sql.Tx) (registry.MutationResult, error) {
			id, err := registry.NewID("key_")
			if err != nil {
				return registry.MutationResult{}, err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO credentials(id,principal_id,kind,digest,public_material,label,scope_json,created_at) VALUES(?,?,'ssh_public_key',?,?,?,'{}',?)`, id, p.ID, hex.EncodeToString(digest[:]), pub, v.Label, time.Now().UnixMilli())
			if err == nil {
				err = registry.AppendEvent(ctx, tx, "auth", "", "", map[string]string{"action": "ssh_key_added", "credential_id": id}, true)
			}
			return registry.MutationResult{Response: lifecycle.JSON(map[string]any{"id": id, "public_key": pub, "guest_update": "pending", "revision": 1})}, err
		})
	})
	for path, kind := range map[string]string{"ssh-keys": "ssh_public_key", "client-certificates": "client_certificate"} {
		s.route("GET /v1/"+path, func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
			if err := p.Authorize("admin", ""); err != nil {
				return err
			}
			rows, err := s.DB.ReadDB().QueryContext(r.Context(), `SELECT id,label,public_material,COALESCE(revoked_at,0) FROM credentials WHERE kind=? AND principal_id=? ORDER BY id`, kind, p.ID)
			if err != nil {
				return err
			}
			defer rows.Close()
			out := []map[string]any{}
			for rows.Next() {
				var id, label, key string
				var rev int64
				if err = rows.Scan(&id, &label, &key, &rev); err != nil {
					return err
				}
				item := map[string]any{"id": id, "label": label, "public_material": key, "revision": 1}
				if rev != 0 {
					item["revoked_at"] = time.UnixMilli(rev).UTC()
					item["revision"] = 2
				}
				out = append(out, item)
			}
			if err = rows.Err(); err != nil {
				return err
			}
			return list(w, r, out, func(v map[string]any) string { return v["id"].(string) })
		})
		s.route("DELETE /v1/"+path+"/{credential}", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
			if err := p.Authorize("admin", ""); err != nil {
				return err
			}
			return s.revokeCredential(w, r, p, r.PathValue("credential"), kind, "id")
		})
	}
}

func (s *Server) revokeCredential(w http.ResponseWriter, r *http.Request, p auth.Principal, id, kind, idField string) error {
	rev, err := revision(r)
	if err != nil {
		return err
	}
	var body struct{}
	if err = decode(r, &body); err != nil {
		return err
	}
	return s.mutate(w, r, p, nil, func(ctx context.Context, tx *sql.Tx) (registry.MutationResult, error) {
		if err := s.Auth.RevokeTx(ctx, tx, p, id, kind, rev); err != nil {
			return registry.MutationResult{}, err
		}
		return registry.MutationResult{Response: lifecycle.JSON(map[string]any{idField: id, "status": "revoked", "revision": rev + 1})}, nil
	})
}

var _ = fmt.Sprint
