package runtime

import (
	"context"
	"io"
)

// UnsupportedOperations keeps legacy mutators closed until their native handlers and
// Worker admission are available.
type UnsupportedOperations struct{}

func (UnsupportedOperations) Create(ctx context.Context, request CreateRequest) (Observation, error) {
	return Observation{}, ErrUnsupported
}

func (UnsupportedOperations) Start(ctx context.Context, name string) (Receipt, error) {
	return Receipt{}, ErrUnsupported
}

func (UnsupportedOperations) Stop(ctx context.Context, name string) (Receipt, error) {
	return Receipt{}, ErrUnsupported
}

func (UnsupportedOperations) Pause(ctx context.Context, name, pauseID string) (Receipt, error) {
	return Receipt{}, ErrUnsupported
}

func (UnsupportedOperations) Resume(ctx context.Context, name, pauseID string) (Receipt, error) {
	return Receipt{}, ErrUnsupported
}

func (UnsupportedOperations) Delete(ctx context.Context, name string) (Receipt, error) {
	return Receipt{}, ErrUnsupported
}

func (UnsupportedOperations) Fork(ctx context.Context, parent string, request ForkRequest) (Observation, error) {
	return Observation{}, ErrUnsupported
}

func (UnsupportedOperations) CheckRestartNever(ctx context.Context, name, binding, state string) error {
	return ErrUnsupported
}

func (UnsupportedOperations) CaptureCheckpoint(ctx context.Context, name string, dst io.Writer) (int64, error) {
	return 0, ErrUnsupported
}

func (UnsupportedOperations) ExportPausedCheckpoint(ctx context.Context, name string, dst io.Writer, pauseID ...string) (int64, error) {
	return 0, ErrUnsupported
}

func (UnsupportedOperations) RestoreCheckpoint(ctx context.Context, name string, source io.Reader, length int64) (Observation, error) {
	return Observation{}, ErrUnsupported
}

func (UnsupportedOperations) SetPorts(ctx context.Context, name string, ports []Port) (Observation, error) {
	return Observation{}, ErrUnsupported
}

func (UnsupportedOperations) SetLabels(ctx context.Context, name string, labels map[string]*string) (Observation, error) {
	return Observation{}, ErrUnsupported
}

func (UnsupportedOperations) EnablePublicNetwork(ctx context.Context, name string) (Observation, error) {
	return Observation{}, ErrUnsupported
}

func (UnsupportedOperations) SetCPULimit(ctx context.Context, name string, millicpus *uint32) (Observation, error) {
	return Observation{}, ErrUnsupported
}

func (UnsupportedOperations) Resize(ctx context.Context, name string, request ResizeRequest) (Observation, error) {
	return Observation{}, ErrUnsupported
}

func (UnsupportedOperations) Capacity(ctx context.Context) (Capacity, error) {
	return Capacity{}, ErrUnsupported
}

func (UnsupportedOperations) Metrics(ctx context.Context, name string) (Metrics, error) {
	return Metrics{}, ErrUnsupported
}

func (UnsupportedOperations) Exec(ctx context.Context, name string, request ExecRequest) (ExecResult, error) {
	return ExecResult{}, ErrUnsupported
}

func (UnsupportedOperations) StartExec(ctx context.Context, name string, request ExecSessionRequest) (ExecSession, error) {
	return ExecSession{}, ErrUnsupported
}

func (UnsupportedOperations) ReadExec(ctx context.Context, name, id string, request ExecRead) (ExecSession, error) {
	return ExecSession{}, ErrUnsupported
}

func (UnsupportedOperations) WriteExec(ctx context.Context, name, id string, seq uint64, data []byte, eof bool) (uint64, error) {
	return 0, ErrUnsupported
}

func (UnsupportedOperations) ResizeExec(ctx context.Context, name, id string, cols, rows uint16) error {
	return ErrUnsupported
}

func (UnsupportedOperations) DeleteExec(ctx context.Context, name, id string) error {
	return ErrUnsupported
}

func (UnsupportedOperations) DownloadFile(ctx context.Context, name, guestPath string, dst io.Writer) error {
	return ErrUnsupported
}

func (UnsupportedOperations) UploadFileWithOptions(ctx context.Context, name, guestPath string, source io.Reader, options FileOptions) error {
	return ErrUnsupported
}
