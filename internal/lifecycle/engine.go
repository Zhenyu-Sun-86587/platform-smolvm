// Package lifecycle owns dispatch and observation of durable operations.
package lifecycle

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"syscall"
	"time"

	"sandbox-platform/internal/config"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/workerclient"
	"sandbox-platform/internal/tasks"
	"sandbox-platform/internal/templates"
)

type Engine struct {
	ReconcileWorkerLeases func(context.Context, registry.Binding, workerclient.Client)
	collectorSession      string
	DB                    *registry.Store
	runtimes              *rt.Resolver
	Config                config.Config
	mu                    sync.Mutex
	running               map[string]bool
	holds                 map[string]int
	waiters               map[string]int
	activeAccess          int
	workers               sync.WaitGroup
	Recover               map[string]func(context.Context, *Job) error
	Extra                 map[string]func(context.Context, *Job) error
}

func New(db *registry.Store, r *rt.Resolver, c config.Config) *Engine {
	return &Engine{DB: db, runtimes: r, Config: c, running: map[string]bool{}, holds: map[string]int{}, waiters: map[string]int{}, Recover: map[string]func(context.Context, *Job) error{}, Extra: map[string]func(context.Context, *Job) error{}}
}

func (e *Engine) OfficialRuntime() rt.Adapter { return e.runtimes.Official() }

func bindingRuntime(b registry.Binding) rt.BindingRuntime {
	return rt.BindingRuntime{
		BundleRoute:   rt.BundleRoute{InstanceID: b.RuntimeInstanceID, BundleID: b.RuntimeBundleID},
		ObjectID:      b.RuntimeInstanceObjectID,
		IncarnationID: b.IncarnationID,
		Epoch:         b.ExecutionEpoch,
	}
}

func (e *Engine) ForBinding(b registry.Binding) (rt.Adapter, error) {
	if e.runtimes == nil {
		return nil, errors.New("runtime resolver unavailable")
	}
	return e.runtimes.ForBinding(bindingRuntime(b))
}

func (e *Engine) AdmitRuntime(route rt.BundleRoute, kind string) error {
	return e.runtimes.Admit(route, kind)
}

type Create struct {
	LifecyclePolicy   json.RawMessage   `json:"lifecycle_policy,omitempty"`
	Name              string            `json:"name"`
	TemplateID        string            `json:"template_id"`
	RuntimeInstanceID string            `json:"runtime_instance_id,omitempty"`
	BootMode          rt.BootMode       `json:"boot_mode,omitempty"`
	Network           *rt.LaunchNetwork `json:"network,omitempty"`
	Resources         struct {
		CPUs    uint8  `json:"cpu_count"`
		Memory  uint32 `json:"memory_mib"`
		Storage uint64 `json:"storage_gib,omitempty"`
		Overlay uint64 `json:"overlay_gib,omitempty"`
	} `json:"resources"`
	IdlePolicy *IdlePolicy `json:"idle_policy,omitempty"`
	Start      bool        `json:"start"`
}
type IdlePolicy struct {
	Enabled bool `json:"enabled"`
	Timeout int  `json:"timeout_seconds"`
}
type Action struct {
	ExpectedGeneration int64  `json:"expected_generation"`
	Reason             string `json:"reason,omitempty"`
	AllowActive        bool   `json:"allow_active_tasks,omitempty"`
	CancelTasks        bool   `json:"cancel_tasks,omitempty"`
}
type Intent struct {
	Native          *rt.NativeSubmission     `json:"native,omitempty"`
	Launch          *rt.LaunchSubmission     `json:"launch,omitempty"`
	Volume          *rt.VolumeMutation       `json:"volume,omitempty"`
	Fork            *nativeForkIntent        `json:"fork,omitempty"`
	StatePoint      *rt.StatePointSubmission `json:"statepoint,omitempty"`
	Stage           string                   `json:"stage"`
	Name            string                   `json:"runtime_name"`
	Source          string                   `json:"source,omitempty"`
	Target          string                   `json:"expected_state,omitempty"`
	PauseID         string                   `json:"pause_id,omitempty"`
	BindingID       string                   `json:"binding_id"`
	WorkloadFailure *workloadStartFailure    `json:"workload_failure,omitempty"`
}

type workloadStartFailure struct {
	Identity *rt.Identity `json:"identity"`
	Message  string       `json:"message"`
}

