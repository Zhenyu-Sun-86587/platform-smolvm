package runtime

import (
	"context"
	"io"
)

// NativeGuestControl addresses the verified execution beneath a machine binding.
type NativeGuestControl interface {
	GuestPing(context.Context, ExecutionRef) error
	GuestExec(context.Context, ExecutionRef, ExecRequest) (ExecResult, error)
	GuestUpload(context.Context, ExecutionRef, string, io.Reader, FileOptions) error
}
