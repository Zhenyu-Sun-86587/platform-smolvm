package smolvm

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/coder/websocket"

	"sandbox-platform/internal/runtime"
)

var execIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

type execBody struct {
	Command        []string          `json:"command"`
	Env            map[string]string `json:"env,omitempty"`
	Cwd            string            `json:"cwd,omitempty"`
	User           string            `json:"user"`
	TimeoutSeconds uint64            `json:"timeoutSeconds,omitempty"`
	Stdin          string            `json:"stdin,omitempty"`
	AutoStart      bool              `json:"autoStart"`
}

func buildExecBody(request runtime.ExecRequest) (execBody, error) {
	if len(request.Command) == 0 || request.Command[0] == "" {
		return execBody{}, invalid("guest command is required")
	}
	user := request.User
	if user == "" {
		user = "0:0"
	}
	return execBody{Command: request.Command, Env: request.Env, Cwd: request.Cwd,
		User: user, TimeoutSeconds: request.TimeoutSeconds, Stdin: request.Stdin}, nil
}

func (client *Client) Exec(ctx context.Context, name string, request runtime.ExecRequest) (runtime.ExecResult, error) {
	execute := func(ctx context.Context) (runtime.ExecResult, error) { return client.exec(ctx, name, request) }
	result, err := execute(ctx)
	return retryAttachment(ctx, client, name, noRunningHandle, result, err, execute)
}

func (client *Client) exec(ctx context.Context, name string, request runtime.ExecRequest) (runtime.ExecResult, error) {
	path, err := machinePath(name, "/exec")
	if err != nil {
		return runtime.ExecResult{}, err
	}
	exec, err := buildExecBody(request)
	if err != nil {
		return runtime.ExecResult{}, err
	}
	body := struct {
		execBody
		Background bool `json:"background,omitempty"`
	}{exec, request.Background}
	var result struct {
		ExitCode *int   `json:"exitCode"`
		Stdout   []byte `json:"stdoutB64"`
		Stderr   []byte `json:"stderrB64"`
	}
	if err := client.json(ctx, http.MethodPost, path, nil, body, &result); err != nil {
		return runtime.ExecResult{}, err
	}
	if result.ExitCode == nil {
		return runtime.ExecResult{}, protocolError("exec response did not include an exit code", true, nil)
	}
	response := runtime.ExecResult{ExitCode: *result.ExitCode, Stdout: result.Stdout, Stderr: result.Stderr}
	if request.Background && response.ExitCode == 0 {
		line, ok := bytes.CutPrefix(bytes.TrimSpace(response.Stdout), []byte("pid="))
		pid, err := strconv.Atoi(string(line))
		if !ok || err != nil || pid <= 0 {
			return response, protocolError("background exec response did not include a valid PID", true, nil)
		}
		response.PID = &pid
	}
	return response, nil
}

func execPath(name, id, suffix string) (string, error) {
	if !execIDPattern.MatchString(id) {
		return "", invalid("exec ID must contain 8 to 64 ASCII letters, digits, hyphens or underscores")
	}
	return machinePath(name, "/execs/"+id+suffix)
}

type nativeExecSession struct {
	ID           string                `json:"execId"`
	State        string                `json:"state"`
	Done         bool                  `json:"done"`
	ExitCode     *int                  `json:"exitCode"`
	Error        *string               `json:"error"`
	Output       []runtime.OutputChunk `json:"output"`
	Next         uint64                `json:"next"`
	Start        uint64                `json:"start"`
	End          uint64                `json:"end"`
	Lost         *runtime.OutputGap    `json:"lost"`
	StdinApplied uint64                `json:"stdinApplied"`
	TTY          bool                  `json:"tty"`
	Interactive  bool                  `json:"interactive"`
	CreatedAt    uint64                `json:"createdAt"`
}

func (session nativeExecSession) result(id string, mutates bool) (runtime.ExecSession, error) {
	if session.ID != id || session.State == "" {
		return runtime.ExecSession{}, protocolError("runtime response did not identify the expected exec session", mutates, nil)
	}
	return runtime.ExecSession(session), nil
}

func (client *Client) StartExec(ctx context.Context, name string, request runtime.ExecSessionRequest) (runtime.ExecSession, error) {
	start := func(ctx context.Context) (runtime.ExecSession, error) { return client.startExec(ctx, name, request) }
	session, err := start(ctx)
	return retryAttachment(ctx, client, name, noRunningHandle, session, err, start)
}

