package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/workerclient"
)

type policyCall func(context.Context, string, string, workerclient.Execution, any, any) error

func TestAccessWaitsForTheOriginalControlReceipt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ref := workerclient.Execution{InstanceID: "owned-slot", IncarnationID: "owned-execution", Epoch: 1}
	expected := map[string]any{"protection_id": "owned-access", "expected_binding_revision": 1}
	calls := 0
	client := accessPolicyWorker{policyCall(func(_ context.Context, method, key string, actual workerclient.Execution, input, result any) error {
		calls++
		raw, err := json.Marshal(input)
		if err != nil || method != "RequestWake" || key != "owned-access-admit" || actual != ref ||
			string(raw) != `{"expected_binding_revision":1,"protection_id":"owned-access"}` {
			t.Fatalf("unexpected control request on attempt %d: %s %s %+v %s", calls, method, key, actual, raw)
		}
		if calls == 1 {
			return context.DeadlineExceeded
		}
		*(result.(*string)) = "original-receipt"
		return nil
	})}
	var receipt string
	if err := client.Lifecycle(ctx, "RequestWake", "owned-access-admit", ref, expected, &receipt); err != nil || calls != 2 || receipt != "original-receipt" {
		t.Fatalf("receipt %q after %d requests: %v", receipt, calls, err)
	}
}

func (call policyCall) Lifecycle(ctx context.Context, method, id string, ref workerclient.Execution, request, result any) error {
	return call(ctx, method, id, ref, request, result)
}

