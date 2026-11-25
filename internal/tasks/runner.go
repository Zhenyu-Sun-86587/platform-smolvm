package tasks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"
)

type ownerAck struct {
	Owner    taskOwner `json:"owner"`
	Identity Identity  `json:"identity"`
}

func LaunchPayload(stateDir, runID, root string) error {
	if !identifier.MatchString(runID) {
		return errors.New("invalid run ID")
	}
	var request taskRequest
	j := journal{stateDir}
	if err := readJSON(j.run(runID, "request.json"), &request); err != nil {
		return err
	}
	gate := os.NewFile(3, "launch-gate")
	handshake := os.NewFile(4, "launch-handshake")
	defer gate.Close()
	defer handshake.Close()
	syscall.CloseOnExec(4)
	cgroup, err := payloadPath(root, runID)
	if err != nil {
		return err
	}
	if err := writeCgroup(cgroup, "cgroup.procs", strconv.Itoa(os.Getpid())); err != nil {
		return err
	}
	if _, err := handshake.Write([]byte{1}); err != nil {
		return err
	}
	release := []byte{0}
	if _, err := io.ReadFull(gate, release); err != nil || release[0] != 1 {
		return errors.New("runner did not release launch gate")
	}
	gate.Close()
	err = execPayload(request.Spec)
	// This descriptor closes on successful exec. A message after the
	// initial membership byte reports a known pre-exec failure.
	_, _ = fmt.Fprint(handshake, err)
	return err
}

func payloadPath(root, id string) (string, error) {
	if !identifier.MatchString(id) {
		return "", errors.New("invalid run ID")
	}
	return root + "/" + payloadPrefix + id, nil
}

