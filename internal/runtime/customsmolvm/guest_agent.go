package customsmolvm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	rt "sandbox-platform/internal/runtime"
)

var _ rt.NativeGuestControl = (*Adapter)(nil)

const guestPayloadLimit = 512 << 10

func (a *Adapter) guestRequest(ctx context.Context, ref rt.ExecutionRef, method string, request, result any, timeout time.Duration) error {
	if !validExecution(ref) {
		return fmt.Errorf("%w: Guest Agent requires an exact execution", rt.ErrInvalid)
	}
	payload, err := json.Marshal(struct {
		Expected rt.ExecutionRef `json:"expected"`
		Request  any             `json:"request"`
	}{ref, request})
	if err != nil {
		return err
	}
	endpoint, _, err := a.connect(ctx)
	if err != nil {
		return err
	}
	defer endpoint.CloseIdleConnections()
	endpoint.Timeout = timeout
	var reply struct {
		Execution rt.ExecutionRef `json:"execution"`
		Result    json.RawMessage `json:"result"`
	}
	if err := exchange(ctx, endpoint, http.MethodPost, extensionPath+"/instances/"+ref.InstanceID+"/guest/"+method, payload, &reply); err != nil {
		return err
	}
	if reply.Execution != ref || len(reply.Result) == 0 || string(reply.Result) == "null" {
		return fmt.Errorf("%w: Guest Agent response differs from the requested execution", rt.ErrUnsupported)
	}
	if err := json.Unmarshal(reply.Result, result); err != nil {
		return fmt.Errorf("%w: invalid Guest Agent response: %v", rt.ErrUnsupported, err)
	}
	return nil
}

func (a *Adapter) GuestPing(ctx context.Context, ref rt.ExecutionRef) error {
	var reply struct {
		Protocol int `json:"protocol"`
	}
	if err := a.guestRequest(ctx, ref, "ping", struct{}{}, &reply, 5*time.Second); err != nil {
		return err
	}
	if reply.Protocol != 1 {
		return fmt.Errorf("%w: incompatible Guest Agent protocol", rt.ErrUnsupported)
	}
	return nil
}

func (a *Adapter) GuestExec(ctx context.Context, ref rt.ExecutionRef, exec rt.ExecRequest) (rt.ExecResult, error) {
	if exec.TimeoutSeconds == 0 {
		exec.TimeoutSeconds = 30
	}
	if exec.Background || exec.User != "" || len(exec.Command) == 0 || exec.TimeoutSeconds > 120 || len(exec.Stdin) > guestPayloadLimit {
		return rt.ExecResult{}, fmt.Errorf("%w: invalid foreground Guest Agent command options", rt.ErrInvalid)
	}
	request := struct {
		Command []string          `json:"command"`
		Env     map[string]string `json:"env,omitempty"`
		Cwd     string            `json:"cwd,omitempty"`
		Stdin   string            `json:"stdin,omitempty"`
		Timeout uint64            `json:"timeout_seconds"`
	}{exec.Command, exec.Env, exec.Cwd, exec.Stdin, exec.TimeoutSeconds}
	var result rt.ExecResult
	err := a.guestRequest(ctx, ref, "exec", request, &result, time.Duration(exec.TimeoutSeconds)*time.Second+5*time.Second)
	return result, err
}

func (a *Adapter) GuestUpload(ctx context.Context, ref rt.ExecutionRef, path string, source io.Reader, options rt.FileOptions) error {
	if options.UID != nil || options.GID != nil || options.Size > guestPayloadLimit || !strings.HasPrefix(path, "/") {
		return fmt.Errorf("%w: invalid Guest Agent file options", rt.ErrInvalid)
	}
	data, err := io.ReadAll(io.LimitReader(source, guestPayloadLimit+1))
	if err != nil {
		return err
	}
	if len(data) > guestPayloadLimit || (options.Size >= 0 && int64(len(data)) != options.Size) {
		return fmt.Errorf("%w: Guest Agent file size does not match the intended upload", rt.ErrInvalid)
	}
	request := struct {
		Path string  `json:"path"`
		Data []byte  `json:"data"`
		Mode *uint32 `json:"mode,omitempty"`
	}{path, data, options.Mode}
	var result struct {
		Bytes *int `json:"bytes_written"`
	}
	if err := a.guestRequest(ctx, ref, "upload", request, &result, 35*time.Second); err != nil {
		return err
	}
	if result.Bytes == nil || *result.Bytes != len(data) {
		return fmt.Errorf("%w: Guest Agent upload receipt differs from the intended size", rt.ErrUnsupported)
	}
	return nil
}