func JSON(v any) json.RawMessage {
	b, e := json.Marshal(v)
	if e != nil {
		panic(e)
	}
	return b
}
func Evidence(o rt.Observation) registry.Observation {
	status := "not_ready"
	if o.State == "running" && o.Identity.Complete() {
		status = "ready"
	}
	x := registry.Observation{ObservedState: o.State, NativeState: o.RawState, Availability: status, Ports: JSON(o.Ports), StateReason: JSON(o.StateReason), ObservedAt: &o.ObservedAt}
	if o.Identity != nil {
		x.RuntimeIdentity = JSON(o.Identity)
	}
	return x
}
func GuestBinding(b registry.Binding) *tasks.Binding {
	return &tasks.Binding{MachineID: b.MachineID, Generation: b.Generation, Namespace: b.TaskNamespace, Nonce: b.ID}
}
func (e *Engine) Call(ctx context.Context, b registry.Binding, req tasks.Request) (tasks.Response, error) {
	adapter, err := e.ForBinding(b)
	if err != nil {
		return tasks.Response{}, err
	}
	if req.Binding == nil {
		req.Binding = GuestBinding(b)
	}
	return tasks.Call(ctx, func(ctx context.Context, argv []string, in []byte) ([]byte, error) {
		res, err := adapter.Exec(ctx, b.RuntimeName, rt.ExecRequest{Command: argv, Stdin: string(in), TimeoutSeconds: 30})
		if err != nil {
			return nil, err
		}
		if res.ExitCode != 0 {
			return nil, fmt.Errorf("taskctl exited %d", res.ExitCode)
		}
		return res.Stdout, nil
	}, req)
}
func readyRuntime(ctx context.Context, adapter rt.Adapter) error {
	_, err := adapter.Health(ctx)
	if err != nil {
		return err
	}
	return adapter.Ready(ctx)
}

