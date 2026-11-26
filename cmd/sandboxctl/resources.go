package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func sub(use, short string, args cobra.PositionalArgs, run func(*cobra.Command, []string) error) *cobra.Command {
	return &cobra.Command{Use: use, Short: short, Args: args, RunE: run}
}

func (c *client) showItem(resource string) *cobra.Command {
	return sub("get ID", "Get resource details", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		if resource == "machines" {
			return c.showMachine(cmd, args[0], "")
		}
		return c.show(cmd, res(resource, args[0]))
	})
}

func (c *client) list(use, path string, machineScoped bool) *cobra.Command {
	var cursor string
	var limit int
	args := cobra.NoArgs
	if machineScoped {
		args = cobra.ExactArgs(1)
	}
	command := sub(use, "List resources", args, func(cmd *cobra.Command, args []string) error {
		endpoint := path
		if machineScoped {
			var err error
			endpoint, err = c.machinePath(cmd.Context(), args[0], "/"+path)
			if err != nil {
				return err
			}
		}
		query := url.Values{"cursor": {cursor}, "limit": {strconv.Itoa(limit)}}
		return c.show(cmd, endpoint+"?"+query.Encode())
	})
	command.Flags().StringVar(&cursor, "cursor", "", "Cursor from the preceding page")
	command.Flags().IntVar(&limit, "limit", 100, "Maximum items in this page")
	return command
}

func jsonBody(value string) (json.RawMessage, error) {
	v := json.RawMessage(value)
	if len(v) == 0 || v[0] != '{' || !json.Valid(v) {
		return nil, errors.New("expected a JSON object")
	}
	return v, nil
}

func (c *client) update(use, path, method string, scoped bool) *cobra.Command {
	var body string
	args := cobra.NoArgs
	if scoped {
		args = cobra.ExactArgs(1)
	}
	cmd := sub(use, "Submit a JSON configuration", args, func(cmd *cobra.Command, args []string) error {
		payload, err := jsonBody(strings.TrimSpace(body))
		if err != nil {
			return err
		}
		target := path
		var rev int64
		if scoped {
			if path == "machines" {
				target, err = c.machinePath(cmd.Context(), args[0], "")
				if err != nil {
					return err
				}
			} else {
				target = res(path, args[0])
			}
		}
		if c.revision == 0 && (method == "PATCH" || method == "DELETE") {
			var obj struct {
				Revision int64 `json:"revision"`
			}
			if err = c.get(cmd.Context(), target, &obj); err != nil {
				return err
			}
			rev = obj.Revision
		}
		return c.mutation(cmd, method, target, payload, rev)
	})
	cmd.Flags().StringVar(&body, "body", "{}", "JSON request object")
	return cmd
}

func (c *client) machineResize() *cobra.Command {
	var cpus uint8
	var memory, balloon, limit uint32
	var clear bool
	cmd := sub("resize MACHINE", "Apply machine resource settings", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		patch := map[string]any{}
		if cmd.Flags().Changed("cpus") {
			if cpus == 0 {
				return errors.New("cpus must be positive")
			}
			patch["cpu_count"] = cpus
		}
		if cmd.Flags().Changed("memory-mib") {
			if memory == 0 {
				return errors.New("memory-mib must be positive")
			}
			patch["memory_mib"] = memory
		}
		if cmd.Flags().Changed("balloon-mib") {
			patch["balloon_target_mib"] = balloon
		}
		if cmd.Flags().Changed("cpu-limit-millicpus") {
			if limit == 0 {
				return errors.New("cpu-limit-millicpus must be positive")
			}
			patch["cpu_limit_millicpus"] = limit
		}
		if clear {
			patch["cpu_limit_millicpus"] = nil
		}
		if len(patch) == 0 {
			return errors.New("provide a resource setting to change")
		}
		m, err := c.machine(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		return c.mutation(cmd, "PATCH", res("machines", m.ID), map[string]any{"expected_generation": m.Generation, "resources": patch}, m.Revision)
	})
	cmd.Flags().Uint8Var(&cpus, "cpus", 0, "Target number of online vCPUs")
	cmd.Flags().Uint32Var(&memory, "memory-mib", 0, "Target allocated memory in MiB")
	cmd.Flags().Uint32Var(&balloon, "balloon-mib", 0, "Balloon target in MiB (zero deflates)")
	cmd.Flags().Uint32Var(&limit, "cpu-limit-millicpus", 0, "Maximum CPU bandwidth in millicpus")
	cmd.Flags().BoolVar(&clear, "clear-cpu-limit", false, "Restore the allocated vCPU bandwidth")
	cmd.MarkFlagsMutuallyExclusive("cpu-limit-millicpus", "clear-cpu-limit")
	return cmd
}

