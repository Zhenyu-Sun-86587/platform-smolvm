// Package workerclient connects the platform to the protected local Worker.
package workerclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	rt "sandbox-platform/internal/runtime"
)

const protocol = "WorkerResource/1"
const callTimeout = 5 * time.Second

type Client struct {
	Socket string
	Bundle string
}

type Execution struct {
	InstanceID    string `json:"instance_id"`
	IncarnationID string `json:"incarnation_id,omitempty"`
	Epoch         int64  `json:"execution_epoch"`
}

type descriptor struct {
	Index      int    `json:"index"`
	ResourceID string `json:"resource_id"`
	Role       string `json:"role"`
}

type request struct {
	Protocol      string       `json:"protocol"`
	Method        string       `json:"method"`
	RequestID     string       `json:"request_id"`
	Route         string       `json:"route_id"`
	Bundle        string       `json:"bundle_digest"`
	OperationID   string       `json:"operation_id,omitempty"`
	RequestDigest string       `json:"request_digest,omitempty"`
	Expected      *Execution   `json:"expected,omitempty"`
	Payload       any          `json:"payload,omitempty"`
	Descriptors   []descriptor `json:"fd_table,omitempty"`
}

type Fault struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (f *Fault) Error() string { return f.Code + ": " + f.Message }

