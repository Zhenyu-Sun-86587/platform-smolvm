package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"sandbox-platform/internal/config"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/workerclient"
)

type maintenanceRuleRuntime struct {
	rt.Adapter
	workerclient.BindingControl
	enabled  bool
	revision int64
	lastKey  string
	drop     bool
}

func (a *maintenanceRuleRuntime) PacketWakeRule(_ context.Context, target workerclient.Execution, r workerclient.PacketWakeRule, key, _ string) (json.RawMessage, error) {
	if target.InstanceID != "slot-target" {
		return nil, rt.ErrInvalid
	}
	if r.Enabled != nil && key != a.lastKey {
		if r.ExpectedRevision == nil || *r.ExpectedRevision != a.revision {
			return nil, registry.ErrRevision
		}
		if *r.Enabled && (r.SourceExecution == nil || r.SourceExecution.InstanceID != "slot-source" || r.SourceExecution.IncarnationID != "inc-source" || r.SourceExecution.Epoch != 1) {
			return nil, registry.ErrGeneration
		}
		a.enabled = *r.Enabled
		a.revision++
		a.lastKey = key
		if a.drop {
			a.drop = false
			return nil, rt.ErrUncertain
		}
	}
	return json.Marshal(packetRuleReceipt{ID: "rule", Revision: a.revision, Enabled: &a.enabled, Source: r.SourceAttachmentID, Target: r.TargetAttachmentID,
		Transport: r.Transport, Port: r.GuestPort, SourceIncarnation: "inc-source", SourceEpoch: 1, SourceCurrent: a.enabled})
}

func TestDrainPacketRuleLostReceiptAndReactivation(t *testing.T) {
	for _, operatorDisable := range []bool{false, true} {
		name := "restore_previous_rule"
		if operatorDisable {
			name = "preserve_operator_disable"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			db, err := registry.Open(ctx, filepath.Join(t.TempDir(), "registry.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			route := rt.BundleRoute{WorkerID: "worker-a", InstanceID: "global", LocalInstanceID: "serve", BundleID: "bundle"}
			if err = db.RegisterWorker(ctx, registry.Worker{ID: route.WorkerID, Name: "Worker A"}); err != nil {
				t.Fatal(err)
			}
			if err = db.RegisterRuntime(ctx, route, "socket", json.RawMessage(`{"contracts":{"state_reader_id":"reader"}}`)); err != nil {
				t.Fatal(err)
			}
			if err = db.EnsureTemplate(ctx, registry.Template{ID: "t", Name: "template", Version: 1, ImageDigest: "image"}); err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"source", "target"} {
				_, err = db.DB().ExecContext(ctx, `INSERT INTO machines(id,name,template_id,revision,binding_generation,desired_power,spec_json,idle_policy_json,keep_running,worker_id)
					VALUES(?,?,'t',1,1,'running','{}','{}',0,'worker-a')`, id, id)
				if err != nil {
					t.Fatal(err)
				}
				_, err = db.DB().ExecContext(ctx, `INSERT INTO runtime_bindings(id,machine_id,generation,runtime_name,status,task_namespace,spec_hash,observed_state,worker_id,runtime_instance_id,
					runtime_bundle_id,runtime_instance_object_id,incarnation_id,execution_epoch) VALUES(?,?,1,?,'current',?,'hash','running','worker-a','global','bundle',?,?,1)`, id, id, id, "ns-"+id, "slot-"+id, "inc-"+id)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = db.DB().ExecContext(ctx, `UPDATE machines SET current_binding_id=? WHERE id=?`, id, id); err != nil {
					t.Fatal(err)
				}
			}
			rule := registry.WorkerPacketWakeRule{BindingID: "target", SourceBindingID: "source", WorkerID: "worker-a", SourceAttachmentID: "src-attachment", TargetAttachmentID: "dst-attachment", Transport: "tcp", Port: 443}
			if err = db.TrackWorkerPacketWakeRule(ctx, rule, true); err != nil {
				t.Fatal(err)
			}
			node := &maintenanceRuleRuntime{enabled: true, revision: 1, drop: true}
			engine := New(db, rt.NewResolver(nil, map[rt.BundleRoute]rt.Adapter{route: node}), config.Defaults())
			m, err := db.GetWorkerMaintenance(ctx, route.WorkerID)
			if err != nil {
				t.Fatal(err)
			}
			m, err = db.SetWorkerMaintenance(ctx, route.WorkerID, m.Revision, "cordoned", "test")
			if err != nil {
				t.Fatal(err)
			}
			m, err = db.SetWorkerMaintenance(ctx, route.WorkerID, m.Revision, "draining", "test")
			if err != nil {
				t.Fatal(err)
			}
			known := func() registry.WorkerPacketWakeRule {
				t.Helper()
				r, err := db.ListWorkerPacketWakeRules(ctx, route.WorkerID)
				if err != nil || len(r) != 1 {
					t.Fatalf("rules=%v: %v", r, err)
				}
				return r[0]
			}
			if _, err = engine.disableManagedPacketWakeRule(ctx, known(), m.Revision); !errors.Is(err, rt.ErrUncertain) {
				t.Fatalf("lost reply: %v", err)
			}
			observed, err := engine.disableManagedPacketWakeRule(ctx, known(), m.Revision)
			if err != nil || observed.ID != "rule" || observed.Enabled || observed.Revision != 2 {
				t.Fatalf("recovery %+v: %v", observed, err)
			}
			if operatorDisable {
				if err = db.TrackWorkerPacketWakeRule(ctx, rule, false); err != nil {
					t.Fatal(err)
				}
			}
			m, err = db.SetWorkerMaintenance(ctx, route.WorkerID, m.Revision, "active", "test")
			if err != nil {
				t.Fatal(err)
			}
			pending, err := engine.RestoreWorkerPacketRules(ctx, route.WorkerID, m.Revision)
			if err != nil || len(pending) != 0 || node.enabled == operatorDisable || known().Overlay != nil {
				t.Fatalf("restored=%v pending=%v: %v", node.enabled, pending, err)
			}
		})
	}
}