func (c *client) machines() *cobra.Command {
	group := &cobra.Command{Use: "machines", Short: "Manage persistent workspaces"}
	group.AddCommand(c.list("list", "machines", false), c.showItem("machines"), c.update("patch MACHINE", "machines", "PATCH", true))
	group.AddCommand(sub("dependencies MACHINE", "List lineage and native dependencies", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		return c.showMachine(cmd, args[0], "/dependencies")
	}))
	group.AddCommand(sub("metrics MACHINE", "Read runtime resource metrics", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		return c.showMachine(cmd, args[0], "/metrics")
	}))
	group.AddCommand(sub("resources MACHINE", "Read resource settings, capabilities and measured allocation", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		return c.showMachine(cmd, args[0], "/resources")
	}), c.machineResize())
	group.AddCommand(sub("network MACHINE", "Read the current private network attachment", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		return c.showMachine(cmd, args[0], "/network")
	}))

	var template, bootMode, runtimeInstance, networkID, networkMode string
	var start, idle, liveGrowth bool
	var cpu, memory, storage, overlay, idleSeconds int
	create := sub("create NAME", "Create a workspace", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		resources := map[string]int{"cpu_count": cpu, "memory_mib": memory}
		body := map[string]any{
			"name": args[0], "template_id": template, "start": start,
			"resources": resources,
		}
		if bootMode != "" {
			body["boot_mode"] = bootMode
		}
		if liveGrowth {
			body["live_compute_growth"] = true
		}
		if runtimeInstance != "" {
			body["runtime_instance_id"] = runtimeInstance
		}
		if networkID != "" || networkMode != "" {
			network := map[string]string{}
			if networkID != "" {
				network["network_id"] = networkID
			}
			if networkMode != "" {
				network["mode"] = networkMode
			}
			body["network"] = network
		}
		if bootMode != "system" && !liveGrowth || cmd.Flags().Changed("storage-gib") {
			resources["storage_gib"] = storage
		}
		if bootMode != "system" && !liveGrowth || cmd.Flags().Changed("overlay-gib") {
			resources["overlay_gib"] = overlay
		}
		if cmd.Flags().Changed("idle") || cmd.Flags().Changed("idle-seconds") {
			body["idle_policy"] = map[string]any{"enabled": idle, "timeout_seconds": idleSeconds}
		}
		return c.mutation(cmd, "POST", "machines", body, 0)
	})
	create.Flags().StringVar(&template, "template", "", "Registered template ID")
	_ = create.MarkFlagRequired("template")
	create.Flags().StringVar(&bootMode, "boot-mode", "", "Template boot mode (oci or system)")
	create.Flags().BoolVar(&liveGrowth, "live-compute-growth", false, "Enable live CPU and RAM growth on a compatible System runtime and disable full VM state capture")
	create.Flags().StringVar(&runtimeInstance, "runtime-instance", "", "Registered Serve instance")
	create.Flags().StringVar(&networkID, "network", "", "Registered Worker private network for System templates")
	create.Flags().StringVar(&networkMode, "network-mode", "", "System network mode (native or fixed_internal), using the platform default when omitted")
	create.Flags().IntVar(&cpu, "cpu", 2, "Number of vCPUs")
	create.Flags().IntVar(&memory, "memory-mib", 2048, "Memory in MiB")
	create.Flags().IntVar(&storage, "storage-gib", 20, "Guest disk space in GiB")
	create.Flags().IntVar(&overlay, "overlay-gib", 10, "Overlay size in GiB")
	create.Flags().BoolVar(&start, "start", true, "Start the workspace after creating it")
	create.Flags().BoolVar(&idle, "idle", true, "Automatically pause on idle")
	create.Flags().IntVar(&idleSeconds, "idle-seconds", 1800, "Automatic pause timeout")
	group.AddCommand(create)

	for _, act := range []string{"start", "stop", "pause", "resume", "wake", "delete"} {
		act := act
		var reason string
		var cancel, allow, yes bool
		power := sub(act+" MACHINE", "Change machine power or lifetime", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
			if act == "delete" && !yes {
				return errors.New("delete requires --yes")
			}
			m, err := c.machine(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			path := res("machines", m.ID)
			body := map[string]any{"expected_generation": m.Generation}
			if act == "delete" {
				return c.mutation(cmd, "DELETE", path, body, m.Revision)
			}
			if reason != "" {
				body["reason"] = reason
			}
			if cancel {
				body["cancel_tasks"] = cancel
			}
			if allow {
				body["allow_active_tasks"] = allow
			}
			return c.mutation(cmd, "POST", path+"/"+act, body, m.Revision)
		})
		if act == "delete" {
			power.Flags().BoolVar(&yes, "yes", false, "Confirm machine deletion")
		} else {
			power.Flags().StringVar(&reason, "reason", "", "Reason recorded with the operation")
			if act == "stop" || act == "pause" {
				power.Flags().BoolVar(&cancel, "cancel-tasks", false, "Cancel active tasks")
				power.Flags().BoolVar(&allow, "allow-active-tasks", false, "Permit the action with active tasks")
			}
		}
		group.AddCommand(power)
	}
	group.AddCommand(c.guardedPower()...)
	for _, act := range []string{"standby", "hibernate"} {
		var allow bool
		power := sub(act+" MACHINE", "Apply a verified full-state machine action", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
			m, err := c.machine(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			body := map[string]any{"expected_generation": m.Generation}
			if allow {
				body["activity_policy"] = map[string]bool{"allow_active_tasks": true}
			}
			return c.mutation(cmd, "POST", res("machines", m.ID)+"/"+act, body, m.Revision)
		})
		power.Flags().BoolVar(&allow, "allow-active-tasks", false, "Permit the action with active tasks")
		group.AddCommand(power)
	}
	for _, name := range []string{"fork", "clone", "live-fork"} {
		group.AddCommand(c.machineFork(name))
	}
	var admission, volumePolicy string
	batch := sub("fork-batch MACHINE CHILD_NAME [CHILD_NAME...]", "Create independent children from one source capture", cobra.MinimumNArgs(2), func(cmd *cobra.Command, args []string) error {
		m, err := c.machine(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		children := make([]map[string]string, 0, len(args)-1)
		for _, name := range args[1:] {
			children = append(children, map[string]string{"name": name})
		}
		body := map[string]any{
			"children": children, "expected_generation": m.Generation, "require_independent": true,
		}
		if admission != "" {
			if admission != "all_or_reject" && admission != "best_effort" {
				return errors.New("--admission must be all_or_reject or best_effort")
			}
			body["admission"] = admission
		}
		if volumePolicy != "" {
			if volumePolicy != "clone" {
				return errors.New("--volume-policy must be clone")
			}
			body["volume_policy"] = map[string]string{"default": volumePolicy}
		}
		return c.mutation(cmd, "POST", res("machines", m.ID)+"/fork-batches", body, m.Revision)
	})
	batch.Flags().StringVar(&admission, "admission", "", "Worker budget policy (all_or_reject by default, or best_effort)")
	batch.Flags().StringVar(&volumePolicy, "volume-policy", "", "Explicit default volume policy (clone)")
	group.AddCommand(batch)
	return group
}

