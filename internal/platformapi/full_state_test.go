package platformapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/config"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/snapshots"
)

func TestFullStateAdmissionPreservesLegacyAndRefusesBeforeWrite(t *testing.T) {
	ctx := context.Background()
	db, err := registry.Open(ctx, filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctl, err := db.Control(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e := lifecycle.New(db, rt.NewResolver(nil, nil), config.Defaults())
	s := New(e, &snapshots.Catalog{DB: db}, nil)
	p, err := s.Auth.Bootstrap(ctx, "owner", "a-test-password")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	_, token, err := s.Auth.IssueTokenTx(ctx, tx, p, "test", auth.Scope{Access: auth.AccessManage}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureTemplate(ctx, registry.Template{ID: "template", Name: "base", Version: 1, ImageDigest: "sha256:image",
		Capabilities: json.RawMessage(`{"state.hibernate":{"supported":true}}`)}); err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureTemplate(ctx, registry.Template{ID: "system-template", Name: "System", Version: 1,
		ImageDigest: "sha256:" + strings.Repeat("1", 64), Spec: json.RawMessage(`{"boot_mode":"system","root_generation_id":"debian-root"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := db.RegisterRuntime(ctx, rt.BundleRoute{InstanceID: "serve", BundleID: "bundle"}, "isolated-socket",
		json.RawMessage(`{"contracts":{"state_reader_id":"reader"},"guest":{"boot_profiles":["oci-v1"]}}`)); err != nil {
		t.Fatal(err)
	}
	_, err = db.DB().ExecContext(ctx, `
		INSERT INTO machines(id,name,template_id,revision,binding_generation,desired_power,spec_json,idle_policy_json,keep_running)
		VALUES ('legacy','legacy','template',1,1,'running','{}','{}',0), ('deep','deep','template',1,1,'running','{}','{}',0);
		INSERT INTO runtime_bindings(id,machine_id,generation,runtime_name,status,task_namespace,spec_hash,observed_state,runtime_instance_id,runtime_bundle_id)
		VALUES ('b_legacy','legacy',1,'legacy','current','ns_legacy','digest','running',NULL,NULL),
		       ('b_deep','deep',1,'deep','current','ns_deep','digest','running','serve','bundle');
		UPDATE machines SET current_binding_id='b_'||id;`)
	if err != nil {
		t.Fatal(err)
	}
	post := func(path, body, key string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("If-Match", `"1"`)
		r.Header.Set("If-Control-Epoch", ctl.ControlEpoch)
		r.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, r)
		return w
	}
	for _, testcase := range []struct{ body, code string }{
		{`{"name":"system","template_id":"system-template","start":true}`, "CAPABILITY_UNSUPPORTED"},
		{`{"name":"system","template_id":"system-template","runtime_instance_id":"serve","start":true}`, "CAPABILITY_UNKNOWN"},
		{`{"name":"system","template_id":"system-template","runtime_instance_id":"serve","boot_mode":"oci","start":true}`, "INVALID_ARGUMENT"},
	} {
		w := post("/v1/machines", testcase.body, "unverified-system")
		var response struct{ Error map[string]string }
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || response.Error["code"] != testcase.code || response.Error["effect"] != "none" {
			t.Fatalf("System create = %d %s, %v", w.Code, w.Body.String(), err)
		}
	}
	for _, tc := range []struct{ path, capability, body string }{
		{"standby", "lifecycle.standby", `{"expected_generation":1}`},
		{"hibernate", "state.hibernate", `{"expected_generation":1,"activity_policy":{"allow_active_tasks":true}}`},
		{"forks", "state.independent_fork", `{"expected_generation":1,"child_name":"child","require_independent":true}`},
		{"fork-batches", "state.fork_batch", `{"expected_generation":1,"children":[{"name":"child"}],"require_independent":true}`},
		{"snapshots", "state.checkpoint", `{"expected_generation":1,"capture_mode":"vm"}`},
	} {
		for _, lane := range []struct{ id, code, reason string }{
			{"legacy", "CAPABILITY_UNSUPPORTED", "RUNTIME_EXT_REQUIRED"},
			{"deep", "CAPABILITY_UNKNOWN", "CAPABILITY_NOT_VERIFIED"},
		} {
			w := post("/v1/machines/"+lane.id+"/"+tc.path, tc.body, "test-"+lane.id+"-"+tc.path)
			var response struct{ Error map[string]string }
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || w.Code != http.StatusConflict ||
				response.Error["code"] != lane.code || response.Error["reason_code"] != lane.reason ||
				response.Error["capability"] != tc.capability || response.Error["effect"] != "none" {
				t.Fatalf("%s/%s = %d %s, %v", lane.id, tc.path, w.Code, w.Body.String(), err)
			}
		}
	}
	var operations, children, idempotency, machines, revisions int
	err = db.ReadDB().QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM operations),
		(SELECT COUNT(*) FROM operation_children), (SELECT COUNT(*) FROM idempotency),
		(SELECT COUNT(*) FROM machines), (SELECT SUM(revision) FROM machines)`).Scan(&operations, &children, &idempotency, &machines, &revisions)
	if err != nil || operations != 0 || children != 0 || idempotency != 0 || machines != 2 || revisions != 2 {
		t.Fatal(operations, children, idempotency, machines, revisions, err)
	}
	var accepted string
	for _, body := range []string{`{"purpose":"recovery","expected_generation":1}`, `{"purpose":"recovery","expected_generation":1,"capture_mode":"managed_tasks"}`} {
		w := post("/v1/machines/legacy/snapshots", body, "legacy-capture")
		var op registry.Operation
		if err := json.Unmarshal(w.Body.Bytes(), &op); err != nil || w.Code != http.StatusAccepted || op.Kind != "snapshot" ||
			op.ID == "" || (accepted != "" && op.ID != accepted) {
			t.Fatalf("legacy managed-task snapshot = %d %s", w.Code, w.Body.String())
		}
		accepted = op.ID
	}

	// Exercise the ledger with persisted Runtime receipts independently of live dispatch.
	_, err = db.DB().ExecContext(ctx, `
		INSERT INTO machines(id,name,template_id,revision,binding_generation,desired_power,spec_json,idle_policy_json,keep_running)
		VALUES ('first','first','template',1,1,'running','{}','{}',0), ('second','second','template',1,1,'running','{}','{}',0);
		INSERT INTO runtime_bindings(id,machine_id,generation,runtime_name,status,task_namespace,spec_hash,runtime_instance_id,runtime_bundle_id)
		VALUES ('b_first','first',1,'first','candidate','ns_first','digest','serve','bundle'),
		       ('b_second','second',1,'second','unknown','ns_second','digest','serve','bundle');
		INSERT INTO operations(id,control_epoch,machine_id,binding_id,binding_generation,runtime_name,
			kind,state,stage,request_json,effect,created_at,updated_at,heavy,source_disposition)
		VALUES ('batch',?,'deep','b_deep',1,'deep','fork_batch','observing','children_identity','{}','partial',1,1,1,'STANDBY'),
		       ('capture',?,'deep','b_deep',1,'deep','vm_snapshot','succeeded','committed','{}','applied',1,1,1,'RUNNING');
		INSERT INTO operation_children(operation_id,ordinal,binding_id,state,resource_independence,error_code)
		VALUES ('batch',1,'b_second','quarantined','failed','NETWORK_IDENTITY_FAILED'),('batch',0,'b_first','ready','verified',NULL);
		INSERT INTO snapshots(id,source_machine_id,source_generation,purpose,task_content,state,relative_path,provenance,created_at)
		VALUES ('vm_snapshot','deep',1,'recovery','active','ready','vm_snapshot','local_record',1)`, ctl.ControlEpoch, ctl.ControlEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `UPDATE operation_children SET ordinal=2 WHERE operation_id='batch' AND ordinal=0`); err == nil {
		t.Fatal("changed a persisted child ordinal")
	}
	batch, err := db.GetOperation(ctx, "batch")
	if err != nil || batch.SourceDisposition != registry.SourceStandby || batch.Effect != registry.EffectPartial || len(batch.Children) != 2 ||
		batch.Children[0].MachineID != "first" || batch.Children[1].ErrorCode != "NETWORK_IDENTITY_FAILED" {
		t.Fatal(batch, err)
	}
	const addProvenance = `INSERT INTO vm_snapshot_provenance VALUES ('vm_snapshot','capture','serve','bundle',?,'capture-evidence')`
	if _, err := db.DB().ExecContext(ctx, addProvenance, "different-reader"); err == nil {
		t.Fatal("accepted a reader that differs from the source Bundle")
	}
	if _, err := db.DB().ExecContext(ctx, addProvenance, "reader"); err != nil {
		t.Fatal(err)
	}
	var pinnedBundle string
	if err := db.ReadDB().QueryRowContext(ctx, `SELECT bundle_id FROM runtime_bundle_refs WHERE owner_kind='snapshot' AND owner_id='vm_snapshot'`).Scan(&pinnedBundle); err != nil || pinnedBundle != "bundle" {
		t.Fatal(pinnedBundle, err)
	}
	if _, err := db.DB().ExecContext(ctx, `UPDATE runtime_bindings SET power_state='standby' WHERE id='b_deep'`); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/runtime-profiles", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	profileResponse := httptest.NewRecorder()
	s.mux.ServeHTTP(profileResponse, req)
	var profiles struct{ Items []registry.RuntimeProfile }
	if err := json.Unmarshal(profileResponse.Body.Bytes(), &profiles); err != nil || profileResponse.Code != http.StatusOK ||
		len(profiles.Items) != 1 || strings.Contains(profileResponse.Body.String(), "isolated-socket") {
		t.Fatalf("runtime profiles = %d %s, %v", profileResponse.Code, profileResponse.Body.String(), err)
	}
	profile := profiles.Items[0]
	if profile.RuntimeInstanceID != "serve" || profile.RuntimeBundleID != "bundle" || profile.StateReaderID != "reader" ||
		profile.StandbyBindings != 1 || profile.BundleSnapshotRefs != 1 || len(profile.BootProfiles) != 1 ||
		profile.BootProfiles[0] != "oci-v1" || profile.Capabilities["state.hibernate"].Status != "unknown" {
		t.Fatalf("inaccurate runtime profile %+v", profile)
	}
	snapshot, err := s.Snapshots.Get(ctx, "vm_snapshot")
	if err != nil || snapshot.CaptureMode != snapshots.CaptureVM || snapshot.VM == nil ||
		snapshot.VM.StateReaderID != "reader" || snapshot.VM.RuntimeBundleID != "bundle" {
		t.Fatal(snapshot, err)
	}
	w := post("/v1/restores", `{"snapshot_id":"vm_snapshot","name":"restored"}`, "vm-restore")
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"code":"CAPABILITY_UNKNOWN"`) {
		t.Fatalf("VM restore = %d %s", w.Code, w.Body.String())
	}
}

func TestOfficialPowerStateRequiresPauseEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, state, pauseID, status, reason string
		power                                registry.PowerState
	}{
		{"created", "created", "", "", "", registry.PowerStopped},
		{"confirmed pause", "paused", "saved", "active", `""`, registry.PowerHibernated},
		{"pressure pause", "paused", "saved", "active", `"pressurePaused"`, registry.PowerUnknown},
		{"missing artifact", "paused", "", "active", `""`, registry.PowerUnknown},
		{"consumed artifact", "paused", "saved", "consumed", `""`, registry.PowerUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binding := registry.Binding{Observation: registry.Observation{
				ObservedState: tc.state, RuntimePauseID: tc.pauseID, PauseStatus: tc.status, StateReason: json.RawMessage(tc.reason),
			}}
			if state := officialPowerState(binding); state != tc.power {
				t.Fatalf("observed power state is %q, want %q", state, tc.power)
			}
		})
	}
}
