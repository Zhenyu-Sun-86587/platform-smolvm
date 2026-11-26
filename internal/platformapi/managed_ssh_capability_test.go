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
	"sandbox-platform/internal/runtime/customsmolvm"
)

func addManagedSSHNetwork(s *Server) {
	s.Config.Runtime.Custom = []customsmolvm.Route{{
		InstanceID: "serve", Bundle: "bundle", WorkerSocket: (managedSSHTunnelRuntime{}).LifecycleWorkerSocket(),
		PrivateNetworks: []customsmolvm.PrivateNetwork{{ID: "private", RouteDomainID: "domain", CIDR: "10.42.0.0/24"}},
	}}
}

func TestManagedSSHCapabilityUsesCurrentRequesterAndIgnoresTemplateClaims(t *testing.T) {
	s, owner := managedSSHTunnelFixture(t)
	addManagedSSHNetwork(s)
	ctx := context.Background()
	manage := managedSSHTunnelToken(t, s, owner, "ssh-capability-manage", auth.AccessManage)
	read := managedSSHTunnelToken(t, s, owner, "ssh-capability-read", auth.AccessRead)
	cap, details := readManagedSSHCapability(t, s, "/v1/machines/m_managed_ssh", manage)
	if cap.Supported || cap.ReasonCode != "MANAGED_SSH_PRIVATE_NETWORK_REQUIRED" || details.Path != "unavailable" {
		t.Fatalf("machine without a private network exposed a path: %+v %+v", cap, details)
	}
	if _, err := s.DB.DB().ExecContext(ctx, `UPDATE machines SET spec_json='{"network":{"network_id":"private","mode":"native","route_domain_id":"domain","cidr":"10.42.0.0/24"}}' WHERE id='m_managed_ssh'`); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/v1/machines", "/v1/machines/m_managed_ssh"} {
		for _, tc := range []struct {
			token   string
			allowed bool
		}{{manage, true}, {read, false}, {manage, true}} {
			cap, details := readManagedSSHCapability(t, s, path, tc.token)
			if !cap.Supported || cap.Status != "supported" || details.ManageAllowed != tc.allowed ||
				details.Path != "configured" || details.HostKeyStatus != "unknown" || details.Reachability != "unknown" || details.GuestPort != 2222 {
				t.Fatalf("%s manage=%v: %+v %+v", path, tc.allowed, cap, details)
			}
		}
	}
	if _, err := s.DB.DB().ExecContext(ctx, `UPDATE templates SET spec_json=json_remove(spec_json,'$.managed_ssh'),
		capabilities_json='{"access.managed_ssh":{"supported":true}}' WHERE id='system'`); err != nil {
		t.Fatal(err)
	}
	cap, details = readManagedSSHCapability(t, s, "/v1/machines/m_managed_ssh", manage)
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

func TestManagedSSHCapabilityRequiresExactPathAndFreshExecutionEvidence(t *testing.T) {
	s, _ := managedSSHTunnelFixture(t)
	addManagedSSHNetwork(s)
	m, err := s.DB.GetMachine(context.Background(), "m_managed_ssh")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	old := now.Add(-time.Hour)
	network := rt.LaunchNetwork{NetworkID: "private", Mode: rt.NativeNetwork, RouteDomainID: "domain", CIDR: "10.42.0.0/24"}
	for _, tc := range []struct {
		name, route, domain, status, hostKey string
		power                                registry.PowerState
		observedAt                           *time.Time
		verifiedKey                          bool
	}{
		{"live key", "serve", "domain", "supported", "observed", registry.PowerRunning, &now, true},
		{"live missing key", "serve", "domain", "supported", "unavailable", registry.PowerRunning, &now, false},
		{"standby trusted key", "serve", "domain", "supported", "observed", registry.PowerStandby, &now, true},
		{"stopped stale key", "serve", "domain", "supported", "unavailable", registry.PowerStopped, &now, true},
		{"stale observation", "serve", "domain", "supported", "unknown", registry.PowerRunning, &old, true},
		{"wrong route", "other", "domain", "unknown", "unknown", registry.PowerRunning, &now, true},
		{"retargeted network", "serve", "other", "unsupported", "unknown", registry.PowerRunning, &now, true},
		{"official", "", "domain", "unsupported", "unknown", registry.PowerRunning, &now, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := *m.CurrentBinding
			b.RuntimeInstanceID = tc.route
			if tc.route == "" {
				b.RuntimeBundleID = ""
			}
			b.ObservedAt = tc.observedAt
			selection := network
			selection.RouteDomainID = tc.domain
			v := machineView{Machine: m, Binding: &b, PowerState: tc.power}
			ssh := s.managedSSHView(&v, true, &selection, tc.verifiedKey)
			if ssh.capability.Status != tc.status || ssh.constraints.HostKeyStatus != tc.hostKey || ssh.constraints.Reachability == "observed" {
				t.Fatalf("unexpected capability: %+v", ssh)
			}
		})
	}
}