func (c *client) machineFork(name string) *cobra.Command {
	independent := name == "live-fork"
	suffix := "/forks"
	if name == "clone" {
		suffix = "/clones"
	}
	short := "Create a child machine through the compatibility path"
	if independent {
		short = "Create a native full-state child with verified resource independence"
	}
	child := sub(name+" MACHINE CHILD_NAME", short, cobra.ExactArgs(2), func(cmd *cobra.Command, args []string) error {
		m, err := c.machine(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		body := map[string]any{"child_name": args[1], "expected_generation": m.Generation}
		if name == "clone" {
			body["copy_policy"] = "managed_clean"
		}
		if independent {
			body["require_independent"] = true
		}
		return c.mutation(cmd, "POST", res("machines", m.ID)+suffix, body, m.Revision)
	})
	if name == "fork" {
		child.Flags().BoolVar(&independent, "require-independent", false, "Require verified runtime resource independence")
	}
	return child
}

func commandArgs(args []string, cwd, user string) map[string]any {
	body := map[string]any{"argv": args}
	if cwd != "" {
		body["cwd"] = cwd
	}
	if user != "" {
		body["user"] = user
	}
	return body
}

func (c *client) tasks() *cobra.Command {
	group := &cobra.Command{Use: "tasks", Short: "Manage persistent Guest tasks"}
	group.AddCommand(c.list("list MACHINE", "tasks", true), c.showItem("tasks"))
	var cwd, user, key string
	start := sub("start MACHINE -- COMMAND [ARG...]", "Start a persistent task", cobra.MinimumNArgs(2), func(cmd *cobra.Command, args []string) error {
		m, err := c.machine(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		body := commandArgs(args[1:], cwd, user)
		body["expected_generation"] = m.Generation
		if key != "" {
			body["task_key"] = key
		}
		return c.mutation(cmd, "POST", res("machines", m.ID)+"/tasks", body, m.Revision)
	})
	start.Flags().StringVar(&cwd, "cwd", "/workspace", "Guest working directory")
	start.Flags().StringVar(&user, "user", "root", "Guest user")
	start.Flags().StringVar(&key, "task-key", "", "Stable task key")
	var grace int
	cancel := sub("cancel TASK", "Cancel a task", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		var task struct {
			Machine string `json:"machine_id"`
		}
		target := res("tasks", args[0])
		if err := c.get(cmd.Context(), target, &task); err != nil {
			return err
		}
		m, err := c.machine(cmd.Context(), task.Machine)
		if err != nil {
			return err
		}
		return c.mutation(cmd, "POST", target+"/cancel", map[string]int{"grace_seconds": grace}, m.Revision)
	})
	cancel.Flags().IntVar(&grace, "grace-seconds", 5, "Grace period before forced cancellation")
	group.AddCommand(start, cancel, c.taskLogs())
	return group
}

func (c *client) taskLogs() *cobra.Command {
	var stream string
	var offset int64
	var follow bool
	logs := sub("logs TASK", "Read task bytes starting at an output cursor", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		if stream != "stdout" && stream != "stderr" {
			return errors.New("stream must be stdout or stderr")
		}
		ticker := time.NewTicker(400 * time.Millisecond)
		defer ticker.Stop()
		for {
			query := url.Values{"stream": {stream}, "offset": {strconv.FormatInt(offset, 10)}, "limit": {"65536"}}
			var chunk struct {
				Data     []byte `json:"data_base64"`
				Start    int64  `json:"start"`
				Next     int64  `json:"next"`
				Gap      bool   `json:"gap"`
				Complete bool   `json:"complete"`
			}
			if err := c.get(cmd.Context(), res("tasks", args[0])+"/output?"+query.Encode(), &chunk); err != nil {
				return fmt.Errorf("output cursor %d: %w", offset, err)
			}
			if chunk.Next < offset || len(chunk.Data) != 0 && offset == chunk.Next {
				return errors.New("server returned an invalid output cursor")
			}
			if c.jsonOutput {
				if err := c.print(cmd, chunk); err != nil {
					return err
				}
			} else {
				if chunk.Gap {
					fmt.Fprintf(cmd.ErrOrStderr(), "Output gap before byte %d\n", chunk.Start)
				}
				if _, err := cmd.OutOrStdout().Write(chunk.Data); err != nil {
					return err
				}
			}
			if chunk.Complete || !follow && chunk.Next == offset {
				return nil
			}
			offset = chunk.Next
			if len(chunk.Data) == 0 {
				select {
				case <-cmd.Context().Done():
					return nil
				case <-ticker.C:
				}
			}
		}
	})
	logs.Flags().StringVar(&stream, "stream", "stdout", "stdout or stderr")
	logs.Flags().Int64Var(&offset, "offset", 0, "Byte offset to resume from")
	logs.Flags().BoolVarP(&follow, "follow", "f", false, "Follow output until the task is complete")
	return logs
}

