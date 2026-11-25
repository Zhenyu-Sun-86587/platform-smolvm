package tasks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

type DaemonConfig struct {
	StateDir   string
	Socket     string
	Runner     string
	CgroupRoot string
}

type manager struct {
	mu      sync.Mutex
	j       journal
	root    string
	runner  string
	boot    string
	probe   error
	binding *Binding
	barrier *Capture
}

func Serve(ctx context.Context, config DaemonConfig) error {
	if config.StateDir == "" {
		config.StateDir = DefaultStateDir
	}
	if config.Socket == "" {
		config.Socket = SocketPath
	}
	if config.Runner == "" {
		config.Runner = "/usr/local/bin/task-runner"
	}
	if config.CgroupRoot == "" {
		config.CgroupRoot = defaultCgroupRoot
	}
	if err := mkdir(filepath.Join(config.StateDir, "runs")); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(config.StateDir, "manager.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("taskd lock: %w", err)
	}
	boot, err := bootID()
	if err != nil {
		return err
	}
	m := &manager{j: journal{config.StateDir}, root: filepath.Clean(config.CgroupRoot), runner: config.Runner, boot: boot}
	var binding Binding
	if err := readJSON(filepath.Join(config.StateDir, "binding.json"), &binding); err == nil {
		m.binding = &binding
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var barrier Capture
	if err := readJSON(filepath.Join(config.StateDir, "barrier.json"), &barrier); err == nil {
		m.barrier = &barrier
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	m.probe = probeCgroup(m.root)
	if err := mkdir(filepath.Dir(config.Socket)); err != nil {
		return err
	}
	if file, err := os.Lstat(config.Socket); err == nil {
		if file.Mode()&os.ModeSocket == 0 {
			return errors.New("taskd control path is not a socket")
		}
		if err := os.Remove(config.Socket); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	socket, err := net.Listen("unix", config.Socket)
	if err != nil {
		return err
	}
	defer socket.Close()
	if err := os.Chmod(config.Socket, 0600); err != nil {
		return err
	}
	server := &http.Server{
		Handler:           http.HandlerFunc(m.serveRPC),
		ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Second,
	}
	daemonContext, stop := context.WithCancel(ctx)
	defer stop()
	go m.reconcile(daemonContext)
	go func() {
		<-daemonContext.Done()
		drain, finish := context.WithTimeout(context.Background(), 5*time.Second)
		defer finish()
		if server.Shutdown(drain) != nil {
			_ = server.Close()
		}
	}()
	err = server.Serve(socket)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (m *manager) serveRPC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/rpc" {
		http.NotFound(w, r)
		return
	}
	var request Request
	if err := decode(r.Body, &request); err != nil {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintln(w, `{"error":{"code":"INVALID_REQUEST","message":"invalid task RPC document"}}`)
		return
	}
	m.mu.Lock()
	reply, err := m.handle(request)
	m.mu.Unlock()
	if err != nil {
		var failure *RPCError
		if !errors.As(err, &failure) {
			failure = &RPCError{Code: "OUTCOME_UNKNOWN", Message: err.Error()}
		}
		reply.Error = failure
	}
	w.Header().Set("Content-Type", "application/json")
	writeResponse(w, reply)
}

func rpcError(code, message string) error {
	return &RPCError{Code: code, Message: message}
}

func (m *manager) handle(request Request) (Response, error) {
	if request.Protocol != 0 && request.Protocol != ProtocolVersion {
		return Response{}, rpcError("INVALID_REQUEST", "task protocol version does not match")
	}
	if request.Method == "hello" {
		summary := Hello{Protocol: ProtocolVersion, BootID: m.boot, ManagerPID: os.Getpid(), Cgroup: m.probe == nil, Barrier: m.barrier}
		if m.probe != nil {
			summary.Reason = m.probe.Error()
		}
		tasks, err := m.list()
		if err != nil {
			return Response{}, err
		}
		for _, task := range tasks {
			if task.State == "unknown" {
				summary.Unknown++
			}
			if !task.Terminal() {
				summary.Active++
			}
		}
		return Response{Hello: &summary, Binding: m.binding}, nil
	}
	if request.Method == "bind" {
		return m.bind(request.Binding)
	}
	switch request.Method {
	case "adopt_live_fork", "complete_copy", "complete_recovery":
		return m.adopt(request)
	}
	if request.Binding == nil || m.binding == nil || *request.Binding != *m.binding {
		return Response{}, rpcError("BINDING_MISMATCH", "expected binding does not match guest")
	}
	transition, err := m.readTransition()
	if err != nil {
		return Response{}, err
	}
	if transition != nil && request.Method != "list" && request.Method != "status" && request.Method != "output" {
		return Response{}, rpcError("BINDING_MISMATCH", "a guest binding transition is pending")
	}
	switch request.Method {
	case "start":
		return m.start(request)
	case "list":
		tasks, err := m.list()
		return Response{Tasks: tasks}, err
	case "status", "output", "cancel":
		return m.taskRPC(request)
	case "prepare_copy", "prepare_recovery_capture":
		return m.prepareCapture(request)
	case "finish_capture":
		return m.finishCapture(request)
	default:
		return Response{}, rpcError("INVALID_REQUEST", "unknown task RPC method")
	}
}

func validBinding(b *Binding) bool {
	return b != nil && b.Generation > 0 && identifier.MatchString(b.MachineID) &&
		identifier.MatchString(b.Namespace) && len(b.Nonce) > 0 && len(b.Nonce) <= 256
}

func (m *manager) bind(binding *Binding) (Response, error) {
	if !validBinding(binding) {
		return Response{}, rpcError("INVALID_REQUEST", "invalid guest binding")
	}
	if m.binding != nil {
		if *m.binding == *binding {
			return Response{Binding: m.binding}, nil
		}
		return Response{}, rpcError("BINDING_MISMATCH", "guest already has a binding")
	}
	if err := m.setBinding(*binding); err != nil {
		return Response{}, err
	}
	return Response{Binding: m.binding}, nil
}

func (m *manager) setBinding(binding Binding) error {
	if err := mkdir(m.j.indices(binding.Namespace)); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(m.j.root, "binding.json"), binding); err != nil {
		return err
	}
	m.binding = &binding
	return nil
}

func (m *manager) start(r Request) (Response, error) {
	if r.Spec == nil || !identifier.MatchString(r.TaskID) || r.StartToken == "" || len(r.StartToken) > 512 {
		return Response{}, rpcError("INVALID_REQUEST", "start requires a task ID, token, and spec")
	}
	spec, err := normalize(*r.Spec)
	if err != nil {
		return Response{}, rpcError("INVALID_REQUEST", err.Error())
	}
	specHash, err := HashSpec(spec)
	if err != nil {
		return Response{}, err
	}
	if r.SpecHash != "" && r.SpecHash != specHash {
		return Response{}, rpcError("INVALID_REQUEST", "task spec hash does not match")
	}
	namespace := m.binding.Namespace
	runID := deriveID("run_", namespace, r.StartToken)
	var record taskRequest
	requestPath := m.j.run(runID, "request.json")
	if err := readJSON(requestPath, &record); err == nil {
		if record.Binding != *m.binding || record.SpecHash != specHash || record.StartToken != r.StartToken {
			return Response{Task: nil}, rpcError("IDEMPOTENCY_CONFLICT", "start token is already used with a different spec")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Response{}, err
	} else {
		var existing taskIndex
		if err := readJSON(m.j.index(namespace, r.TaskID), &existing); err == nil {
			return Response{}, rpcError("IDEMPOTENCY_CONFLICT", "task ID is already used")
		} else if !errors.Is(err, os.ErrNotExist) {
			return Response{}, err
		}
		if err := m.startGate(); err != nil {
			return Response{}, err
		}
		free, err := diskAvailable(m.j.root)
		if err != nil {
			return Response{}, err
		}
		allocated, err := m.j.outputAllocated()
		if err != nil {
			return Response{}, err
		}
		if free < minimumFreeSpace+spec.OutputLimit || allocated+spec.OutputLimit > guestOutputLimit {
			return Response{}, rpcError("TASK_STORAGE_LIMIT", "insufficient guest storage for requested task output")
		}
		if _, err := guestUser(spec.User); err != nil {
			return Response{}, rpcError("INVALID_REQUEST", "requested Guest user is unavailable")
		}
		record = taskRequest{ID: r.TaskID, RunID: runID, Binding: *m.binding, StartToken: r.StartToken, SpecHash: specHash, Spec: spec, CreatedAt: time.Now().UTC(), BootID: m.boot}
		for _, path := range []string{m.j.run(runID), m.j.run(runID, "stdout"), m.j.run(runID, "stderr")} {
			if err := mkdir(path); err != nil {
				return Response{}, err
			}
		}
		if err := writeJSON(requestPath, record); err != nil {
			return Response{}, err
		}
	}
	index := taskIndex{ID: record.ID, RunID: runID, StartToken: record.StartToken, SpecHash: record.SpecHash}
	var existing taskIndex
	if err := readJSON(m.j.index(namespace, index.ID), &existing); errors.Is(err, os.ErrNotExist) {
		if err := writeJSON(m.j.run(runID, "owner.json"), taskOwner{Binding: *m.binding, TaskID: index.ID}); err != nil {
			return Response{}, err
		}
		if err := writeJSON(m.j.index(namespace, index.ID), index); err != nil {
			return Response{}, err
		}
	} else if err != nil {
		return Response{}, err
	} else if existing != index {
		return Response{}, rpcError("IDEMPOTENCY_CONFLICT", "task ID is already used")
	}
	dispatchPath := m.j.run(runID, "dispatch.json")
	var dispatch taskRequest
	if err := readJSON(dispatchPath, &dispatch); errors.Is(err, os.ErrNotExist) && record.BootID == m.boot {
		if err := m.startGate(); err != nil {
			return Response{}, err
		}
		dispatch = record
		dispatch.CreatedAt = time.Now().UTC()
		if err := writeJSON(dispatchPath, dispatch); err != nil {
			return Response{}, err
		}
		cmd := exec.Command(m.runner, "run", m.j.root, runID, m.root)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			return Response{}, fmt.Errorf("runner dispatch pending verification: %w", err)
		}
		go func() {
			if err := cmd.Wait(); err != nil {
				slog.Warn("task runner exited without success", "run_id", runID, "error", err)
			}
		}()
		until := time.Now().Add(2 * time.Second)
		for time.Now().Before(until) {
			t, err := m.inspect(*m.binding, index)
			if err != nil {
				return Response{}, err
			}
			if t.State != "starting" {
				return Response{Task: &t}, nil
			}
			time.Sleep(20 * time.Millisecond)
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Response{}, err
	}
	t, err := m.inspect(*m.binding, index)
	return Response{Task: &t}, err
}

func (m *manager) startGate() error {
	if m.barrier != nil {
		return rpcError("TASK_CAPTURE_PENDING", "task starts are closed for a capture operation")
	}
	if m.probe != nil {
		return rpcError("GUEST_MANAGEMENT_UNAVAILABLE", m.probe.Error())
	}
	return nil
}

func (m *manager) list() ([]Task, error) {
	if m.binding == nil {
		return []Task{}, nil
	}
	indices, err := m.j.taskIndices(m.binding.Namespace)
	if err != nil {
		return nil, err
	}
	tasks := make([]Task, 0, len(indices))
	for _, index := range indices {
		t, err := m.inspect(*m.binding, index)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, nil
}

func (m *manager) taskRPC(request Request) (Response, error) {
	if !identifier.MatchString(request.TaskID) {
		return Response{}, rpcError("INVALID_REQUEST", "invalid task ID")
	}
	var index taskIndex
	err := readJSON(m.j.index(m.binding.Namespace, request.TaskID), &index)
	if errors.Is(err, os.ErrNotExist) {
		return Response{}, rpcError("NOT_FOUND", "task is not in the current namespace")
	}
	if err != nil || index.ID != request.TaskID || !identifier.MatchString(index.RunID) {
		return Response{}, errors.Join(err, errors.New("invalid task index"))
	}
	t, err := m.inspect(*m.binding, index)
	if err != nil {
		return Response{}, err
	}
	switch request.Method {
	case "status":
		return Response{Task: &t}, nil
	case "output":
		if request.Stream == "" {
			request.Stream = "stdout"
		}
		if request.Limit == 0 {
			request.Limit = 64 << 10
		}
		if (request.Stream != "stdout" && request.Stream != "stderr") || request.Offset < 0 || request.Limit < 1 || request.Limit > 512<<10 {
			return Response{}, rpcError("INVALID_REQUEST", "invalid output stream, offset, or limit")
		}
		output, err := m.j.readOutput(t, request.Stream, request.Offset, request.Limit)
		return Response{Output: &output}, err
	default:
		return m.cancel(request, index, t)
	}
}