func Run(stateDir, runID, root string) error {
	if root == "" {
		root = defaultCgroupRoot
	}
	if !identifier.MatchString(runID) {
		return errors.New("invalid run ID")
	}
	j := journal{stateDir}
	lock, err := os.OpenFile(j.run(runID, "runner.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return err
	}
	var request taskRequest
	if err := readJSON(j.run(runID, "request.json"), &request); err != nil {
		return err
	}
	var dispatch taskRequest
	if err := readJSON(j.run(runID, "dispatch.json"), &dispatch); err != nil {
		return err
	}
	boot, err := bootID()
	if err != nil || request.RunID != runID || request.BootID != boot || dispatch.RunID != runID || dispatch.SpecHash != request.SpecHash {
		return errors.New("runner has no matching dispatch in this boot")
	}
	marker, err := os.OpenFile(j.run(runID, "runner.once"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err = marker.Sync(); err != nil {
		marker.Close()
		return err
	}
	marker.Close()
	if err := syncDir(j.run(runID)); err != nil {
		return err
	}

	terminal, err := runPayload(j, request, root)
	if err != nil {
		return err
	}
	terminal.Finished = time.Now().UTC()
	if err := writeJSON(j.run(runID, "result.json"), terminal); err != nil {
		return err
	}
	// The receipt is durable before removal. A cgroup with unexpected
	// subgroups can remain here; no task reuses this run's name.
	_ = os.Remove(root + "/" + payloadPrefix + runID)
	return nil
}

func runPayload(j journal, request taskRequest, root string) (result, error) {
	runID := request.RunID
	failed := func(err error) (result, error) {
		return result{State: "failed", Reason: "launch_failed: " + err.Error()}, nil
	}
	path, err := makeCgroup(root, runID)
	if err != nil {
		return failed(err)
	}
	inode, device, err := cgroupIdentity(path)
	if err != nil {
		return failed(err)
	}
	start, err := processStart(os.Getpid())
	if err != nil {
		return failed(err)
	}
	identity := Identity{BootID: request.BootID, PID: os.Getpid(), StartTime: start, Cgroup: path, Inode: inode, Device: device}
	if err := writeJSON(j.run(runID, "runner.json"), identity); err != nil {
		return result{}, err
	}
	executable, err := os.Executable()
	if err != nil {
		return failed(err)
	}
	cmd := exec.Command(executable, "launch", j.root, runID, root)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	var pipes []*os.File
	defer func() {
		for _, file := range pipes {
			_ = file.Close()
		}
	}()
	for i := 0; i < 4; i++ {
		reader, writer, err := os.Pipe()
		if err != nil {
			return failed(err)
		}
		pipes = append(pipes, reader, writer)
	}
	gateReader, gateWriter, readyReader, readyWriter := pipes[0], pipes[1], pipes[2], pipes[3]
	stdoutReader, stdoutWriter, stderrReader, stderrWriter := pipes[4], pipes[5], pipes[6], pipes[7]
	cmd.ExtraFiles = []*os.File{gateReader, readyWriter}
	cmd.Stdout, cmd.Stderr = stdoutWriter, stderrWriter
	if err := cmd.Start(); err != nil {
		return failed(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	gateReader.Close()
	readyWriter.Close()
	stdoutWriter.Close()
	stderrWriter.Close()

	_ = readyReader.SetReadDeadline(time.Now().Add(5 * time.Second))
	ready := []byte{0}
	if _, err := io.ReadFull(readyReader, ready); err != nil || ready[0] != 1 {
		return failed(errors.New("payload did not join its cgroup"))
	}
	identity.ChildPID = cmd.Process.Pid
	identity.ChildTime, err = processStart(identity.ChildPID)
	if err != nil {
		return failed(err)
	}
	if err := writeJSON(j.run(runID, "runner.json"), identity); err != nil {
		return result{}, err
	}
	if _, err := gateWriter.Write([]byte{1}); err != nil {
		return failed(err)
	}
	gateWriter.Close()
	launchErr, err := io.ReadAll(io.LimitReader(readyReader, 4096))
	if err != nil {
		_ = killCgroup(path)
		return result{}, fmt.Errorf("launch handshake: %w", err)
	}
	if len(launchErr) == 0 {
		identity.Released = true
		if err := writeJSON(j.run(runID, "runner.json"), identity); err != nil {
			_ = killCgroup(path)
			return result{}, err
		}
	}
	readyReader.Close()

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go watchOwner(ctx, j, runID, identity)
	budget := &logBudget{remaining: request.Spec.OutputLimit}
	var limitOnce sync.Once
	onDrop := func() {
		if request.Spec.OutputPolicy == "cancel" {
			limitOnce.Do(func() {
				if err := writeJSON(j.run(runID, "output-limit.json"), cancelIntent{Reason: "output_limit"}); err == nil {
					_ = killCgroup(path)
				}
			})
		}
	}
	type collected struct {
		index LogIndex
		err   error
	}
	stdout, stderr := make(chan collected, 1), make(chan collected, 1)
	go func() {
		index, err := collect(j, runID, "stdout", stdoutReader, budget, onDrop)
		stdout <- collected{index, err}
	}()
	go func() {
		index, err := collect(j, runID, "stderr", stderrReader, budget, onDrop)
		stderr <- collected{index, err}
	}()
	waitErr := cmd.Wait()
	state, groupErr := readCgroup(path)
	if groupErr == nil && state.populated {
		groupErr = killCgroup(path)
	}
	if groupErr == nil {
		groupErr = waitCgroup(path, func(state cgroupState) bool { return !state.populated })
	}
	// A process moved out of its cgroup may still hold output descriptors.
	// Bound the wait and surface incomplete evidence as unknown.
	_ = stdoutReader.SetReadDeadline(time.Now().Add(3 * time.Second))
	_ = stderrReader.SetReadDeadline(time.Now().Add(3 * time.Second))
	out, errOut := <-stdout, <-stderr
	if err := errors.Join(groupErr, out.err, errOut.err); err != nil {
		return result{}, err
	}
	terminal := result{State: "succeeded", Stdout: out.index, Stderr: errOut.index}
	if len(launchErr) > 0 {
		terminal.State = "failed"
		terminal.Reason = "launch_failed: " + string(launchErr)
		return terminal, nil
	}
	waitStatus, known := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !known {
		return result{}, errors.New("payload wait status is unavailable")
	}
	switch {
	case waitStatus.Exited():
		code := waitStatus.ExitStatus()
		terminal.ExitCode = &code
		if code != 0 {
			terminal.State = "failed"
		}
	case waitStatus.Signaled():
		terminal.State = "failed"
		terminal.Signal = waitStatus.Signal().String()
	default:
		return result{}, fmt.Errorf("payload exit status is unknown: %v", waitErr)
	}
	var cancellation cancelIntent
	for _, marker := range []string{"cancel.json", "output-limit.json"} {
		err := readJSON(j.run(runID, marker), &cancellation)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return result{}, err
		}
		terminal.State = "cancelled"
		terminal.Reason = cancellation.Reason
		if cancellation.Reason == "interrupted_by_restore" {
			terminal.State = "interrupted"
			terminal.ExitCode = nil
		}
		break
	}
	return terminal, nil
}

func watchOwner(ctx context.Context, j journal, runID string, identity Identity) {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	var old taskOwner
	for {
		var owner taskOwner
		if err := readJSON(j.run(runID, "owner.json"), &owner); err == nil && owner != old {
			if writeJSON(j.run(runID, "owner-ack.json"), ownerAck{Owner: owner, Identity: identity}) == nil {
				old = owner
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