func (c *client) operations() *cobra.Command {
	group := &cobra.Command{Use: "operations", Short: "Observe and reconcile operations"}
	group.AddCommand(c.list("list", "operations", false), c.showItem("operations"),
		sub("wait OPERATION", "Observe until the operation reaches a terminal state", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
			return c.wait(cmd, args[0])
		}))
	for _, act := range []string{"reconcile", "cancel"} {
		act := act
		group.AddCommand(sub(act+" OPERATION", "Update an operation", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
			return c.mutation(cmd, "POST", res("operations", args[0])+"/"+act, struct{}{}, 0)
		}))
	}
	group.AddCommand(sub("retry OPERATION", "Retry failed targets from the retained fork cut", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		path := res("operations", args[0])
		revision := c.revision
		if revision == 0 {
			var op struct {
				Revision int64 `json:"revision"`
			}
			if err := c.get(cmd.Context(), path, &op); err != nil {
				return err
			}
			revision = op.Revision
		}
		return c.mutation(cmd, "POST", path+"/retry", struct{}{}, revision)
	}))
	return group
}

func (c *client) snapshots() *cobra.Command {
	group := &cobra.Command{Use: "snapshots", Short: "Capture, verify, import, and restore snapshots"}
	group.AddCommand(c.list("list MACHINE", "snapshots", true), c.showItem("snapshots"), c.update("delete SNAPSHOT", "snapshots", "DELETE", true))
	var purpose, label, captureMode, verification, taskDisposition, oldState, restoreVolumes, rollbackVolumes string
	capture := sub("capture MACHINE", "Capture a checkpoint", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		m, err := c.machine(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		body := map[string]any{
			"purpose": purpose, "label": label, "expected_generation": m.Generation,
		}
		if captureMode != "" {
			body["capture_mode"] = captureMode
		}
		return c.mutation(cmd, "POST", res("machines", m.ID)+"/snapshots", body, m.Revision)
	})
	capture.Flags().StringVar(&purpose, "purpose", "clone", "clone or recovery")
	capture.Flags().StringVar(&label, "label", "", "Snapshot label")
	capture.Flags().StringVar(&captureMode, "capture-mode", "", "vm or managed_tasks")
	verify := sub("verify SNAPSHOT", "Verify an archive or an isolated restore", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		path := res("snapshots", args[0])
		var current struct {
			Revision int64 `json:"revision"`
		}
		if err := c.get(cmd.Context(), path, &current); err != nil {
			return err
		}
		return c.mutation(cmd, "POST", path+"/verify", map[string]string{"verification": verification}, current.Revision)
	})
	verify.Flags().StringVar(&verification, "verification", "checksum", "checksum or restore")
	restore := sub("restore SNAPSHOT NAME", "Create a new machine from a snapshot", cobra.ExactArgs(2), func(cmd *cobra.Command, args []string) error {
		body := map[string]string{"snapshot_id": args[0], "name": args[1]}
		if restoreVolumes != "" {
			body["volume_restore_policy"] = restoreVolumes
		}
		return c.mutation(cmd, "POST", "restores", body, 0)
	})
	restore.Flags().StringVar(&restoreVolumes, "volume-restore-policy", "", "VM volume policy: exact or clone_conflicts")
	rollback := sub("rollback MACHINE SNAPSHOT", "Replace the current runtime binding from a snapshot", cobra.ExactArgs(2), func(cmd *cobra.Command, args []string) error {
		m, err := c.machine(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		var snapshot struct {
			CaptureMode string `json:"capture_mode"`
		}
		if err := c.get(cmd.Context(), res("snapshots", args[1]), &snapshot); err != nil {
			return err
		}
		body := map[string]any{"snapshot_id": args[1], "expected_generation": m.Generation}
		if c.generation > 0 {
			body["expected_generation"] = c.generation
		}
		if rollbackVolumes != "" {
			body["volume_restore_policy"] = rollbackVolumes
		}
		if snapshot.CaptureMode != "vm" {
			body["task_disposition"], body["old_state_policy"] = taskDisposition, oldState
		}
		return c.mutation(cmd, "POST", res("machines", m.ID)+"/rollback", body, m.Revision)
	})
	rollback.Long = "Replace the current runtime binding and retain a rescue capture of the previous execution."
	rollback.Flags().StringVar(&taskDisposition, "task-disposition", "cancel_captured", "cancel_captured or resume_captured")
	rollback.Flags().StringVar(&oldState, "old-state-policy", "export_then_delete_on_paused", "Old state policy")
	rollback.Flags().StringVar(&rollbackVolumes, "volume-restore-policy", "", "VM volume policy: exact or clone_conflicts")
	group.AddCommand(capture, verify, restore, rollback, c.list("restores", "restores", false))
	group.AddCommand(c.vmSnapshotCommands()...)
	group.AddCommand(sub("export SNAPSHOT OUTPUT", "Download a snapshot archive", cobra.ExactArgs(2), func(cmd *cobra.Command, args []string) error {
		return c.download(cmd, res("snapshots", args[0])+"/export", args[1])
	}))
	group.AddCommand(sub("import ARCHIVE", "Stage a snapshot archive", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		return c.upload(cmd, args[0], "POST", "snapshots/import", 0, true)
	}))
	return group
}

