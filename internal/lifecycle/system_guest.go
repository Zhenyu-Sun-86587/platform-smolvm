package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/templates"
)

const systemSSHCommand = "/usr/local/sbin/sandbox-system-ssh-sync"
const SystemSSHPort = 2222

type nativeSSHEvidence struct {
	Ready        bool                        `json:"ssh_ready"`
	Key          string                      `json:"ssh_host_key,omitempty"`
	KeysetDigest string                      `json:"ssh_keyset_digest,omitempty"`
	Execution    rt.ExecutionRef             `json:"ssh_execution"`
	FileTransfer *rt.GuestFileEvidence       `json:"file_transfer,omitempty"`
	ExecStream   *rt.GuestExecStreamEvidence `json:"exec_stream,omitempty"`
}

type nativeSSHRequest struct {
	KeysetDigest string          `json:"key_set_digest"`
	Execution    rt.ExecutionRef `json:"execution"`
}

func keysetDigest(data []byte) string { return fmt.Sprintf("sha256:%x", sha256.Sum256(data)) }

func (e *Engine) systemGuestProfile(ctx context.Context, binding registry.Binding) (templates.Spec, error) {
	machine, err := e.DB.GetMachine(ctx, binding.MachineID)
	if err != nil {
		return templates.Spec{}, err
	}
	template, err := e.DB.GetTemplate(ctx, machine.TemplateID)
	if err != nil {
		return templates.Spec{}, err
	}
	return templates.ReadSpec(template)
}

func (e *Engine) VerifiedNativeSSHKey(ctx context.Context, b registry.Binding, port uint16) (string, error) {
	if port != SystemSSHPort || b.ExecutionEpoch == 0 || b.Status != "current" || len(b.GuestManagementEvidence) == 0 {
		return "", nil
	}
	var evidence nativeSSHEvidence
	if err := json.Unmarshal(b.GuestManagementEvidence, &evidence); err != nil {
		return "", err
	}
	if !evidence.Ready || evidence.Key == "" || evidence.Execution != b.ExecutionRef() {
		return "", nil
	}
	keys, err := e.authorizedSSHKeys(ctx)
	if err != nil || evidence.KeysetDigest != keysetDigest(keys) {
		return "", err
	}
	return evidence.Key, nil
}

func (e *Engine) observeSystemGuest(ctx context.Context, binding registry.Binding, adapter rt.Adapter, x *registry.Observation) string {
	guest, ok := adapter.(rt.NativeGuestControl)
	if !ok || binding.ExecutionEpoch == 0 || x.Native.Execution != binding.ExecutionRef() || x.Native.PowerState != registry.PowerRunning {
		return ""
	}
	profile, err := e.systemGuestProfile(ctx, binding)
	if err != nil || profile.BootMode != rt.BootSystem {
		return ""
	}
	probe, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	x.GuestManagementStatus = "degraded"
	ref := binding.ExecutionRef()
	var capabilities []string
	if inspection, ok := adapter.(interface {
		GuestCapabilities(context.Context, rt.ExecutionRef) ([]string, error)
	}); ok {
		capabilities, err = inspection.GuestCapabilities(probe, ref)
	} else {
		err = guest.GuestPing(probe, ref)
	}
	if err != nil {
		return ""
	}
	x.GuestManagementStatus = "ready"
	var evidence nativeSSHEvidence
	if json.Unmarshal(binding.GuestManagementEvidence, &evidence) != nil || evidence.Execution != ref {
		evidence = nativeSSHEvidence{Execution: ref}
	}
	evidence.FileTransfer = nil
	evidence.ExecStream = nil
	if _, ok := PolicyClient(adapter, binding); ok && capabilities != nil {
		evidence.FileTransfer = &rt.GuestFileEvidence{Execution: ref, Supported: slices.Contains(capabilities, rt.SystemFileTransferCapability)}
		evidence.ExecStream = &rt.GuestExecStreamEvidence{Execution: ref, Supported: slices.Contains(capabilities, rt.SystemExecStreamCapability)}
	}
	x.GuestManagementEvidence = JSON(evidence)
	if !profile.ManagedSSH {
		return ""
	}
	keys, err := e.authorizedSSHKeys(ctx)
	if err != nil {
		return ""
	}
	digest := keysetDigest(keys)
	if evidence.KeysetDigest != digest {
		evidence.Ready = false
		x.GuestManagementEvidence = JSON(evidence)
		return digest
	}
	response, err := guest.GuestExec(probe, binding.ExecutionRef(), rt.ExecRequest{Command: []string{systemSSHCommand, "--check"}, TimeoutSeconds: 2})
	_, err = verifySSHHostKey(response, []byte(evidence.Key), err)
	evidence.Ready = err == nil
	x.GuestManagementEvidence = JSON(evidence)
	return ""
}

