package customsmolvm

import (
	"context"
	"io"

	rt "sandbox-platform/internal/runtime"
)

// unimplemented keeps legacy mutators closed until their native handlers and
// Worker admission are available.
type unimplemented struct{}

func (unimplemented) Create(ctx context.Context, request rt.CreateRequest) (rt.Observation, error) {
	return rt.Observation{}, rt.ErrUnsupported
}

func (unimplemented) Start(ctx context.Context, name string) (rt.Receipt, error) {
	return rt.Receipt{}, rt.ErrUnsupported
}

func (unimplemented) Stop(ctx context.Context, name string) (rt.Receipt, error) {
	return rt.Receipt{}, rt.ErrUnsupported
}

func (unimplemented) Pause(ctx context.Context, name, pauseID string) (rt.Receipt, error) {
	return rt.Receipt{}, rt.ErrUnsupported
}

func (unimplemented) Resume(ctx context.Context, name, pauseID string) (rt.Receipt, error) {
	return rt.Receipt{}, rt.ErrUnsupported
}

func (unimplemented) Delete(ctx context.Context, name string) (rt.Receipt, error) {
	return rt.Receipt{}, rt.ErrUnsupported
}

func (unimplemented) Fork(ctx context.Context, parent string, request rt.ForkRequest) (rt.Observation, error) {
	return rt.Observation{}, rt.ErrUnsupported
}

func (unimplemented) CheckRestartNever(ctx context.Context, name, binding, state string) error {
	return rt.ErrUnsupported
}

func (unimplemented) CaptureCheckpoint(ctx context.Context, name string, dst io.Writer) (int64, error) {
	return 0, rt.ErrUnsupported
}

func (unimplemented) ExportPausedCheckpoint(ctx context.Context, name string, dst io.Writer, pauseID ...string) (int64, error) {
	return 0, rt.ErrUnsupported
}

func (unimplemented) RestoreCheckpoint(ctx context.Context, name string, source io.Reader, length int64) (rt.Observation, error) {
	return rt.Observation{}, rt.ErrUnsupported
}

func (unimplemented) SetPorts(ctx context.Context, name string, ports []rt.Port) (rt.Observation, error) {
	return rt.Observation{}, rt.ErrUnsupported
}

func (unimplemented) SetLabels(ctx context.Context, name string, labels map[string]*string) (rt.Observation, error) {
	return rt.Observation{}, rt.ErrUnsupported
}

func (unimplemented) EnablePublicNetwork(ctx context.Context, name string) (rt.Observation, error) {
	return rt.Observation{}, rt.ErrUnsupported
}

func (unimplemented) SetCPULimit(ctx context.Context, name string, millicpus *uint32) (rt.Observation, error) {
	return rt.Observation{}, rt.ErrUnsupported
}

func (unimplemented) Resize(ctx context.Context, name string, request rt.ResizeRequest) (rt.Observation, error) {
	return rt.Observation{}, rt.ErrUnsupported
}

func (unimplemented) Capacity(ctx context.Context) (rt.Capacity, error) {
	return rt.Capacity{}, rt.ErrUnsupported
}

func (unimplemented) Metrics(ctx context.Context, name string) (rt.Metrics, error) {
	return rt.Metrics{}, rt.ErrUnsupported
}

func (unimplemented) Exec(ctx context.Context, name string, request rt.ExecRequest) (rt.ExecResult, error) {
	return rt.ExecResult{}, rt.ErrUnsupported
}

func (unimplemented) StartExec(ctx context.Context, name string, request rt.ExecSessionRequest) (rt.ExecSession, error) {
	return rt.ExecSession{}, rt.ErrUnsupported
}

func (unimplemented) ReadExec(ctx context.Context, name, id string, request rt.ExecRead) (rt.ExecSession, error) {
	return rt.ExecSession{}, rt.ErrUnsupported
}

func (unimplemented) WriteExec(ctx context.Context, name, id string, seq uint64, data []byte, eof bool) (uint64, error) {
	return 0, rt.ErrUnsupported
}

func (unimplemented) ResizeExec(ctx context.Context, name, id string, cols, rows uint16) error {
	return rt.ErrUnsupported
}

func (unimplemented) DeleteExec(ctx context.Context, name, id string) error {
	return rt.ErrUnsupported
}

func (unimplemented) DownloadFile(ctx context.Context, name, guestPath string, dst io.Writer) error {
	return rt.ErrUnsupported
}

func (unimplemented) UploadFileWithOptions(ctx context.Context, name, guestPath string, source io.Reader, options rt.FileOptions) error {
	return rt.ErrUnsupported
}