func (c *client) download(cmd *cobra.Command, path, filename string) error {
	resp, err := c.request(cmd.Context(), "GET", path, nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if filename == "-" {
		_, err = io.Copy(cmd.OutOrStdout(), resp.Body)
		return err
	}
	f, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err = io.Copy(f, resp.Body); err != nil {
		f.Close()
		_ = os.Remove(filename)
		return err
	}
	return f.Close()
}

func (c *client) upload(cmd *cobra.Command, source, method, path string, revision int64, digest bool) error {
	f, err := os.Open(source)
	if err != nil {
		return err
	}
	defer f.Close()
	size, err := f.Stat()
	if err != nil {
		return err
	}
	if !size.Mode().IsRegular() {
		return errors.New("upload requires a regular local file")
	}
	headers, err := c.intent(cmd.Context(), revision)
	if err != nil {
		return err
	}
	headers.Set("Content-Type", "application/octet-stream")
	headers.Set("Content-Length", strconv.FormatInt(size.Size(), 10))
	if digest {
		h := sha256.New()
		if _, err = io.Copy(h, f); err != nil {
			return err
		}
		if _, err = f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		headers.Set("X-Content-SHA256", hex.EncodeToString(h.Sum(nil)))
	}
	var body io.Reader = f
	if size.Size() == 0 {
		body = http.NoBody
	}
	return c.write(cmd, method, path, body, headers, 0)
}

func (c *client) files() *cobra.Command {
	group := &cobra.Command{Use: "files", Short: "Transfer Guest files"}
	path := func(id, target, suffix string) string {
		return res("machines", id) + "/files" + suffix + "?" + url.Values{"path": {target}}.Encode()
	}
	group.AddCommand(
		sub("capabilities MACHINE", "Read Guest file capabilities and transfer limits", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
			var m struct {
				Capabilities map[string]json.RawMessage `json:"capabilities"`
			}
			target, err := c.machinePath(cmd.Context(), args[0], "")
			if err != nil {
				return err
			}
			if err := c.get(cmd.Context(), target, &m); err != nil {
				return err
			}
			files := map[string]json.RawMessage{}
			for key, capability := range m.Capabilities {
				if strings.HasPrefix(key, "files.") {
					files[key] = capability
				}
			}
			return c.print(cmd, files)
		}),
		sub("list MACHINE GUEST_DIRECTORY", "List a Guest directory", cobra.ExactArgs(2), func(cmd *cobra.Command, args []string) error {
			id, err := c.machineID(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return c.show(cmd, path(id, args[1], "/list"))
		}),
		sub("get MACHINE GUEST_PATH OUTPUT", "Download a Guest file", cobra.ExactArgs(3), func(cmd *cobra.Command, args []string) error {
			id, err := c.machineID(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return c.download(cmd, path(id, args[1], ""), args[2])
		}),
		sub("put MACHINE LOCAL_FILE GUEST_PATH", "Upload a local file to the Guest", cobra.ExactArgs(3), func(cmd *cobra.Command, args []string) error {
			m, err := c.machine(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return c.upload(cmd, args[1], "PUT", path(m.ID, args[2], ""), m.Revision, true)
		}),
	)
	return group
}

func (c *client) services() *cobra.Command {
	group := &cobra.Command{Use: "services", Short: "Manage stable service endpoints"}
	group.AddCommand(c.list("list MACHINE", "services", true), c.showItem("services"),
		c.update("patch SERVICE", "services", "PATCH", true), c.update("delete SERVICE", "services", "DELETE", true))
	var protocol, body string
	var port int
	create := sub("create MACHINE NAME", "Publish a Guest service", cobra.ExactArgs(2), func(cmd *cobra.Command, args []string) error {
		m, err := c.machine(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		extra, err := jsonBody(strings.TrimSpace(body))
		if err != nil {
			return err
		}
		var spec map[string]any
		if err = json.Unmarshal(extra, &spec); err != nil {
			return err
		}
		spec["name"], spec["protocol"], spec["guest_port"] = args[1], protocol, port
		return c.mutation(cmd, "POST", res("machines", m.ID)+"/services", spec, m.Revision)
	})
	create.Flags().StringVar(&protocol, "protocol", "http", "http, https, ssh or tcp-mtls")
	create.Flags().IntVar(&port, "guest-port", 8080, "Guest TCP port")
	create.Flags().StringVar(&body, "body", "{}", "Optional JSON access and readiness settings")
	group.AddCommand(create)
	return group
}

func (c *client) leases() *cobra.Command {
	group := &cobra.Command{Use: "leases", Short: "Manage keep-running leases"}
	group.AddCommand(c.list("list MACHINE", "leases", true))
	var reason string
	var ttl, semantics int
	acquire := sub("acquire MACHINE", "Acquire a keep-running lease", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		if semantics != 1 && semantics != 2 {
			return errors.New("lease semantics version must be 1 or 2")
		}
		m, err := c.machine(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		body := map[string]any{"reason": reason, "ttl_seconds": ttl}
		if semantics == 2 {
			body["semantics_version"] = semantics
		}
		return c.mutation(cmd, "POST", res("machines", m.ID)+"/leases", body, m.Revision)
	})
	acquire.Flags().StringVar(&reason, "reason", "", "Reason for the keep-running lease")
	acquire.Flags().IntVar(&ttl, "ttl-seconds", 300, "Lease duration")
	acquire.Flags().IntVar(&semantics, "semantics-version", 1, "Expiry behavior: 1 keeps protection pending review, 2 expires at the Worker deadline")
	group.AddCommand(acquire)
	for _, action := range []string{"renew", "release"} {
		action := action
		var token string
		var ttl int
		edit := sub(action+" LEASE", "Change an existing keep-running lease", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
			if token == "" || c.revision <= 0 {
				return errors.New("supply SANDBOX_LEASE_TOKEN and the current --revision from leases list")
			}
			method := http.MethodDelete
			body := map[string]any{"lease_token": token}
			if action == "renew" {
				method = http.MethodPatch
				body["ttl_seconds"] = ttl
			}
			return c.mutation(cmd, method, res("leases", args[0]), body, c.revision)
		})
		edit.Flags().StringVar(&token, "lease-token", os.Getenv("SANDBOX_LEASE_TOKEN"), "Secret lease token (prefer SANDBOX_LEASE_TOKEN)")
		if action == "renew" {
			edit.Flags().IntVar(&ttl, "ttl-seconds", 300, "Lease duration")
		}
		group.AddCommand(edit)
	}
	return group
}

func (c *client) credentials() *cobra.Command {
	group := &cobra.Command{Use: "credentials", Short: "Manage tokens, SSH public keys, and client certificates"}
	for _, resource := range []string{"tokens", "ssh-keys", "client-certificates"} {
		collection := &cobra.Command{Use: resource, Short: "Manage " + resource}
		collection.AddCommand(c.list("list", resource, false))
		collection.AddCommand(c.update("create", resource, "POST", false),
			sub("revoke ID", "Revoke a credential", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
				revision, err := c.credentialRevision(cmd.Context(), resource, args[0])
				if err != nil {
					return err
				}
				return c.mutation(cmd, "DELETE", res(resource, args[0]), struct{}{}, revision)
			}))
		group.AddCommand(collection)
	}
	return group
}

func (c *client) credentialRevision(ctx context.Context, resource, id string) (int64, error) {
	if c.revision > 0 {
		return c.revision, nil
	}
	cursor := ""
	for {
		var page struct {
			Items []struct {
				ID       string `json:"id"`
				TokenID  string `json:"token_id"`
				Revision int64  `json:"revision"`
			} `json:"items"`
			Next string `json:"next_cursor"`
		}
		path := resource + "?" + url.Values{"cursor": {cursor}}.Encode()
		if err := c.get(ctx, path, &page); err != nil {
			return 0, err
		}
		for _, item := range page.Items {
			if item.ID == id || item.TokenID == id {
				if item.Revision <= 0 {
					return 0, errors.New("platform did not return a credential revision")
				}
				return item.Revision, nil
			}
		}
		if page.Next == "" {
			return 0, fmt.Errorf("credential %s was not found", id)
		}
		if page.Next == cursor {
			return 0, errors.New("platform did not advance the credential cursor")
		}
		cursor = page.Next
	}
}

func (c *client) templates() *cobra.Command {
	group := &cobra.Command{Use: "templates", Short: "Manage immutable template versions"}
	group.AddCommand(c.list("list", "templates", false), c.update("register", "templates", "POST", false))
	return group
}

func (c *client) settings() *cobra.Command {
	group := &cobra.Command{Use: "settings", Short: "Read and update platform settings"}
	group.AddCommand(sub("get", "Get current settings", cobra.NoArgs, func(cmd *cobra.Command, _ []string) error {
		return c.show(cmd, "system/settings")
	}), c.update("patch", "system/settings", "PATCH", false))
	return group
}

func (c *client) lifecyclePolicy() *cobra.Command {
	group := &cobra.Command{Use: "lifecycle", Short: "Manage Worker lifecycle policies"}
	group.AddCommand(sub("status MACHINE", "Show effective policy, revisions and activity blockers", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		return c.showMachine(cmd, args[0], "/lifecycle")
	}), c.update("apply-default", "lifecycle-policy-applications", http.MethodPost, false),
		sub("application ID", "Show results for each selected machine", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
			return c.show(cmd, res("lifecycle-policy-applications", args[0]))
		}))
	var policyJSON string
	var revision int64
	set := sub("set MACHINE", "Update selected lifecycle policy fields", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		patch, err := jsonBody(strings.TrimSpace(policyJSON))
		if err != nil {
			return err
		}
		m, err := c.machine(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		policyRevision := revision
		if policyRevision == 0 {
			var status struct {
				Desired int64 `json:"desired_revision"`
			}
			if err := c.get(cmd.Context(), res("machines", m.ID)+"/lifecycle", &status); err != nil {
				return err
			}
			policyRevision = status.Desired
		}
		return c.mutation(cmd, http.MethodPatch, res("machines", m.ID), map[string]any{
			"expected_policy_revision": policyRevision, "expected_generation": m.Generation, "lifecycle_policy": patch,
		}, m.Revision)
	})
	set.Flags().StringVar(&policyJSON, "policy", "", "JSON object containing the fields to change")
	set.Flags().Int64Var(&revision, "policy-revision", 0, "Expected desired policy revision (default reads current revision)")
	group.AddCommand(set)
	return group
}

