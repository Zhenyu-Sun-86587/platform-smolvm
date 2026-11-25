package platformapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/smolvm"
	"sandbox-platform/internal/snapshots"
	"sandbox-platform/internal/tasks"
)

func (s *Server) verifySnapshotRestore(ctx context.Context, j *lifecycle.Job, snap snapshots.Snapshot) error {
	if snap.State != "ready" || snap.Content != "clean" || strings.TrimSpace(s.Config.Runtime.VerifySocket) == "" {
		return &lifecycle.Failure{Code: "CAPABILITY_NOT_VERIFIED", Message: "restore verification requires a trusted clean checkpoint and an isolated test runtime"}
	}
	mainSocket, err := os.Stat(s.Config.Runtime.Socket)
	if err != nil {
		return err
	}
	verifySocket, err := os.Stat(s.Config.Runtime.VerifySocket)
	if err != nil {
		return err
	}
	if os.SameFile(mainSocket, verifySocket) {
		return &lifecycle.Failure{Code: "CAPABILITY_NOT_VERIFIED", Message: "restore verification points to the product runtime"}
	}
	var manifest snapshots.Manifest
	if err := json.Unmarshal(snap.Manifest, &manifest); err != nil {
		return err
	}
	var capture tasks.Capture
	if err := json.Unmarshal(manifest.CaptureReceipt, &capture); err != nil || !capture.Ready ||
		capture.Content != "clean" || capture.Source.MachineID != manifest.SourceMachineID ||
		capture.Source.Generation != manifest.SourceGeneration || capture.OperationID != manifest.OperationID {
		return registry.ErrConflict
	}
	testRuntime, err := smolvm.New(s.Config.Runtime.VerifySocket)
	if err != nil {
		return err
	}
	if err := testRuntime.Ready(ctx); err != nil {
		return err
	}
	f, err := os.Open(filepath.Join(s.Snapshots.Root, snap.ID, snapshots.CheckpointFile))
	if err != nil {
		return err
	}
	defer f.Close()
	name := "verify_" + j.Op.ID[3:]
	if o, err := testRuntime.Observe(ctx, name); !errors.Is(err, rt.ErrNotFound) {
		return fmt.Errorf("verification runtime name %q must be absent before import (state %q): %w", name, o.State, registry.ErrConflict)
	}
	if err := j.Stage(ctx, "isolated_verify_import", ""); err != nil {
		return err
	}
	if _, err := testRuntime.RestoreCheckpoint(ctx, name, f, snap.Size); err != nil {
		return err
	}
	j.Written = true
	if _, err := testRuntime.SetPorts(ctx, name, []rt.Port{}); err != nil {
		return err
	}
	if _, err := testRuntime.SetLabels(ctx, name, map[string]*string{"platform.verification": &j.Op.ID}); err != nil {
		return err
	}
	observed, err := testRuntime.Observe(ctx, name)
	if err != nil {
		return err
	}
	if observed.State != rt.StateCreated || observed.ParentMachine != "" ||
		observed.Labels["platform.verification"] != j.Op.ID || !verifyBlockedNetwork(observed) {
		return errors.New("verification candidate isolation is unconfirmed")
	}
	if err := j.Stage(ctx, "isolated_verify_start", ""); err != nil {
		return err
	}
	if _, err := testRuntime.Start(ctx, name); err != nil {
		return err
	}
	if err := candidateReady(ctx, 20*time.Second, func(ctx context.Context) error {
		return checkVerificationGuest(ctx, testRuntime, name, capture)
	}); err != nil {
		return err
	}
	observed, err = testRuntime.Observe(ctx, name)
	if err != nil {
		return err
	}
	if observed.State != rt.StateRunning || !observed.Identity.Complete() ||
		observed.Labels["platform.verification"] != j.Op.ID || !verifyBlockedNetwork(observed) {
		return errors.New("running verification candidate isolation is unconfirmed")
	}
	if err := j.Stage(ctx, "isolated_verify_dispose", ""); err != nil {
		return err
	}
	if _, err := testRuntime.Stop(ctx, name); err != nil {
		return err
	}
	observed, err = testRuntime.Observe(ctx, name)
	if err != nil {
		return err
	}
	if observed.State != rt.StateStopped || observed.Labels["platform.verification"] != j.Op.ID {
		return errors.New("verification candidate did not stop")
	}
	if _, err := testRuntime.Delete(ctx, name); err != nil {
		return err
	}
	if _, err := testRuntime.Observe(ctx, name); !errors.Is(err, rt.ErrNotFound) {
		return errors.New("verification candidate absence not confirmed")
	}
	return j.Done(ctx, registry.TransitionRequest{
		Effect: "applied", Result: lifecycle.JSON(map[string]string{
			"snapshot_id": snap.ID, "verification": "restore", "verification_runtime": name,
		}),
		Apply: func(ctx context.Context, tx *sql.Tx) error {
			r, err := tx.ExecContext(ctx, "UPDATE snapshots SET restore_verified_at=?, revision=revision+1 WHERE id=? AND state='ready' AND revision=?", time.Now().UnixMilli(), snap.ID, snap.Revision)
			if err != nil {
				return err
			}
			n, err := r.RowsAffected()
			if err != nil {
				return err
			}
			if n != 1 {
				return registry.ErrRevision
			}
			_, err = tx.ExecContext(ctx, "DELETE FROM snapshot_pins WHERE snapshot_id=? AND owner_kind='verify' AND owner_id=?", snap.ID, j.Op.ID)
			return err
		},
	})
}

func verifyBlockedNetwork(o rt.Observation) bool {
	return len(o.Ports) == 0 && o.AllowedCIDRs != nil && len(o.AllowedCIDRs) == 0 &&
		o.AllowedHosts != nil && len(o.AllowedHosts) == 0 && len(o.EgressRules) == 0
}

func checkVerificationGuest(ctx context.Context, runtime *smolvm.Client, name string, capture tasks.Capture) error {
	reply, err := tasks.Call(ctx, func(ctx context.Context, argv []string, in []byte) ([]byte, error) {
		out, err := runtime.Exec(ctx, name, rt.ExecRequest{Command: argv, Stdin: string(in), TimeoutSeconds: 8})
		if err != nil {
			return nil, err
		}
		if out.ExitCode != 0 {
			return nil, errors.New("taskctl verification failed")
		}
		return out.Stdout, nil
	}, tasks.Request{Method: "hello"})
	if err != nil {
		return err
	}
	if reply.Binding == nil || *reply.Binding != capture.Source || reply.Hello == nil ||
		!reply.Hello.Cgroup || reply.Hello.Active != 0 || reply.Hello.Unknown != 0 ||
		reply.Hello.Barrier == nil || !reflect.DeepEqual(*reply.Hello.Barrier, capture) {
		return errors.New("restored Guest did not reproduce its signed clean capture evidence")
	}
	return nil
}
