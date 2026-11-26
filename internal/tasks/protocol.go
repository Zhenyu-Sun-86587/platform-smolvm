package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

const (
	ProtocolVersion = 1
	SocketPath      = "/run/sandbox-taskd/control.sock"
	BootstrapPath   = "/usr/local/sbin/sandbox-bootstrap"
	TaskctlPath     = "/usr/local/bin/taskctl"
	DefaultWorkDir  = "/workspace"
	DefaultStateDir = "/var/lib/sandbox-taskd"
	documentLimit   = 2 << 20
)

type Binding struct {
	MachineID  string `json:"machine_id"`
	Generation int64  `json:"generation"`
	Namespace  string `json:"task_namespace"`
	Nonce      string `json:"nonce"`
}

type Spec struct {
	Argv         []string          `json:"argv"`
	Cwd          string            `json:"cwd,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	User         string            `json:"user,omitempty"`
	OutputPolicy string            `json:"output_policy,omitempty"`
	OutputLimit  int64             `json:"output_limit,omitempty"`
}

type Request struct {
	Method       string   `json:"method"`
	Protocol     int      `json:"protocol,omitempty"`
	Binding      *Binding `json:"binding,omitempty"`
	Source       *Binding `json:"source,omitempty"`
	Spec         *Spec    `json:"spec,omitempty"`
	TaskID       string   `json:"task_id,omitempty"`
	StartToken   string   `json:"start_token,omitempty"`
	SpecHash     string   `json:"spec_hash,omitempty"`
	Stream       string   `json:"stream,omitempty"`
	OperationID  string   `json:"operation_id,omitempty"`
	Policy       string   `json:"policy,omitempty"`
	StopEvidence string   `json:"stop_evidence,omitempty"`
	Offset       int64    `json:"offset,omitempty"`
	Limit        int      `json:"limit,omitempty"`
	GraceSeconds *int     `json:"grace_seconds,omitempty"`
}

type Task struct {
	ID              string    `json:"id"`
	RunID           string    `json:"run_id"`
	MachineID       string    `json:"machine_id"`
	Namespace       string    `json:"task_namespace"`
	Generation      int64     `json:"binding_generation"`
	State           string    `json:"state"`
	Reason          string    `json:"reason,omitempty"`
	ExitCode        *int      `json:"exit_code,omitempty"`
	Signal          string    `json:"signal,omitempty"`
	OriginTaskID    string    `json:"origin_task_id,omitempty"`
	OriginMachineID string    `json:"origin_machine_id,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	ObservedAt      time.Time `json:"observed_at"`
	Runner          *Identity `json:"runner,omitempty"`
	Stdout          LogIndex  `json:"stdout"`
	Stderr          LogIndex  `json:"stderr"`
}

func (t Task) Terminal() bool {
	switch t.State {
	case "succeeded", "failed", "cancelled", "interrupted":
		return true
	default:
		return false
	}
}

type Identity struct {
	BootID    string `json:"boot_id"`
	PID       int    `json:"pid"`
	StartTime string `json:"start_time"`
	Cgroup    string `json:"cgroup"`
	Inode     uint64 `json:"cgroup_inode"`
	Device    uint64 `json:"cgroup_device"`
	ChildPID  int    `json:"child_pid,omitempty"`
	ChildTime string `json:"child_start_time,omitempty"`
	Released  bool   `json:"released"`
}

type LogIndex struct {
	CommittedTo  int64 `json:"committed_to"`
	Dropped      int64 `json:"dropped"`
	RetainedFrom int64 `json:"retained_from,omitempty"`
}

type Output struct {
	Stream       string `json:"stream"`
	Data         []byte `json:"data"`
	Start        int64  `json:"start"`
	End          int64  `json:"end"`
	Next         int64  `json:"next"`
	RetainedFrom int64  `json:"retained_from"`
	CommittedTo  int64  `json:"committed_to"`
	Dropped      int64  `json:"dropped"`
	Gap          bool   `json:"gap"`
	Complete     bool   `json:"complete"`
}

type Hello struct {
	Protocol   int      `json:"protocol"`
	BootID     string   `json:"boot_id"`
	ManagerPID int      `json:"manager_pid"`
	Cgroup     bool     `json:"cgroup_v2_usable"`
	Reason     string   `json:"reason,omitempty"`
	Barrier    *Capture `json:"barrier,omitempty"`
	Transition string   `json:"pending_transition,omitempty"`
	Active     int      `json:"active"`
	Unknown    int      `json:"unknown"`
}

type FrozenTask struct {
	ID       string   `json:"id"`
	RunID    string   `json:"run_id"`
	Identity Identity `json:"identity"`
}

type Capture struct {
	OperationID string           `json:"operation_id"`
	Purpose     string           `json:"purpose"`
	Content     string           `json:"task_content"`
	Source      Binding          `json:"source"`
	Frozen      []FrozenTask     `json:"frozen,omitempty"`
	Processes   []CaptureProcess `json:"processes,omitempty"`
	Ready       bool             `json:"ready"`
}

type RPCError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (err *RPCError) Error() string {
	return err.Code + ": " + err.Message
}

type Response struct {
	Binding *Binding  `json:"binding,omitempty"`
	Task    *Task     `json:"task,omitempty"`
	Tasks   []Task    `json:"tasks,omitempty"`
	Output  *Output   `json:"output,omitempty"`
	Hello   *Hello    `json:"hello,omitempty"`
	Capture *Capture  `json:"capture,omitempty"`
	Error   *RPCError `json:"error,omitempty"`
}

type ExecFunc func(context.Context, []string, []byte) ([]byte, error)

func Call(ctx context.Context, exec ExecFunc, request Request) (Response, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return Response{}, err
	}
	out, err := exec(ctx, []string{TaskctlPath, "rpc"}, body)
	if err != nil {
		return Response{}, fmt.Errorf("taskctl exec: %w", err)
	}
	return decodeResponse(bytes.NewReader(out))
}

func Send(ctx context.Context, socket string, input io.Reader) (Response, error) {
	dialer := net.Dialer{}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socket)
		},
	}
	defer transport.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://taskd/rpc", input)
	if err != nil {
		return Response{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	reply, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		return Response{}, err
	}
	defer reply.Body.Close()
	if reply.StatusCode != http.StatusOK {
		return Response{}, fmt.Errorf("taskd: %s", reply.Status)
	}
	return decodeResponse(reply.Body)
}

func writeResponse(w io.Writer, reply Response) {
	_ = json.NewEncoder(w).Encode(reply)
}

func decodeResponse(input io.Reader) (Response, error) {
	var reply Response
	if err := decode(input, &reply); err != nil {
		return Response{}, err
	}
	if reply.Error != nil {
		return reply, reply.Error
	}
	return reply, nil
}

func decode(input io.Reader, value any) error {
	body, err := io.ReadAll(io.LimitReader(input, documentLimit+1))
	if err != nil {
		return err
	}
	if len(body) > documentLimit {
		return fmt.Errorf("control document exceeds %d bytes", documentLimit)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("control document contains trailing data")
	}
	return nil
}
