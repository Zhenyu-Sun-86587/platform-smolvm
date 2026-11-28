package platformapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/config"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/nodeproto"
	"sandbox-platform/internal/placement"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/snapshots"
)

type placementAPIRuntime struct {
	rt.Adapter
	rt.NativeLauncher
}

func TestHTTPPlacementAndWorkerMaintenance(t *testing.T) {
	ctx := context.Background()
	db, err := registry.Open(ctx, filepath.Join(t.TempDir(), "platform.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	control, err := db.Control(ctx)
	if err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("3", 64)
	bundle := "sha256:" + strings.Repeat("4", 64)
	if err = db.EnsureTemplate(ctx, registry.Template{ID: "template", Name: "System", Version: 1, ImageDigest: digest,
		Spec: json.RawMessage(`{"boot_mode":"system","root_generation_id":"legacy-local-root"}`)}); err != nil {
		t.Fatal(err)
	}

	conf := config.Defaults()
	adapters := map[rt.BundleRoute]rt.Adapter{}
	for _, id := range []string{"worker-a", "worker-b"} {
		route := rt.BundleRoute{WorkerID: id, InstanceID: "global-" + id, LocalInstanceID: "same-serve", BundleID: bundle}
		localRoot := "root-" + id
		conf.Nodes = append(conf.Nodes, config.Node{ID: id, Routes: []config.NodeRoute{{Route: nodeproto.Route{RuntimeInstanceID: route.InstanceID,
			LocalInstanceID: route.LocalInstanceID, BundleDigest: bundle}, Reader: "reader", Artifacts: map[string]string{digest: localRoot}}}})
		if err = db.RegisterWorker(ctx, registry.Worker{ID: id, Name: id}); err != nil {
			t.Fatal(err)
		}
		if err = db.EnrollWorker(ctx, registry.WorkerEnrollment{WorkerID: id, Generation: 1, CertificateFingerprint: "pin-" + id, Authority: control.ControlEpoch}); err != nil {
			t.Fatal(err)
		}
		if err = db.RegisterRuntime(ctx, route, "/run/same-serve.sock", json.RawMessage(`{"contracts":{"state_reader_id":"reader"}}`)); err != nil {
			t.Fatal(err)
		}
		if err = db.RecordArtifactPlacement(ctx, id, route.InstanceID, bundle, digest, localRoot, 1<<30); err != nil {
			t.Fatal(err)
		}
		sample := placement.Sample{WorkerID: id, ControlEpoch: control.ControlEpoch, EnrollmentGeneration: 1, Sequence: 1, Source: placement.SourceWorkerBudget, QueryNonce: "nonce-" + id, Reconciled: true,
			Session:  placement.Session{HostBootID: "host-" + id, BrokerSessionID: "broker-" + id, NodeSessionID: "node-" + id},
			Pressure: &placement.Pressure{AvailableMemoryMiB: 8192, DiskFreeBytes: 1 << 38}, Budget: placement.Budget{Capacity: placement.Resources{VCPUs: 8, MemoryMiB: 8192, StorageBytes: 1 << 38, HeavySlots: 1}},
			Profiles: []placement.RuntimeProfile{{RuntimeInstanceID: route.InstanceID, LocalInstanceID: route.LocalInstanceID, BundleID: bundle, Ready: true, RuntimeProcessID: "serve-process",
				Reader: "reader", Architecture: "x86_64", Hypervisor: "kvm", Capabilities: []string{rt.SystemBootCapability}, Artifacts: []string{digest}}}}
		if id == "worker-b" {
			sample.Budget.Capacity.HeavySlots = 2
			sample.Profiles[0].Networks = []placement.Network{{ID: "remote-only", CIDR: "10.24.0.0/24"}}
			sample.Profiles[0].Capabilities = append(sample.Profiles[0].Capabilities, rt.KernelCowLaunchCapability)
		}
		if err = db.RecordWorkerSample(ctx, sample, placement.Query{Nonce: sample.QueryNonce, StartedAt: time.Now(), CompletedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		adapters[route] = &placementAPIRuntime{}
	}
	server := New(lifecycle.New(db, rt.NewResolver(nil, adapters), conf), &snapshots.Catalog{DB: db}, nil)
	p, err := server.Auth.Bootstrap(ctx, "owner", "bounded-test-password")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	_, token, err := server.Auth.IssueTokenTx(ctx, tx, p, "admin", auth.Scope{Access: auth.AccessManage}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body, key, revision string, want int) []byte {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Host = server.ManagementHost()
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("If-Control-Epoch", control.ControlEpoch)
		r.Header.Set("Idempotency-Key", key)
		if revision != "" {
			r.Header.Set("If-Match", revision)
		}
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, r)
		if rec.Code != want {
			t.Fatalf("%s %s returned %d, want %d: %s", method, path, rec.Code, want, rec.Body.String())
		}
		return rec.Body.Bytes()
	}
	var worker workerView
	if err = json.Unmarshal(call("GET", "/v1/workers/worker-a", "", "", "", http.StatusOK), &worker); err != nil || !worker.Fresh || worker.Maintenance.Coverage != "known_only" {
		t.Fatalf("worker %+v: %v", worker, err)
	}
	t.Run("Kernel CoW placement keeps the requested profile", func(t *testing.T) {
		refused := call("POST", "/v1/machines", `{"name":"cow","template_id":"template","memory_fork_profile":"kernel_cow_anon_v1","placement":{"mode":"pinned","worker_id":"worker-a"}}`, "cow-unsupported", "", 409)
		if !strings.Contains(string(refused), "capability_unavailable") {
			t.Fatalf("missing Worker capability rejection: %s", refused)
		}
		var accepted registry.Operation
		placed := call("POST", "/v1/machines", `{"name":"cow","template_id":"template","memory_fork_profile":"kernel_cow_anon_v1","placement":{"mode":"auto"}}`, "cow-auto", "", 202)
		if err := json.Unmarshal(placed, &accepted); err != nil || accepted.WorkerID != "worker-b" {
			t.Fatalf("Kernel CoW owner %+v: %v", accepted, err)
		}
		machine, err := db.GetMachine(ctx, accepted.MachineID)
		if err != nil {
			t.Fatal(err)
		}
		var spec lifecycle.Create
		if err := json.Unmarshal(machine.Spec, &spec); err != nil || spec.MemoryForkProfile != rt.KernelCowProfile {
			t.Fatalf("accepted spec lost the RAM profile: %s: %v", machine.Spec, err)
		}
	})

	pinned := `{"name":"pinned","template_id":"template","start":true,"placement":{"mode":"pinned","worker_id":"worker-a"}}`
	if !strings.Contains(string(call("GET", "/v1/networks", "", "", "", 200)), "remote-only") {
		t.Fatal("remote network missing from catalog")
	}
	withNetwork := strings.Replace(pinned, `"start":true`, `"start":true,"network":{"mode":"native","network_id":"remote-only"}`, 1)
	call("POST", "/v1/machines", withNetwork, "network-missing", "", 409)
	call("PATCH", "/v1/workers/local-worker/maintenance", `{"mode":"cordoned","reason":"test"}`, "local-gate", `"1"`, 409)
	var op registry.Operation
	if err = json.Unmarshal(call("POST", "/v1/machines", pinned, "pin", "", 202), &op); err != nil || op.WorkerID != "worker-a" {
		t.Fatalf("pinned %+v: %v", op, err)
	}
	check, err := db.GetMachine(ctx, op.MachineID)
	if err != nil || check.WorkerID != op.WorkerID {
		t.Fatalf("machine owner %s: %v", check.WorkerID, err)
	}
	prepared, err := db.GetBinding(ctx, op.BindingID)
	if err != nil || prepared.MachineID != check.ID || prepared.WorkerID != op.WorkerID {
		t.Fatalf("accepted binding %+v: %v", prepared, err)
	}
	root, err := db.TemplateGeneration(ctx, prepared, digest)
	if err != nil || root != "root-worker-a" {
		t.Fatalf("root %q: %v", root, err)
	}
	var hold placement.Reservation
	if err = json.Unmarshal(call("GET", "/v1/operations/"+op.ID+"/placement", "", "", "", 200), &hold); err != nil || hold.WorkerID != op.WorkerID || hold.Phase != placement.Reserved {
		t.Fatalf("hold %+v: %v", hold, err)
	}
	rev := fmt.Sprintf(`"%d"`, worker.Revision)
	change := `{"mode":"cordoned","reason":"maintenance test"}`
	original := call("PATCH", "/v1/workers/worker-a/maintenance", change, "cordon", rev, 200)
	replay := call("PATCH", "/v1/workers/worker-a/maintenance", change, "cordon", rev, 200)
	if string(replay) != string(original) {
		t.Fatalf("maintenance replay differs: %s %s", original, replay)
	}
	var again registry.Operation
	if err = json.Unmarshal(call("POST", "/v1/machines", pinned, "pin", "", 202), &again); err != nil || again.ID != op.ID || again.WorkerID != op.WorkerID {
		t.Fatalf("pinned replay %+v: %v", again, err)
	}
	call("POST", "/v1/machines", strings.Replace(pinned, `"name":"pinned"`, `"name":"refused"`, 1), "cordoned-create", "", 409)
	var other registry.Operation
	if err = json.Unmarshal(call("POST", "/v1/machines", `{"name":"automatic","template_id":"template","start":true,"network":{"mode":"native","network_id":"remote-only"},"placement":{"mode":"auto"}}`, "auto", "", 202), &other); err != nil || other.WorkerID != "worker-b" {
		t.Fatalf("automatic %+v: %v", other, err)
	}
	t.Run("delegated create reserves the granted runtime", func(t *testing.T) {
		_, err := db.DB().ExecContext(ctx, `INSERT INTO machines(id,name,template_id,worker_id,revision,binding_generation,desired_power,
			spec_json,idle_policy_json,keep_running) VALUES('m_source','guest','template','worker-a',1,1,'running','{}','{}',0);
			INSERT INTO runtime_bindings(id,machine_id,generation,runtime_name,status,task_namespace,spec_hash,worker_id,runtime_instance_id,
			runtime_bundle_id,runtime_instance_object_id,incarnation_id,execution_epoch,power_state)
			VALUES('g-bind','m_source',1,'guest','current','ns','spec','worker-a','global-worker-a',?,'g-vm','g-inc',1,'running');
			UPDATE machines SET current_binding_id='g-bind' WHERE id='m_source'`, bundle)
		if err != nil {
			t.Fatal(err)
		}
		peer := auth.AttestedGuestContext{MachineID: "m_source", BindingID: "g-bind", RuntimeInstanceID: "global-worker-a",
			RuntimeBundleID: bundle, RuntimeObjectID: "g-vm", IncarnationID: "g-inc", ExecutionEpoch: 1}
		grant, err := server.Auth.CreateGuestGrant(ctx, p, auth.GuestGrantSpec{SourceMachineID: peer.MachineID,
			Action: auth.GuestCreate, MaximumUses: 1, ExpiresAt: time.Now().Add(time.Minute),
			Limits: auth.GuestLimits{MaximumChildren: 1, MaximumMemoryMiB: 512, Create: &auth.GuestCreateProfile{
				TemplateID: "template", RuntimeInstanceID: "global-worker-b", CPUs: 1, MemoryMiB: 512, NetworkID: "remote-only"}}})
		if err != nil {
			t.Fatal(err)
		}
		secret, err := server.Auth.IssueGuestCredential(ctx, p, peer, grant, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest("POST", "/v1/machines", strings.NewReader(`{"name":"guest-child"}`))
		r.Header.Set("If-Control-Epoch", control.ControlEpoch)
		r.Header.Set("Idempotency-Key", "guest-placement")
		out := httptest.NewRecorder()
		if err := server.guestCreate(out, r, peer, secret); err != nil {
			t.Fatal(err)
		}
		var op registry.Operation
		if err := json.Unmarshal(out.Body.Bytes(), &op); err != nil || op.WorkerID != "worker-b" {
			t.Fatalf("delegated operation %+v: %v", op, err)
		}
		hold, err := db.GetPlacementReservation(ctx, op.ID)
		if err != nil || hold.WorkerID != op.WorkerID || hold.Phase != placement.Reserved || hold.Resources.MemoryMiB != 512 {
			t.Fatalf("delegated Worker reservation %+v: %v", hold, err)
		}
	})
	var cordoned workerView
	if err := json.Unmarshal(call("GET", "/v1/workers/worker-a", "", "", "", 200), &cordoned); err != nil {
		t.Fatal(err)
	}
	revoke := `{"enrollment_generation":1,"reason":"scheduled rotation"}`
	call("POST", "/v1/workers/worker-a/enrollment/revoke", revoke, "revoke", fmt.Sprintf(`"%d"`, cordoned.Revision), 200)
	var revoked workerView
	if err := json.Unmarshal(call("GET", "/v1/workers/worker-a", "", "", "", 200), &revoked); err != nil || revoked.EnrollmentStatus != "revoked" || revoked.Fresh {
		t.Fatalf("revoked Worker %+v: %v", revoked, err)
	}
	if err := db.EnrollWorker(ctx, registry.WorkerEnrollment{WorkerID: "worker-a", Generation: 1, Authority: control.ControlEpoch, CertificateFingerprint: "pin-worker-a"}); err == nil {
		t.Fatal("revoked enrollment reopened by handshake")
	}

}