// Run keeps work independent of request lifetimes. Freshly dispatched calls are
// observed after a crash; a dispatch intent is never used as permission to retry.
func (e *Engine) Run(ctx context.Context) {
	tick := time.NewTicker(400 * time.Millisecond)
	defer tick.Stop()
	defer e.workers.Wait()
	e.workers.Add(2)
	go func() { defer e.workers.Done(); e.monitor(ctx) }()
	go func() { defer e.workers.Done(); e.recoverConnections(ctx) }()
	for {
		if ctx.Err() != nil {
			return
		}
		e.wakeSuccessors(ctx)
		ops, err := e.DB.ListPending(ctx)
		if err != nil {
			slog.Error("read pending operations", "error", err)
		} else {
			for _, op := range ops {
				if op.State == registry.RecoveryRequired {
					continue
				}
				e.mu.Lock()
				active := e.running[op.ID]
				if !active {
					e.running[op.ID] = true
				}
				e.mu.Unlock()
				if active {
					continue
				}
				e.workers.Add(1)
				go func(op registry.Operation) {
					defer e.workers.Done()
					defer func() { e.mu.Lock(); delete(e.running, op.ID); e.mu.Unlock() }()
					if err := e.drive(ctx, op); err != nil && ctx.Err() == nil && !errors.Is(err, registry.ErrHeavyBusy) && !errors.Is(err, registry.ErrOperationState) {
						slog.Debug("operation awaiting reconciliation", "operation_id", op.ID, "error", err)
					}
				}(op)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

type Job struct {
	*Engine
	Runtime rt.Adapter
	Op      registry.Operation
	Binding registry.Binding
	Intent  Intent
	Written bool
}

func (j *Job) transition(ctx context.Context, r registry.TransitionRequest) error {
	r.ExpectedVersion = j.Op.Version
	op, err := j.DB.Transition(ctx, j.Op.ID, r)
	if err == nil {
		j.Op = op
	}
	return err
}
func (j *Job) Stage(ctx context.Context, stage, target string) error {
	return j.StageApply(ctx, stage, target, nil)
}

// StageApply commits related state and the outgoing intent before the RPC.
func (j *Job) StageApply(ctx context.Context, stage, target string, apply func(context.Context, *sql.Tx) error) error {
	j.Intent.Stage = stage
	j.Intent.Target = target
	state := registry.Dispatching
	if j.Op.State == registry.Observing {
		state = registry.Observing
	}
	return j.transition(ctx, registry.TransitionRequest{State: state, Stage: stage, Dispatch: JSON(j.Intent), MaxHeavy: j.Config.Lifecycle.Heavy, Apply: apply})
}
func (j *Job) Done(ctx context.Context, r registry.TransitionRequest) error {
	return j.finish(ctx, registry.Succeeded, r)
}

func (j *Job) finish(ctx context.Context, state string, r registry.TransitionRequest) error {
	if j.Op.State != registry.Observing {
		if err := j.transition(ctx, registry.TransitionRequest{State: registry.Observing, Stage: "verify", Effect: r.Effect}); err != nil {
			return err
		}
	}
	r.State = state
	r.Stage = "completed"
	if r.Result == nil {
		r.Result = JSON(map[string]string{"machine_id": j.Op.MachineID})
	}
	return j.transition(ctx, r)
}
func (j *Job) fail(ctx context.Context, err error) error {
	code := "PRECONDITION_FAILED"
	var re *rt.Error
	if errors.As(err, &re) {
		code = re.Code
	}
	var capability *rt.CapabilityError
	if errors.As(err, &capability) {
		code = "CAPABILITY_UNKNOWN"
		if capability.Status.Status == "unsupported" {
			code = "CAPABILITY_UNSUPPORTED"
		}
	}
	var ge *tasks.RPCError
	if errors.As(err, &ge) {
		code = ge.Code
	}
	var pe *Failure
	if errors.As(err, &pe) {
		code = pe.Code
	}
	unknown := errors.Is(err, rt.ErrUncertain) || j.Written
	effect := registry.EffectNone
	detail := JSON(map[string]string{"code": code, "message": err.Error()})
	if unknown {
		return j.transition(ctx, registry.TransitionRequest{State: registry.RecoveryRequired, Stage: "verify", Effect: registry.EffectUnknown, Error: detail})
	}
	if j.Op.State != registry.Observing {
		if er := j.transition(ctx, registry.TransitionRequest{State: registry.Observing, Stage: "verify"}); er != nil {
			return er
		}
	}
	return j.transition(ctx, registry.TransitionRequest{State: registry.Failed, Stage: "completed", Effect: effect, Error: detail})
}

type Failure struct{ Code, Message string }

func (e *Failure) Error() string { return e.Message }
func (e *Engine) drive(parent context.Context, op registry.Operation) error {
	ctx, cancel := context.WithTimeout(parent, 15*time.Minute)
	defer cancel()
	ctl, err := e.DB.Control(ctx)
	if err != nil {
		return err
	}
	if ctl.Maintenance {
		return registry.ErrMaintenance
	}
	if ctl.ControlEpoch != op.ControlEpoch {
		return registry.ErrControlEpoch
	}
	j := &Job{Engine: e, Op: op, Intent: Intent{Name: op.RuntimeName, BindingID: op.BindingID}}
	if op.BindingID != "" {
		j.Binding, err = e.DB.GetBinding(ctx, op.BindingID)
		if err != nil {
			return err
		}
	}
	if op.Kind == rt.VolumeOperation && op.BindingID == "" {
		var route rt.VolumeRoute
		if err := json.Unmarshal(op.Request, &route); err != nil {
			return err
		}
		j.Binding.RuntimeInstanceID = route.RuntimeInstanceID
		j.Binding.RuntimeBundleID = route.RuntimeBundleID
	}
	if op.State != registry.Queued {
		if err = json.Unmarshal(op.Dispatch, &j.Intent); err != nil {
			return err
		}
	}
	j.Runtime, err = e.ForBinding(j.Binding)
	if rt.ManagedStatePoint(op.Kind) || (op.Kind == "start" && (j.Binding.PowerState == registry.PowerHibernated || j.Intent.StatePoint != nil)) {
		return j.driveStatePoint(ctx, err)
	}
	if op.Kind == "sync_keys" && !bindingRuntime(j.Binding).Legacy() {
		return j.driveNativeSSH(ctx, err)
	}
	if rt.IsNativeFork(op.Kind) {
		return j.driveNativeFork(ctx, err)
	}
	if op.Kind == "create" {
		var request Create
		if err := json.Unmarshal(op.Request, &request); err != nil {
			return err
		}
		if request.BootMode == rt.BootSystem {
			return j.driveSystemLaunch(ctx, request, err)
		}
	}
	if op.Kind == "start" && !bindingRuntime(j.Binding).Legacy() {
		machine, loadErr := e.DB.GetMachine(ctx, op.MachineID)
		if loadErr != nil {
			return loadErr
		}
		var request Create
		if loadErr := json.Unmarshal(machine.Spec, &request); loadErr != nil {
			return loadErr
		}
		if request.BootMode == rt.BootSystem {
			return j.driveSystemLaunch(ctx, request, err)
		}
	}
	if err == nil {
		err = e.AdmitRuntime(bindingRuntime(j.Binding).BundleRoute, op.Kind)
	}
	dispatched := op.State != registry.Queued && j.Intent.Stage != "preflight"
	if err != nil {
		if op.State == registry.Queued {
			if stageErr := j.Stage(ctx, "preflight", ""); stageErr != nil {
				return stageErr
			}
		}
		j.Written = dispatched
		return j.fail(ctx, err)
	}
	if err = readyRuntime(ctx, j.Runtime); err != nil {
		return err
	}
	native, hasNative := j.Runtime.(rt.NativeControl)
	useNative := hasNative && !bindingRuntime(j.Binding).Legacy() && rt.NativePowerCapability(op.Kind) != ""
	policyReplay := dispatched && op.Kind == "configure" && j.Intent.Stage == "apply_lifecycle_policy"
	if dispatched && !policyReplay {
		if useNative {
			return j.reconcileNative(ctx, native)
		}
		return j.reconcile(ctx)
	}
	if !policyReplay {
		if err = j.prepareDispatch(ctx); err != nil || j.Op.Terminal() {
			return err
		}
	}
	if useNative {
		err = j.nativePower(ctx, native)
	} else if fn := e.Extra[op.Kind]; fn != nil {
		err = fn(ctx, j)
	} else {
		err = j.power(ctx)
	}
	if err == nil || parent.Err() != nil {
		return err
	}
	if useNative && j.Intent.Native == nil && errors.Is(err, rt.ErrUnavailable) {
		// The preflight has issued no operation. A restarted Worker may need
		// another observation before the original intent can be dispatched.
		return err
	}
	if errors.Is(err, registry.ErrOperationState) || errors.Is(err, registry.ErrHeavyBusy) {
		return err
	}
	return j.fail(parent, err)
}

func (j *Job) prepareDispatch(ctx context.Context) error {
	if err := j.Stage(ctx, "preflight", ""); err != nil {
		return err
	}
	if err := j.DB.CheckGuestDispatch(ctx, j.Op.ID); err != nil {
		if errors.Is(err, registry.ErrGuestAuthorityRevoked) {
			return j.fail(ctx, &Failure{"PERMISSION_DENIED", err.Error()})
		}
		return err
	}
	if !j.Op.Heavy {
		return nil
	}
	var s syscall.Statfs_t
	if err := syscall.Statfs(j.Config.StateDir, &s); err != nil {
		return j.fail(ctx, err)
	}
	if s.Bavail*uint64(s.Bsize) < j.Config.Storage.MinBytes || s.Blocks > 0 && 100*s.Bavail/s.Blocks < uint64(j.Config.Storage.MinPercent) {
		return j.fail(ctx, &Failure{"DISK_LOW", "insufficient free space for heavy operation"})
	}
	return nil
}

// CheckSameRuntime protects operations whose native source and target must live
// in the same runtime instance and use the same bundle.
func (j *Job) CheckSameRuntime(source registry.Binding) error {
	if _, err := j.ForBinding(source); err != nil {
		return err
	}
	if bindingRuntime(source).BundleRoute != bindingRuntime(j.Binding).BundleRoute {
		return &rt.Error{Code: "NOT_SUPPORTED", Message: "source and target use different runtime instances or bundles"}
	}
	return nil
}

func (j *Job) labels() map[string]string {
	return map[string]string{"platform.machine": j.Op.MachineID, "platform.binding": j.Binding.ID, "platform.spec": j.Binding.SpecHash}
}
func (j *Job) matches(o rt.Observation) bool {
	for k, v := range j.labels() {
		if o.Labels[k] != v {
			return false
		}
	}
	return true
}
func (j *Job) loadOwned(ctx context.Context) (rt.Observation, error) {
	o, err := j.Runtime.Observe(ctx, j.Op.RuntimeName)
	if err != nil {
		return o, err
	}
	if !j.matches(o) {
		return o, &Failure{"RECOVERY_REQUIRED", "runtime ownership labels differ from current binding"}
	}
	return o, nil
}

func (j *Job) start(ctx context.Context, name string) error {
	receipt, err := j.Runtime.Start(ctx, name)
	if err != nil {
		return err
	}
	j.Written = true
	o := receipt.Observation
	if o.WorkloadError == nil {
		return nil
	}
	message := *o.WorkloadError
	if message == "" {
		message = "runtime reported that the image workload failed to start"
	}
	j.Intent.WorkloadFailure = &workloadStartFailure{Identity: o.Identity, Message: message}
	// The Start response can be the only source of this error.
	return j.Stage(ctx, j.Intent.Stage, j.Intent.Target)
}

func (j *Job) workloadError(o rt.Observation) (*string, error) {
	if failure := j.Intent.WorkloadFailure; failure != nil {
		if o.State != rt.StateRunning || !o.Identity.Equal(failure.Identity) {
			return nil, errors.New("workload start receipt does not match the running execution")
		}
		return &failure.Message, nil
	}
	if j.Intent.Stage == "start" || (j.Intent.Stage == "task_wake" && j.Intent.PauseID == "") {
		cleared := ""
		return &cleared, nil
	}
	return nil, nil
}

func (j *Job) power(ctx context.Context) error {
	op := j.Op
	if op.Kind == "sync_keys" {
		return j.updateSSH(ctx)
	}
	name := op.RuntimeName
	if op.Kind == "create" {
		var req Create
		if err := json.Unmarshal(op.Request, &req); err != nil {
			return err
		}
		tpl, err := j.DB.GetTemplate(ctx, req.TemplateID)
		if err != nil {
			return err
		}
		if err = templates.VerifyLocal(j.Config.Runtime.LocalImageDir, tpl); err != nil {
			return err
		}
		var spec struct {
			LocalImage string   `json:"local_image"`
			Command    []string `json:"command"`
		}
		if err = json.Unmarshal(tpl.Spec, &spec); err != nil {
			return err
		}
		r := rt.CreateRequest{Name: name, Image: tpl.ImageDigest, LocalImage: spec.LocalImage, Command: spec.Command, CPUs: req.Resources.CPUs, MemoryMB: req.Resources.Memory, StorageGB: req.Resources.Storage, OverlayGB: req.Resources.Overlay, Labels: j.labels()}
		if r.LocalImage != "" {
			r.Image = ""
		}
		if err = j.Stage(ctx, "create", "created"); err != nil {
			return err
		}
		if _, err = j.Runtime.Create(ctx, r); err != nil {
			return err
		}
		j.Written = true
		if req.Start {
			if err = j.Stage(ctx, "start", "running"); err != nil {
				return err
			}
			if err = j.start(ctx, name); err != nil {
				return err
			}
		}
		return j.verify(ctx)
	}
	if op.Kind == "fork" {
		src, err := j.DB.GetMachine(ctx, op.SourceMachineID)
		if err != nil {
			return err
		}
		if src.CurrentBinding == nil {
			return registry.ErrGeneration
		}
		if err := j.CheckSameRuntime(*src.CurrentBinding); err != nil {
			return err
		}
		o, err := j.Runtime.Observe(ctx, src.CurrentBinding.RuntimeName)
		if err != nil {
			return err
		}
		if o.Labels["platform.binding"] != src.CurrentBinding.ID || o.State != "running" || !o.Branchable {
			return &Failure{"PROFILE_UNSUPPORTED", "source must be a running branchable owned machine"}
		}
		j.Intent.Source = src.CurrentBinding.RuntimeName
		if err = j.Stage(ctx, "branch", "running"); err != nil {
			return err
		}
		if _, err = j.Runtime.Fork(ctx, j.Intent.Source, rt.ForkRequest{Name: name, Branchable: true, Ports: []rt.Port{}, Labels: j.labels()}); err != nil {
			return err
		}
		j.Written = true
		return j.verify(ctx)
	}
	o, err := j.loadOwned(ctx)
	if err != nil {
		return err
	}
	action := op.Kind
	if action == "idle_pause" {
		action = "pause"
		m, err := j.DB.GetMachine(ctx, op.MachineID)
		if err != nil {
			return err
		}
		var prev struct {
			At *time.Time `json:"last_user_activity_at"`
		}
		if err = json.Unmarshal(op.Request, &prev); err != nil {
			return err
		}
		if m.LastUserActivityAt == nil || prev.At == nil || !m.LastUserActivityAt.Equal(*prev.At) || m.KeepRunning || m.CurrentBinding.GuestManagementStatus != "ready" {
			return &Failure{"ACTIVE_ACTIVITY", "idle criteria changed"}
		}
	}
	if action == "wake" {
		switch o.State {
		case "running":
			if !o.Identity.Complete() {
				return &Failure{"RECOVERY_REQUIRED", "runtime execution identity unavailable"}
			}
			x := Evidence(o)
			return j.Done(ctx, registry.TransitionRequest{Observation: &x, Effect: registry.EffectNone})
		case "paused":
			if o.PressurePaused() {
				return &Failure{"RESOURCE_EXHAUSTED", "runtime pressure guard currently owns pause"}
			}
			action = "resume"
		case "created", "stopped":
			if !j.Config.Lifecycle.WakeStopped {
				return &Failure{"SERVICE_NOT_READY", "cold wake is disabled"}
			}
			action = "start"
		default:
			return &Failure{"RECOVERY_REQUIRED", "wake requires a known eligible state"}
		}
	}
	target := map[string]string{"pause": "paused", "resume": "running", "start": "running", "stop": "stopped", "delete": "absent"}[action]
	if target == "" {
		return registry.ErrInvalid
	}
	valid := map[string]bool{"start": o.State == "created" || o.State == "stopped", "stop": o.State == "running", "resume": o.State == "paused", "pause": o.State == "running", "delete": o.State == "created" || o.State == "stopped" || o.State == "paused" || o.State == "running"}
	if !valid[action] {
		return &Failure{"STATE_CONFLICT", fmt.Sprintf("%s is unavailable in %s", action, o.State)}
	}
	if action == "stop" || action == "pause" || action == "delete" {
		if err = j.checkHolds(ctx, action); err != nil {
			return err
		}
	}
	if action == "pause" {
		j.Intent.PauseID = "p" + op.ID[3:]
	}
	if action == "resume" {
		if j.Binding.PauseStatus == "active" {
			j.Intent.PauseID = j.Binding.RuntimePauseID
		} else if !o.PressurePaused() {
			return &Failure{"RECOVERY_REQUIRED", "saved execution has no matching pause identity"}
		}
	}
	if op.Kind == "idle_pause" {
		j.mu.Lock()
		if j.holds[op.MachineID] != 0 {
			j.mu.Unlock()
			return &Failure{"ACTIVE_ACTIVITY", "activity arrived before idle dispatch"}
		}
		err = j.StageApply(ctx, action, target, func(ctx context.Context, tx *sql.Tx) error {
			var pending bool
			if err := tx.QueryRowContext(ctx, "SELECT wake_pending FROM operations WHERE id=?", j.Op.ID).Scan(&pending); err != nil {
				return err
			}
			if pending {
				return &Failure{"ACTIVE_ACTIVITY", "activity arrived during idle preflight"}
			}
			return nil
		})
		j.mu.Unlock()
	} else {
		err = j.Stage(ctx, action, target)
	}
	if err != nil {
		return err
	}
	switch action {
	case "start":
		err = j.start(ctx, name)
	case "pause":
		_, err = j.Runtime.Pause(ctx, name, j.Intent.PauseID)
	case "resume":
		_, err = j.Runtime.Resume(ctx, name, j.Intent.PauseID)
	case "stop":
		_, err = j.Runtime.Stop(ctx, name)
	case "delete":
		_, err = j.Runtime.Delete(ctx, name)
	}
	if err != nil {
		return err
	}
	j.Written = true
	return j.verify(ctx)
}
func (j *Job) reconcile(ctx context.Context) error {
	j.Written = true
	if fn := j.Recover[j.Op.Kind]; fn != nil {
		if err := fn(ctx, j); err != nil {
			return j.fail(ctx, err)
		}
		return nil
	}
	if j.Intent.Target == "" {
		return j.fail(ctx, errors.New("dispatched operation needs specific outcome evidence"))
	}
	if err := j.verify(ctx); err != nil {
		return j.fail(ctx, err)
	}
	return nil
}
func (j *Job) verify(ctx context.Context) error {
	o, err := j.loadOwned(ctx)
	if j.Intent.Target == "absent" && errors.Is(err, rt.ErrNotFound) {
		return j.Done(ctx, registry.TransitionRequest{MarkDeleted: true, Effect: registry.EffectApplied, DesiredPower: "deleted", Apply: func(ctx context.Context, tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, "UPDATE dependencies SET status='released' WHERE child_binding_id=? AND kind='live_branch_resource'", j.Binding.ID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "UPDATE port_allocations SET state='released' WHERE binding_id=?", j.Binding.ID); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, "DELETE FROM snapshot_pins WHERE owner_kind='binding' AND owner_id=? OR owner_kind='pre_replacement' AND owner_id IN (SELECT id FROM operations WHERE machine_id=? AND kind='rollback')", j.Binding.ID, j.Binding.MachineID)
			return err
		}})
	}
	if err != nil {
		return err
	}
	if o.State != j.Intent.Target || (o.State == "running" && !o.Identity.Complete()) {
		return fmt.Errorf("observed %s without confirmed target identity/state %s", o.State, j.Intent.Target)
	}
	x := Evidence(o)
	x.WorkloadError, err = j.workloadError(o)
	if err != nil {
		return err
	}
	power := o.State
	if power == "created" {
		power = "stopped"
	}
	r := registry.TransitionRequest{Effect: registry.EffectApplied, Observation: &x, PromoteBinding: j.Binding.Status != "current", DesiredPower: power}
	if j.Intent.Stage == "pause" {
		x.RuntimePauseID = j.Intent.PauseID
		x.PauseStatus = "active"
	}
	if j.Intent.Stage == "resume" {
		x.PauseStatus = "consumed"
	}
	if j.Op.Kind == "create" && j.Intent.Stage == "create" {
		var req Create
		if err = json.Unmarshal(j.Op.Request, &req); err != nil {
			return err
		}
		if req.Start {
			if err = j.Stage(ctx, "start", "running"); err != nil {
				return err
			}
			if err = j.start(ctx, j.Op.RuntimeName); err != nil {
				return err
			}
			return j.verify(ctx)
		}
	}
	if j.Op.Kind == "fork" {
		if o.ParentMachine != j.Intent.Source {
			return errors.New("child runtime parent mismatch")
		}
		r.Apply = func(ctx context.Context, tx *sql.Tx) error {
			for _, kind := range []string{"logical_origin", "live_branch_resource"} {
				res, err := tx.ExecContext(ctx, `INSERT INTO dependencies(parent_binding_id,child_binding_id,kind,observed_at,status) SELECT id,?,?,?,'active' FROM runtime_bindings WHERE runtime_name=? AND machine_id=?`, j.Binding.ID, kind, time.Now().UnixMilli(), j.Intent.Source, j.Op.SourceMachineID)
				if err != nil {
					return err
				}
				n, err := res.RowsAffected()
				if err != nil || n != 1 {
					return registry.ErrGeneration
				}
			}
			return nil
		}
	}
	if o.State == "running" {
		adopted := j.guestReady(ctx, &x)
		if x.GuestManagementStatus == "ready" {
			keyRev, _ := j.SSHKeyRevision(ctx)
			pub, sshErr := j.InstallSSH(ctx, j.Binding)
			if sshErr != nil {
				x.GuestManagementEvidence = JSON(map[string]any{"task_manager": x.GuestManagementEvidence, "ssh_ready": false})
			} else {
				x.GuestManagementEvidence = JSON(map[string]any{"task_manager": x.GuestManagementEvidence, "ssh_ready": true, "ssh_key_revision": keyRev, "ssh_host_key": pub})
			}
		}
		if len(adopted) > 0 {
			prev := r.Apply
			r.Apply = func(ctx context.Context, tx *sql.Tx) error {
				if prev != nil {
					if e := prev(ctx, tx); e != nil {
						return e
					}
				}
				return SaveAdopted(ctx, tx, j.Binding, adopted)
			}
		}
	}
	if j.Intent.WorkloadFailure != nil && (j.Op.Kind == "create" || j.Op.Kind == "start") {
		r.Error = JSON(map[string]string{"code": registry.WorkloadStartFailed, "message": j.Intent.WorkloadFailure.Message})
		return j.finish(ctx, registry.Failed, r)
	}
	return j.Done(ctx, r)
}
func (j *Job) guestReady(ctx context.Context, x *registry.Observation) (adopted []tasks.Task) {
	x.GuestManagementStatus = "degraded"
	x.GuestManagementEvidence = JSON(map[string]string{"reason": "handshake unavailable"})
	m, err := j.DB.GetMachine(ctx, j.Op.MachineID)
	if err != nil {
		return
	}
	tpl, err := j.DB.GetTemplate(ctx, m.TemplateID)
	if err != nil || tpl.GuestRelease == "" {
		return
	}
	req := tasks.Request{Method: "bind", Binding: GuestBinding(j.Binding), OperationID: j.Op.ID}
	if j.Op.Kind == "fork" {
		src, e := j.DB.GetMachine(ctx, j.Op.SourceMachineID)
		if e != nil || src.CurrentBinding == nil || j.CheckSameRuntime(*src.CurrentBinding) != nil {
			return
		}
		req.Method = "adopt_live_fork"
		req.Source = GuestBinding(*src.CurrentBinding)
	}
	probe, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_, err = j.Call(probe, j.Binding, tasks.Request{Method: "hello"})
	if err != nil {
		_, err = j.Runtime.Exec(probe, j.Binding.RuntimeName, rt.ExecRequest{Command: []string{tasks.BootstrapPath}, Background: true})
		if err != nil {
			return
		}
	}
	for probe.Err() == nil {
		res, err := j.Call(probe, j.Binding, req)
		if err == nil && res.Binding != nil && *res.Binding == *req.Binding {
			hello, err := j.Call(probe, j.Binding, tasks.Request{Method: "hello"})
			if err == nil && hello.Hello != nil {
				x.GuestManagementStatus = "ready"
				x.GuestManagementEvidence = JSON(hello.Hello)
			}
			return res.Tasks
		}
		select {
		case <-probe.Done():
			return
		case <-time.After(150 * time.Millisecond):
		}
	}
	return
}
func (j *Job) checkHolds(ctx context.Context, action string) error {
	j.mu.Lock()
	holds := j.holds[j.Op.MachineID]
	j.mu.Unlock()
	if holds > 0 {
		return &Failure{"ACTIVE_ACTIVITY", "machine has active connections"}
	}
	var n int
	if err := j.DB.ReadDB().QueryRowContext(ctx, "SELECT COUNT(*) FROM dependencies WHERE parent_binding_id=? AND kind='live_branch_resource' AND status!='released'", j.Binding.ID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return &Failure{"DEPENDENCY_BLOCKED", "live fork child retains a native resource dependency"}
	}
	if err := j.DB.ReadDB().QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM leases WHERE machine_id=? AND binding_generation=? AND state!='released')+(SELECT COUNT(*) FROM exec_sessions WHERE binding_id=? AND state NOT IN ('done','exited','lost','failed'))`, j.Op.MachineID, j.Binding.Generation, j.Binding.ID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return &Failure{"ACTIVE_ACTIVITY", "active lease or exec session"}
	}
	var opts Action
	if err := json.Unmarshal(j.Op.Request, &opts); err != nil {
		return err
	}
	// A disconnected client or stale host cache cannot prove a task ended.
	if j.Binding.ObservedState == "running" && j.Binding.GuestManagementStatus == "ready" {
		res, err := j.Call(ctx, j.Binding, tasks.Request{Method: "list"})
		if err != nil {
			return &Failure{"ACTIVE_ACTIVITY", "cannot verify guest task state"}
		}
		for _, t := range res.Tasks {
			if t.Namespace != j.Binding.TaskNamespace || t.MachineID != j.Binding.MachineID {
				return registry.ErrStaleObservation
			}
			if _, err := j.DB.DB().ExecContext(ctx, "UPDATE tasks SET state=?,exit_code=?,observed_at=?,result_json=? WHERE id=? AND task_namespace=? AND observed_at<=?", t.State, t.ExitCode, t.ObservedAt.UnixMilli(), string(JSON(t)), t.ID, t.Namespace, t.ObservedAt.UnixMilli()); err != nil {
				return err
			}
			if !t.Terminal() && (action == "stop" || action == "delete") && opts.CancelTasks {
				if _, err := j.CancelGuestTask(ctx, tasks.Request{Method: "cancel", TaskID: t.ID}); err != nil {
					return err
				}
				continue
			}
			if t.Terminal() {
				continue
			}
			if action == "pause" && opts.AllowActive && t.State == "running" {
				continue
			}
			return &Failure{"ACTIVE_ACTIVITY", "guest task " + t.ID + " is " + t.State}
		}
	}
	if action == "pause" && opts.AllowActive {
		return nil
	}
	if err := j.DB.ReadDB().QueryRowContext(ctx, "SELECT COUNT(*) FROM tasks WHERE machine_id=? AND binding_generation=? AND state NOT IN ('succeeded','failed','cancelled','interrupted')", j.Op.MachineID, j.Binding.Generation).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return &Failure{"ACTIVE_ACTIVITY", "task record requires reconciliation"}
	}
	return nil
}

func SaveAdopted(ctx context.Context, tx *sql.Tx, b registry.Binding, ts []tasks.Task) error {
	for _, t := range ts {
		if t.Namespace != b.TaskNamespace || t.MachineID != b.MachineID {
			return errors.New("adopted task identity mismatch")
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO tasks(id,machine_id,binding_generation,task_namespace,guest_task_id,start_token,spec_hash,spec_json,state,exit_code,origin_machine_id,origin_task_id,observed_at,result_json) VALUES(?,?,?,?,?,?,'','{}',?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, t.ID, b.MachineID, b.Generation, t.Namespace, t.ID, "origin:"+t.ID, t.State, t.ExitCode, t.OriginMachineID, t.OriginTaskID, t.ObservedAt.UnixMilli(), string(JSON(t)))
		if err != nil {
			return err
		}
	}
	return nil
}

// TaskRunning performs any necessary wake under the already accepted task's
// durable ownership. A restart after the resume intent reobserves the same VM.
func (j *Job) TaskRunning(ctx context.Context, recovering bool) error {
	o, err := j.loadOwned(ctx)
	if err != nil {
		return err
	}
	if o.State != "running" {
		if recovering {
			return errors.New("task wake was dispatched and its execution is unconfirmed")
		}
		action := "resume"
		pid := ""
		switch o.State {
		case "paused":
			if o.PressurePaused() {
				return &Failure{"RESOURCE_EXHAUSTED", "pressure pause requires explicit resume"}
			}
			if j.Binding.PauseStatus != "active" || j.Binding.RuntimePauseID == "" {
				return &Failure{"RECOVERY_REQUIRED", "saved execution identity requires reconciliation"}
			}
			pid = j.Binding.RuntimePauseID
		case "stopped", "created":
			if !j.Config.Lifecycle.WakeStopped {
				return &Failure{"SERVICE_NOT_READY", "cold wake is disabled"}
			}
			action = "start"
		default:
			return &Failure{"RECOVERY_REQUIRED", "unusable task execution state"}
		}
		// Acquire the node heavy permit in the same transaction that upgrades this
		// light task operation. Never queue a second resume under another owner.
		if !j.Op.Heavy {
			tx, err := j.DB.DB().BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			var n int
			if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM operations WHERE heavy=1 AND state IN ('dispatching','observing','recovery_required') AND id!=?", j.Op.ID).Scan(&n); err != nil {
				return err
			}
			if n >= j.Config.Lifecycle.Heavy {
				return &Failure{"RESOURCE_EXHAUSTED", "heavy operation already in flight"}
			}
			if _, err = tx.ExecContext(ctx, "UPDATE operations SET heavy=1 WHERE id=? AND state='dispatching'", j.Op.ID); err != nil {
				return err
			}
			if err = tx.Commit(); err != nil {
				return err
			}
			j.Op.Heavy = true
		}
		j.Intent.PauseID = pid
		if err = j.Stage(ctx, "task_wake", ""); err != nil {
			return err
		}
		if action == "resume" {
			_, err = j.Runtime.Resume(ctx, j.Binding.RuntimeName, pid)
		} else {
			err = j.start(ctx, j.Binding.RuntimeName)
		}
		if err != nil {
			return err
		}
		j.Written = true
		o, err = j.loadOwned(ctx)
		if err != nil {
			return err
		}
	}
	if o.State != "running" || !o.Identity.Complete() {
		return errors.New("task requires confirmed current execution")
	}
	if !recovering && j.Intent.Stage != "task_wake" && j.Binding.GuestManagementStatus == "ready" {
		return nil
	}
	x := Evidence(o)
	x.WorkloadError, err = j.workloadError(o)
	if err != nil {
		return err
	}
	if j.Binding.PauseStatus == "active" {
		x.PauseStatus = "consumed"
	}
	j.guestReady(ctx, &x)
	if err = j.transition(ctx, registry.TransitionRequest{State: registry.Dispatching, Stage: "task_ready", Dispatch: JSON(j.Intent), Observation: &x}); err != nil {
		return err
	}
	if x.GuestManagementStatus != "ready" {
		return &Failure{"CAPABILITY_NOT_VERIFIED", "Guest Task Manager handshake unavailable"}
	}
	return nil
}