func (c *client) events() *cobra.Command {
	var last string
	cmd := sub("events", "Stream platform events", cobra.NoArgs, func(cmd *cobra.Command, _ []string) error {
		h := make(http.Header)
		h.Set("Accept", "text/event-stream")
		if last != "" {
			h.Set("Last-Event-ID", last)
		}
		resp, err := c.request(cmd.Context(), "GET", "events", nil, h)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, err = io.Copy(cmd.OutOrStdout(), resp.Body)
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	})
	cmd.Flags().StringVar(&last, "last-event-id", "", "Resume after a previous event ID")
	return cmd
}

type execOutput struct {
	ID     string                     `json:"exec_id"`
	Exit   *int                       `json:"exit_code"`
	Done   bool                       `json:"done"`
	Next   uint64                     `json:"next"`
	End    uint64                     `json:"end"`
	Error  string                     `json:"error"`
	Gap    *struct{ From, To uint64 } `json:"gap"`
	Output []struct {
		Stream string `json:"stream"`
		Data   []byte `json:"data"`
	} `json:"output"`
}

func (c *client) exec() *cobra.Command {
	var cwd, user string
	var timeout, wait int
	exec := sub("exec MACHINE -- COMMAND [ARG...]", "Run a native Guest command", cobra.MinimumNArgs(2), func(cmd *cobra.Command, args []string) error {
		m, err := c.machine(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		body := commandArgs(args[1:], cwd, user)
		body["timeout_seconds"], body["wait_seconds"] = timeout, wait
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		headers, err := c.intent(cmd.Context(), m.Revision)
		if err != nil {
			return err
		}
		headers.Set("Content-Type", "application/json")
		resp, err := c.request(cmd.Context(), "POST", res("machines", m.ID)+"/exec", strings.NewReader(string(raw)), headers)
		if err != nil {
			var api *apiError
			if !errors.As(err, &api) || api.Status >= 500 || api.Detail.Code == "OUTCOME_UNKNOWN" {
				c.printIntent(cmd, headers, 0)
			}
			return err
		}
		defer resp.Body.Close()
		var result json.RawMessage
		if err = json.NewDecoder(resp.Body).Decode(&result); err != nil {
			c.printIntent(cmd, headers, 0)
			return err
		}
		var output execOutput
		if err = json.Unmarshal(result, &output); err != nil {
			return err
		}
		if c.jsonOutput {
			if err = c.print(cmd, result); err != nil {
				return err
			}
			return execExit(output)
		}
		if err = c.writeExecOutput(cmd, output); err != nil {
			return err
		}
		if !output.Done {
			fmt.Fprintf(cmd.ErrOrStderr(), "Native exec %s at byte %d\n", output.ID, output.Next)
			return nil
		}
		id := output.ID
		for output.Next < output.End {
			cursor := output.Next
			output = execOutput{}
			if err = c.get(cmd.Context(), res("execs", id)+"/output?cursor="+strconv.FormatUint(cursor, 10), &output); err != nil {
				return err
			}
			if output.Next <= cursor && output.End > cursor {
				return errors.New("server did not advance native output cursor")
			}
			if err = c.writeExecOutput(cmd, output); err != nil {
				return err
			}
		}
		return execExit(output)
	})
	exec.Flags().StringVar(&cwd, "cwd", "/workspace", "Guest working directory")
	exec.Flags().StringVar(&user, "user", "root", "Guest user")
	exec.Flags().IntVar(&timeout, "timeout-seconds", 300, "Native command timeout")
	exec.Flags().IntVar(&wait, "wait-seconds", 10, "Wait for a short command before returning its native session ID")
	return exec
}

func execExit(output execOutput) error {
	if output.Error != "" {
		return errors.New(output.Error)
	}
	if output.Exit != nil && *output.Exit != 0 {
		return exitStatus(*output.Exit)
	}
	return nil
}

func (c *client) writeExecOutput(cmd *cobra.Command, output execOutput) error {
	if output.Gap != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Native output gap from byte %d to %d\n", output.Gap.From, output.Gap.To)
	}
	for _, entry := range output.Output {
		dest := cmd.OutOrStdout()
		if entry.Stream == "stderr" {
			dest = cmd.ErrOrStderr()
		}
		if _, err := dest.Write(entry.Data); err != nil {
			return err
		}
	}
	return nil
}

func (c *client) execs() *cobra.Command {
	group := &cobra.Command{Use: "execs", Short: "Inspect native Guest sessions"}
	group.AddCommand(c.list("list MACHINE", "execs", true), c.showItem("execs"))
	var cursor uint64
	output := sub("output EXEC", "Read native session output", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		path := res("execs", args[0]) + "/output?cursor=" + strconv.FormatUint(cursor, 10)
		if c.jsonOutput {
			return c.show(cmd, path)
		}
		var view execOutput
		if err := c.get(cmd.Context(), path, &view); err != nil {
			return err
		}
		if err := c.writeExecOutput(cmd, view); err != nil {
			return err
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "Native exec %s at byte %d\n", view.ID, view.Next)
		return execExit(view)
	})
	output.Flags().Uint64Var(&cursor, "cursor", 0, "Native output byte cursor")
	group.AddCommand(output, sub("terminate EXEC", "Terminate a native Guest session", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		return c.mutation(cmd, "DELETE", res("execs", args[0]), struct{}{}, 0)
	}))
	return group
}
