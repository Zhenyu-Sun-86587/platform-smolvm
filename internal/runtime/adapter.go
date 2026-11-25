package runtime

import (
	"context"
	"io"
)

// Adapter is the VM and guest interface used by the existing product lifecycle.
// Pause and Resume retain the saved-execution semantics of the original API.
type Adapter interface {
	Health(ctx context.Context) (Health, error)
	Ready(ctx context.Context) error

	Observe(ctx context.Context, name string) (Observation, error)
	Create(ctx context.Context, request CreateRequest) (Observation, error)
	Start(ctx context.Context, name string) (Receipt, error)
	Stop(ctx context.Context, name string) (Receipt, error)
	Pause(ctx context.Context, name, pauseID string) (Receipt, error)
	Resume(ctx context.Context, name, pauseID string) (Receipt, error)
	Delete(ctx context.Context, name string) (Receipt, error)
	Fork(ctx context.Context, parent string, request ForkRequest) (Observation, error)
	CheckRestartNever(ctx context.Context, name, binding, state string) error

	CaptureCheckpoint(ctx context.Context, name string, dst io.Writer) (int64, error)
	ExportPausedCheckpoint(ctx context.Context, name string, dst io.Writer, pauseID ...string) (int64, error)
	RestoreCheckpoint(ctx context.Context, name string, source io.Reader, length int64) (Observation, error)

	SetPorts(ctx context.Context, name string, ports []Port) (Observation, error)
	SetLabels(ctx context.Context, name string, labels map[string]*string) (Observation, error)
	EnablePublicNetwork(ctx context.Context, name string) (Observation, error)
	SetCPULimit(ctx context.Context, name string, millicpus *uint32) (Observation, error)
	Resize(ctx context.Context, name string, request ResizeRequest) (Observation, error)
	Capacity(ctx context.Context) (Capacity, error)
	Metrics(ctx context.Context, name string) (Metrics, error)

	Exec(ctx context.Context, name string, request ExecRequest) (ExecResult, error)
	StartExec(ctx context.Context, name string, request ExecSessionRequest) (ExecSession, error)
	ReadExec(ctx context.Context, name, id string, request ExecRead) (ExecSession, error)
	WriteExec(ctx context.Context, name, id string, seq uint64, data []byte, eof bool) (uint64, error)
	ResizeExec(ctx context.Context, name, id string, cols, rows uint16) error
	DeleteExec(ctx context.Context, name, id string) error
	DownloadFile(ctx context.Context, name, guestPath string, dst io.Writer) error
	UploadFileWithOptions(ctx context.Context, name, guestPath string, source io.Reader, options FileOptions) error
}