func (c Client) call(ctx context.Context, q request, pass *os.File, expect *descriptor) (json.RawMessage, *os.File, error) {
	backup := q.Method == "PrepareBackupClosure" || q.Method == "ReadBackupMember" || q.Method == "ReleaseBackupClosure"
	statePointRelease := q.Method == "ReleaseStatePoint"
	privateWake := q.Method == "QueryPrivatePacketWakeEndpoint" || q.Method == "QueryPrivatePacketWakeRule" || q.Method == "ApplyPrivatePacketWakeRule"
	readOnly := q.Method == "Health" || q.Method == "QueryWorkerBudget" || q.Method == "QueryResource" || q.Method == "QueryOperation" || q.Method == "QueryInstanceBinding"
	if !filepath.IsAbs(c.Socket) || c.Bundle == "" || (!backup && !statePointRelease && !readOnly && q.Expected == nil) ||
		(q.Expected != nil && (q.Expected.InstanceID == "" || q.Expected.Epoch < 0 ||
			(!lifecycleMethod(q.Method) && (q.Expected.Epoch < 1 || q.Expected.IncarnationID == "" && !privateWake)))) {
		return nil, nil, errors.New("invalid local Worker execution scope")
	}
	timeout := callTimeout
	if q.Method == "PrepareBackupClosure" {
		timeout = 120 * time.Second
	}
	if q.Method == "QueryNativeResources" || q.Method == "SubmitNativeResize" {
		// The Worker also samples the current Guest Agent and VMM for these calls.
		timeout = 20 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	peer, err := (&net.Dialer{}).DialContext(ctx, "unixpacket", c.Socket)
	if err != nil {
		return nil, nil, err
	}
	defer peer.Close()
	conn := peer.(*net.UnixConn)
	deadline, _ := ctx.Deadline()
	if err = conn.SetDeadline(deadline); err != nil {
		return nil, nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()
	raw, err := conn.SyscallConn()
	if err != nil {
		return nil, nil, err
	}
	var uid uint32
	var peerErr error
	if err := raw.Control(func(fd uintptr) {
		var credentials *unix.Ucred
		credentials, peerErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if peerErr == nil {
			uid = credentials.Uid
		}
	}); err != nil {
		return nil, nil, err
	}
	if peerErr != nil || uid != 0 {
		return nil, nil, errors.New("Worker connection requires the privileged local peer")
	}
	q.Protocol, q.RequestID, q.Route, q.Bundle = protocol, q.Method, "deep", c.Bundle
	var rights []byte
	if pass != nil {
		rights = unix.UnixRights(int(pass.Fd()))
	}
	wire, err := json.Marshal(q)
	if err != nil {
		return nil, nil, err
	}
	n, sent, err := conn.WriteMsgUnix(wire, rights, nil)
	if err != nil || n != len(wire) || sent != len(rights) {
		writeErr := errors.Join(errors.New("cannot send complete Worker request"), err)
		if n > 0 || sent > 0 {
			writeErr = workerReplyError(q.Method, writeErr)
		}
		return nil, nil, writeErr
	}
	body, file, err := readReply(raw, q.RequestID, expect)
	var transport net.Error
	if errors.As(err, &transport) {
		err = fmt.Errorf("read Worker %s response: %w", q.Method, err)
	}
	return body, file, workerReplyError(q.Method, err)
}

func workerReplyError(method string, err error) error {
	var fault *Fault
	if err == nil || errors.As(err, &fault) {
		return err
	}
	mutates := false
	switch method {
	case "SubmitNativeResize", "SetManagedCPULimit", "ApplyPrivatePacketWakeRule", "ReleaseStatePoint",
		"PrepareBackupClosure", "ReleaseBackupClosure":
		mutates = true
	default:
		mutates = lifecycleMethod(method) && !LifecycleReadOnly(method)
	}
	if mutates {
		return fmt.Errorf("%w: %w", rt.ErrUncertain, err)
	}
	return err
}

func readReply(raw syscall.RawConn, requestID string, expect *descriptor) (json.RawMessage, *os.File, error) {
	body := make([]byte, 64*1024)
	oob := make([]byte, unix.CmsgSpace(4))
	var n, oobn, flags int
	var receiveErr error
	err := raw.Read(func(fd uintptr) bool {
		for {
			n, oobn, flags, _, receiveErr = unix.Recvmsg(int(fd), body, oob, unix.MSG_CMSG_CLOEXEC)
			if receiveErr != unix.EINTR {
				return receiveErr != unix.EAGAIN
			}
		}
	})
	if err != nil || receiveErr != nil {
		return nil, nil, errors.Join(err, receiveErr)
	}
	var descriptors []int
	defer func() {
		for _, fd := range descriptors {
			_ = unix.Close(fd)
		}
	}()
	control, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return nil, nil, err
	}
	for _, message := range control {
		fds, err := unix.ParseUnixRights(&message)
		if err != nil {
			return nil, nil, err
		}
		descriptors = append(descriptors, fds...)
	}
	if n == 0 {
		return nil, nil, io.EOF
	}
	if flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 {
		return nil, nil, errors.New("incomplete Worker reply")
	}
	var reply struct {
		Protocol    string          `json:"protocol"`
		RequestID   string          `json:"request_id"`
		Result      json.RawMessage `json:"result"`
		Error       *Fault          `json:"error"`
		Descriptors []descriptor    `json:"fd_table"`
	}
	if err := json.Unmarshal(body[:n], &reply); err != nil {
		return nil, nil, err
	}
	if reply.Protocol != protocol || reply.RequestID != requestID {
		return nil, nil, errors.New("Worker reply does not belong to this request")
	}
	if reply.Error != nil {
		return nil, nil, reply.Error
	}
	if len(reply.Result) == 0 {
		return nil, nil, errors.New("missing Worker result")
	}
	if expect == nil && len(descriptors) == 0 && len(reply.Descriptors) == 0 {
		return reply.Result, nil, nil
	}
	if expect == nil || len(descriptors) != 1 || len(reply.Descriptors) != 1 || reply.Descriptors[0] != *expect {
		return nil, nil, errors.New("unexpected Worker descriptor table")
	}
	file := os.NewFile(uintptr(descriptors[0]), expect.Role)
	descriptors = nil
	return reply.Result, file, nil
}

// VerifyGuestTransport hands the Worker the accepted host socket itself. The
// Worker reads its kernel peer identity and compares it with the exact VMM.
func (c Client) VerifyGuestTransport(ctx context.Context, execution Execution, conn *net.UnixConn) error {
	if conn == nil {
		return errors.New("missing accepted Guest relay connection")
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var fd int
	var dupErr error
	if err := raw.Control(func(peer uintptr) {
		fd, dupErr = unix.FcntlInt(peer, unix.F_DUPFD_CLOEXEC, 0)
	}); err != nil {
		return err
	}
	if dupErr != nil {
		return dupErr
	}
	file := os.NewFile(uintptr(fd), "guest-transport")
	defer file.Close()
	result, _, err := c.call(ctx, request{Method: "VerifyGuestTransport", Expected: &execution,
		Descriptors: []descriptor{{ResourceID: execution.IncarnationID, Role: "guest_transport"}}}, file, nil)
	if err != nil {
		return err
	}
	var observed struct {
		Execution
		Bundle string `json:"bundle_digest"`
		Phase  string `json:"phase"`
	}
	if err := json.Unmarshal(result, &observed); err != nil {
		return err
	}
	if observed.Execution != execution || observed.Bundle != c.Bundle || observed.Phase != "registered" {
		return fmt.Errorf("Worker did not attest the requested running execution")
	}
	return nil
}

// OpenGuestAgent returns the connected stream opened by the Worker after it
// verifies the running VMM. No socket path is supplied by the caller.
func (c Client) OpenGuestAgent(ctx context.Context, execution Execution) (*net.UnixConn, error) {
	result, file, err := c.call(ctx, request{Method: "OpenGuestAgent", Expected: &execution, Payload: struct{}{}},
		nil, &descriptor{ResourceID: execution.IncarnationID, Role: "guest_agent"})
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var observed struct {
		Execution
		Bundle    string `json:"bundle_digest"`
		Route     string `json:"route_id"`
		Phase     string `json:"phase"`
		PID       int    `json:"pid"`
		StartTime int64  `json:"pid_start_time"`
		BootID    string `json:"host_boot_id"`
	}
	if err = json.Unmarshal(result, &observed); err != nil {
		return nil, err
	}
	if observed.Execution != execution || observed.Bundle != c.Bundle || observed.Route != "deep" ||
		observed.Phase != "registered" || observed.PID <= 0 || observed.StartTime <= 0 || observed.BootID == "" {
		return nil, errors.New("Worker did not attest the requested Guest Agent execution")
	}
	conn, err := net.FileConn(file)
	if err != nil {
		return nil, err
	}
	if stream, ok := conn.(*net.UnixConn); ok && stream.RemoteAddr().Network() == "unix" {
		return stream, nil
	}
	conn.Close()
	return nil, errors.New("Worker did not return a connected Guest Agent stream")
}
