package platformapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
)

func TestManagedSSHCapabilityUsesCurrentRequesterAndIgnoresTemplateClaims(t *testing.T) {
	s, owner := managedSSHTunnelFixture(t)
	ctx := context.Background()
	manage := managedSSHTunnelToken(t, s, owner, "ssh-capability-manage", auth.AccessManage)
	read := managedSSHTunnelToken(t, s, owner, "ssh-capability-read", auth.AccessRead)
	for _, path := range []string{"/v1/machines", "/v1/machines/m_managed_ssh"} {
		for _, tc := range []struct {
			token   string
			allowed bool
		}{{manage, true}, {read, false}, {manage, true}} {
			cap, details := readManagedSSHCapability(t, s, path, tc.token)
			if !cap.Supported || cap.Status != "supported" || details.ManageAllowed != tc.allowed ||
				details.Path != "configured" || details.HostKeyStatus != "unknown" || details.Reachability != "unknown" || details.AgentExec != "unknown" {
				t.Fatalf("%s manage=%v: %+v %+v", path, tc.allowed, cap, details)
			}
		}
	}
	if _, err := s.DB.DB().ExecContext(ctx, `UPDATE templates SET spec_json=json_remove(spec_json,'$.managed_ssh'),
		capabilities_json='{"access.managed_ssh":{"supported":true}}' WHERE id='system'`); err != nil {
		t.Fatal(err)
	}
	cap, details := readManagedSSHCapability(t, s, "/v1/machines/m_managed_ssh", manage)
	if cap.Supported || cap.Status != "unsupported" || details.Path != "unavailable" || details.Reachability != "unavailable" {
		t.Fatalf("template declaration overrode native profile admission: %+v %+v", cap, details)
	}
	var operations, connections int
	if err := s.DB.ReadDB().QueryRowContext(ctx, `SELECT (SELECT count(*) FROM operations), (SELECT count(*) FROM connection_activity)`).Scan(&operations, &connections); err != nil || operations != 0 || connections != 0 {
		t.Fatalf("capability read started activity: %d operations, %d connections, %v", operations, connections, err)
	}
}

func readManagedSSHCapability(t *testing.T, s *Server, path, token string) (rt.Capability, managedSSHConstraints) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, w.Code, w.Body.String())
	}
	var body struct {
		Capabilities map[string]rt.Capability `json:"capabilities"`
		Items        []struct {
			Capabilities map[string]rt.Capability `json:"capabilities"`
		} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if path == "/v1/machines" {
		if len(body.Items) != 1 {
			t.Fatalf("unexpected list: %s", w.Body.String())
		}
		body.Capabilities = body.Items[0].Capabilities
	}
	cap := body.Capabilities[managedSSHAccessCapability]
	var details managedSSHConstraints
	if err := json.Unmarshal(cap.Constraints, &details); err != nil {
		t.Fatalf("decode managed capability: %v", err)
	}
	return cap, details
}

func TestManagedSSHCapabilityRequiresExactFreshAgentEvidence(t *testing.T) {
	s, _ := managedSSHTunnelFixture(t)
	m, err := s.DB.GetMachine(context.Background(), "m_managed_ssh")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	old := now.Add(-time.Hour)
	for _, tc := range []struct {
		name, route, status, hostKey, agent string
		power                               registry.PowerState
		observedAt                          *time.Time
		verifiedKey, matches, stream        bool
	}{
		{"live agent", "serve", "supported", "observed", "observed", registry.PowerRunning, &now, true, true, true},
		{"old agent", "serve", "unsupported", "observed", "unavailable", registry.PowerRunning, &now, true, true, false},
		{"other execution", "serve", "supported", "unavailable", "unknown", registry.PowerRunning, &now, false, false, false},
		{"stood by", "serve", "supported", "observed", "observed", registry.PowerStandby, &now, true, true, true},
		{"stopped", "serve", "supported", "unavailable", "unknown", registry.PowerStopped, &now, true, true, true},
		{"stale", "serve", "supported", "unknown", "unknown", registry.PowerRunning, &old, true, true, true},
		{"unregistered route", "other", "unknown", "unknown", "unknown", registry.PowerRunning, &now, true, true, true},
		{"official", "", "unsupported", "unknown", "unknown", registry.PowerRunning, &now, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := *m.CurrentBinding
			b.RuntimeInstanceID = tc.route
			if tc.route == "" {
				b.RuntimeBundleID = ""
			}
			b.ObservedAt = tc.observedAt
			exec := b.ExecutionRef()
			if !tc.matches {
				exec.ExecutionEpoch++
			}
			b.GuestManagementEvidence, err = json.Marshal(map[string]any{"exec_stream": rt.GuestExecStreamEvidence{Execution: exec, Supported: tc.stream}})
			if err != nil {
				t.Fatal(err)
			}
			v := machineView{Machine: m, Binding: &b, PowerState: tc.power}
			result := s.managedSSHView(&v, true, tc.verifiedKey)
			if result.capability.Status != tc.status || result.constraints.HostKeyStatus != tc.hostKey ||
				result.constraints.AgentExec != tc.agent || result.constraints.Reachability == "observed" {
				t.Fatalf("unexpected capability: %+v", result)
			}
		})
	}
}