func TestPolicyBindingReplacementResumesHistoricalWorkerWithoutApplyingSupersededPolicy(t *testing.T) {
	ctx := context.Background()
	db, err := registry.Open(ctx, filepath.Join(t.TempDir(), "platform.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	policy := rt.DefaultLifecyclePolicy()
	policy.KeepRunning = true
	old := registry.Binding{ID: "previous", MachineID: "m", RuntimeInstanceObjectID: "old-instance", IncarnationID: "observed", ExecutionEpoch: 2}
	fresh := registry.Binding{ID: "replacement", MachineID: "m", RuntimeInstanceObjectID: "fresh-instance", IncarnationID: "boot", ExecutionEpoch: 1}
	serialized := string(JSON(policy))
	oldScope := WorkerExecution(old)
	oldScope.IncarnationID, oldScope.Epoch = "original", 1
	_, err = db.DB().ExecContext(ctx, `
		INSERT INTO templates(id,name,version,image_digest,spec_json,capabilities_json) VALUES('t','t',1,'image','{}','{}');
		INSERT INTO machines(id,name,template_id,revision,binding_generation,desired_power,spec_json,idle_policy_json,keep_running) VALUES('m','m','t',1,1,'running','{}','{}',1);
		INSERT INTO runtime_bindings(id,machine_id,generation,runtime_name,status,task_namespace,spec_hash,observed_state)
			VALUES('previous','m',1,'old','current','old-ns','old-hash','running'),('replacement','m',2,'new','candidate','new-ns','new-hash','running');
		UPDATE machines SET current_binding_id='previous' WHERE id='m';
		INSERT INTO machine_lifecycle_policies(machine_id,desired_revision,applied_revision,desired_json,source,last_operation) VALUES('m',4,3,?,'override','edit');
		INSERT INTO lifecycle_deliveries(id,machine_id,binding_id,desired_revision,policy_json,expected_json,previous_revision) VALUES('policy-m-4','m','previous',4,?,?,3);
		INSERT INTO lifecycle_outbox_cursors VALUES('m',79);
		INSERT INTO lifecycle_lease_intents(id,lease_id,machine_id,binding_id,method,principal,credential,scope_json,request_json,secret,lease_revision,created_at)
			VALUES('intent','lease','m','previous','AcquireRunningLease','p','c','{}','{}',x'12',1,1)`, serialized, serialized, string(JSON(oldScope)))
	if err != nil {
		t.Fatal(err)
	}
	e := Engine{DB: db}
	previous := policyCall(func(_ context.Context, method, id string, ref workerclient.Execution, payload, out any) error {
		if method != "ApplyLifecyclePolicy" || id != "policy-m-4" || ref != oldScope || payload.(map[string]any)["expected_applied_revision"] != int64(3) {
			t.Fatalf("historical request changed: %s %s %+v", method, id, ref)
		}
		*out.(*policyReceipt) = policyReceipt{Instance: ref.InstanceID, Applied: 4, Effective: &policy}
		return nil
	})
	if p, err := e.ApplyMachinePolicy(ctx, old, previous); err != nil || p.AppliedRevision != 4 {
		t.Fatalf("old receipt: %+v %v", p, err)
	}
	if _, err = db.DB().ExecContext(ctx, `UPDATE runtime_bindings SET status='retired' WHERE id='previous';
		UPDATE runtime_bindings SET status='current' WHERE id='replacement';
		UPDATE machines SET current_binding_id='replacement',binding_generation=2 WHERE id='m'`); err != nil {
		t.Fatal(err)
	}
	p, err := db.LifecyclePolicy(ctx, "m")
	if err != nil || p.AppliedRevision != 0 || p.Applied != nil || p.DesiredRevision != 4 || p.Desired != policy {
		t.Fatalf("replaced projection: %+v %v", p, err)
	}
	var cursorCount, secretSize int
	var state, code string
	if err = db.ReadDB().QueryRowContext(ctx, `SELECT (SELECT count(*) FROM lifecycle_outbox_cursors),state,length(secret),json_extract(receipt_json,'$.code')
		FROM lifecycle_lease_intents WHERE id='intent'`).Scan(&cursorCount, &state, &secretSize, &code); err != nil ||
		cursorCount != 0 || state != "rejected" || secretSize != 0 || code != "E_STALE_EXECUTION" {
		t.Fatalf("replaced cursor and lease: %d %s %d %s %v", cursorCount, state, secretSize, code, err)
	}
	if _, err = e.ApplyMachinePolicy(ctx, old, previous); !errors.Is(err, registry.ErrGeneration) {
		t.Fatalf("stale binding %v", err)
	}
	var revision int64
	historical := policyCall(func(_ context.Context, method, id string, ref workerclient.Execution, payload, out any) error {
		if ref != WorkerExecution(fresh) {
			t.Fatalf("wrong replacement %+v", ref)
		}
		ack := out.(*policyReceipt)
		if method == "GetLifecycleStatus" {
			if revision == 0 {
				return &workerclient.Fault{Code: "E_NOT_FOUND"}
			}
			*ack = policyReceipt{Instance: ref.InstanceID, Applied: revision, Effective: &policy}
			return nil
		}
		body := payload.(map[string]any)
		if body["initialize_replacement"] == true {
			return &workerclient.Fault{Code: "E_INVALID_REQUEST"}
		}
		var received rt.LifecyclePolicy
		if err := json.Unmarshal(body["effective_policy"].(json.RawMessage), &received); err != nil || received != policy {
			t.Fatalf("intermediate snapshot %+v %v", received, err)
		}
		if body["expected_applied_revision"] != revision || body["new_revision"] != revision+1 || id != fmt.Sprintf("binding-policy-replacement-4-sync-%d", revision+1) {
			t.Fatalf("wrong replay %s at %d: %+v", id, revision, body)
		}
		revision++
		if revision == 2 {
			return context.DeadlineExceeded // The Worker committed the update; its response was lost.
		}
		*ack = policyReceipt{Instance: ref.InstanceID, Applied: revision, Effective: &policy}
		return nil
	})
	if _, err = e.ApplyMachinePolicy(ctx, fresh, historical); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lost acknowledgement %v", err)
	}
	p, err = e.ApplyMachinePolicy(ctx, fresh, historical)
	if err != nil || p.AppliedRevision != 4 || p.Applied == nil || *p.Applied != policy || revision != 4 || p.LastError != nil {
		t.Fatalf("replacement delivery: %+v %v", p, err)
	}
}