func (client *Client) startExec(ctx context.Context, name string, request runtime.ExecSessionRequest) (runtime.ExecSession, error) {
	path, err := machinePath(name, "/execs")
	if err != nil {
		return runtime.ExecSession{}, err
	}
	if !execIDPattern.MatchString(request.ID) || request.Background || request.WaitSeconds > 60 || request.MaxBytes < 0 {
		return runtime.ExecSession{}, invalid("invalid exec session ID or options")
	}
	exec, err := buildExecBody(request.ExecRequest)
	if err != nil {
		return runtime.ExecSession{}, err
	}
	body := struct {
		execBody
		ID          string `json:"execId"`
		Interactive bool   `json:"interactive,omitempty"`
		TTY         bool   `json:"tty,omitempty"`
		Cols        uint16 `json:"cols,omitempty"`
		Rows        uint16 `json:"rows,omitempty"`
		WaitSeconds uint64 `json:"waitSeconds,omitempty"`
		MaxBytes    int    `json:"maxBytes,omitempty"`
	}{exec, request.ID, request.Interactive, request.TTY, request.Cols, request.Rows, request.WaitSeconds, request.MaxBytes}
	var result nativeExecSession
	if err := client.json(ctx, http.MethodPost, path, nil, body, &result); err != nil {
		return runtime.ExecSession{}, err
	}
	return result.result(request.ID, true)
}

// ReadExec acknowledges earlier output. Use one reader per native session.
func (client *Client) ReadExec(ctx context.Context, name, id string, request runtime.ExecRead) (runtime.ExecSession, error) {
	path, err := execPath(name, id, "")
	if err != nil {
		return runtime.ExecSession{}, err
	}
	if request.Wait > 60 || request.Max < 0 {
		return runtime.ExecSession{}, invalid("invalid exec reader options")
	}
	query := url.Values{"after": {strconv.FormatUint(request.After, 10)}}
	if request.Wait != 0 {
		query.Set("wait", strconv.FormatUint(request.Wait, 10))
	}
	if request.Max != 0 {
		query.Set("max", strconv.Itoa(request.Max))
	}
	var result nativeExecSession
	if err := client.json(ctx, http.MethodGet, path, query, nil, &result); err != nil {
		return runtime.ExecSession{}, err
	}
	return result.result(id, false)
}

func (client *Client) WriteExec(ctx context.Context, name, id string, seq uint64, data []byte, eof bool) (uint64, error) {
	path, err := execPath(name, id, "/stdin")
	if err != nil {
		return 0, err
	}
	if seq == 0 || len(data) > 64<<10 {
		return 0, invalid("invalid exec stdin frame")
	}
	body := struct {
		Seq  uint64 `json:"seq"`
		Data []byte `json:"data,omitempty"`
		EOF  bool   `json:"eof"`
	}{seq, data, eof}
	var result struct {
		StdinApplied uint64 `json:"stdinApplied"`
	}
	err = client.json(ctx, http.MethodPost, path, nil, body, &result)
	return result.StdinApplied, err
}

func (client *Client) ResizeExec(ctx context.Context, name, id string, cols, rows uint16) error {
	path, err := execPath(name, id, "/resize")
	if err != nil {
		return err
	}
	if cols == 0 || rows == 0 {
		return invalid("terminal dimensions must be positive")
	}
	body := struct {
		Cols uint16 `json:"cols"`
		Rows uint16 `json:"rows"`
	}{cols, rows}
	return client.json(ctx, http.MethodPost, path, nil, body, nil)
}

func (client *Client) DeleteExec(ctx context.Context, name, id string) error {
	path, err := execPath(name, id, "")
	if err != nil {
		return err
	}
	return client.json(ctx, http.MethodDelete, path, nil, nil, nil)
}

// AttachExec opens one native reader. The caller owns fan-out to platform clients.
func (client *Client) AttachExec(ctx context.Context, name, id string, after uint64) (*websocket.Conn, error) {
	path, err := execPath(name, id, "/attach")
	if err != nil {
		return nil, err
	}
	query := url.Values{"after": {strconv.FormatUint(after, 10)}}
	address := "ws://smolvm" + path + encodedQuery(query)
	connection, response, err := websocket.Dial(ctx, address, &websocket.DialOptions{HTTPClient: client.http})
	if err == nil {
		return connection, nil
	}
	if response != nil && response.StatusCode != http.StatusSwitchingProtocols {
		defer response.Body.Close()
		return nil, statusError(response, false)
	}
	return nil, &runtime.Error{Code: "RUNTIME_UNAVAILABLE", Message: strings.TrimSpace(err.Error()), Cause: err}
}