func (e *Engine) queueNativeSSH(ctx context.Context, binding registry.Binding, digest string) {
	ctl, err := e.DB.Control(ctx)
	if err != nil {
		return
	}
	owner, err := e.DB.OwnerID(ctx)
	if err != nil {
		return
	}
	request := JSON(nativeSSHRequest{KeysetDigest: digest, Execution: binding.ExecutionRef()})
	key := fmt.Sprintf("ssh-%s-%x-%d", binding.ID, sha256.Sum256(request), time.Now().Unix()/60)
	_, _, _ = e.DB.Accept(ctx, registry.AcceptRequest{
		PrincipalID: owner, Epoch: ctl.ControlEpoch, Key: key, Scope: "POST /internal/ssh/" + binding.MachineID,
		MachineID: binding.MachineID, ExpectedGeneration: binding.Generation, Kind: "sync_keys",
		Request: request,
		AdmitRuntime: func(route rt.BundleRoute, _ string) error {
			if route != bindingRuntime(binding).BundleRoute {
				return rt.ErrUnsupported
			}
			return nil
		},
	})
}

func (j *Job) driveNativeSSH(ctx context.Context, routeError error) error {
	var request nativeSSHRequest
	if err := json.Unmarshal(j.Op.Request, &request); err != nil {
		return err
	}
	if j.Op.State == registry.Queued {
		if err := j.Stage(ctx, "preflight", ""); err != nil {
			return err
		}
	}
	guest, canManage := j.Runtime.(rt.NativeGuestControl)
	control, canInspect := j.Runtime.(rt.NativeControl)
	if routeError != nil || !canManage || !canInspect || request.KeysetDigest == "" {
		return j.fail(ctx, rt.ErrUnsupported)
	}
	profile, err := j.systemGuestProfile(ctx, j.Binding)
	if err != nil {
		return j.fail(ctx, err)
	}
	if profile.BootMode != rt.BootSystem || !profile.ManagedSSH {
		return j.fail(ctx, rt.ErrUnsupported)
	}
	ref := j.Binding.ExecutionRef()
	if request.Execution != ref {
		return j.fail(ctx, registry.ErrStaleObservation)
	}
	inventory, err := control.NativeInventory(ctx, ref)
	if errors.Is(err, rt.ErrUnavailable) {
		return err
	}
	if err != nil || inventory.Inventory.SourceDisposition != "RUNNING" {
		return j.fail(ctx, &Failure{"STATE_CONFLICT", "managed SSH requires the current running execution"})
	}
	keys, err := j.authorizedSSHKeys(ctx)
	if err != nil {
		return j.fail(ctx, err)
	}
	if keysetDigest(keys) != request.KeysetDigest {
		return j.Done(ctx, registry.TransitionRequest{Effect: registry.EffectNone})
	}
	private, public, err := j.SSHHostKey(j.Op.MachineID)
	if err != nil {
		return j.fail(ctx, err)
	}
	if j.Intent.Stage != "native_ssh_sync" {
		if err := j.Stage(ctx, "native_ssh_sync", "running"); err != nil {
			return err
		}
	}
	key, err := installSystemSSH(ctx, ref, guest, private, public, keys)
	if err != nil {
		return j.finish(ctx, registry.Failed, registry.TransitionRequest{
			Effect: registry.EffectPartial, Error: JSON(map[string]string{"code": "SSH_IDENTITY_UNCONFIRMED", "message": "Guest SSH listener verification did not complete"}),
		})
	}
	x := nativeObservation(j.Binding, ref, registry.PowerRunning, j.Binding.CapabilityProfileID)
	x.GuestManagementStatus = "ready"
	var previous nativeSSHEvidence
	_ = json.Unmarshal(j.Binding.GuestManagementEvidence, &previous)
	x.GuestManagementEvidence = JSON(nativeSSHEvidence{Ready: true, Key: key, KeysetDigest: request.KeysetDigest, Execution: ref,
		FileTransfer: previous.FileTransfer, ExecStream: previous.ExecStream})
	return j.Done(ctx, registry.TransitionRequest{Effect: registry.EffectApplied, Observation: &x})
}
