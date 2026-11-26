package execbridge

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	rt "sandbox-platform/internal/runtime"
)

const guestFrameLimit = 1 << 20
const guestExecCapability = "system-exec-stream-v1"

var ErrGuestStart = errors.New("Guest Agent could not start the command")
var ErrGuestCapacity = errors.New("too many active System Agent exec sessions")
var ErrStdinSequence = errors.New("stdin must follow the last acknowledged sequence")

type guestTerminal struct {
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

type guestCommand struct {
	ID       string         `json:"id"`
	Command  []string       `json:"command"`
	Env      [][2]string    `json:"env"`
	Workdir  string         `json:"workdir"`
	Timeout  uint64         `json:"timeout_ms,omitempty"`
	Terminal *guestTerminal `json:"terminal,omitempty"`
}

type guestResponse struct {
	Status       string   `json:"status"`
	Data         []byte   `json:"data"`
	Message      string   `json:"message"`
	Code         string   `json:"code"`
	ExitCode     *int     `json:"exit_code"`
	Version      int      `json:"version"`
	Capabilities []string `json:"capabilities"`
}

// GuestSession owns a dedicated System Agent stream for the lifetime of the
// command. Browser and API clients can come and go while it stays connected.
type GuestSession struct {
	socket      net.Conn
	output      *Stream
	interactive bool
	tty         bool
	input       sync.Mutex
	inputSeq    uint64
	inputEOF    bool
}

func StartGuest(ctx context.Context, socket net.Conn, output *Stream, req rt.ExecSessionRequest) (*GuestSession, error) {
	ready := false
	defer func() {
		if !ready {
			socket.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	if err := socket.SetDeadline(deadline); err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = socket.Close() })
	defer stop()
	if err := writeGuestFrame(socket, struct {
		Method string `json:"method"`
	}{"ping"}); err != nil {
		return nil, err
	}
	pong, err := readGuestFrame(socket)
	if err != nil {
		return nil, err
	}
	if pong.Status != "pong" || pong.Version != 1 || !slices.Contains(pong.Capabilities, guestExecCapability) {
		return nil, fmt.Errorf("%w: the System Guest Agent does not advertise %s", rt.ErrUnsupported, guestExecCapability)
	}
	command := guestCommand{
		ID: strings.TrimPrefix(req.ID, "exec_"), Command: req.Command,
		Workdir: req.Cwd, Timeout: req.TimeoutSeconds * 1000,
		Env: make([][2]string, 0, len(req.Env)+1),
	}
	for k, v := range req.Env {
		command.Env = append(command.Env, [2]string{k, v})
	}
	if req.TTY {
		command.Terminal = &guestTerminal{Cols: req.Cols, Rows: req.Rows}
		if _, exists := req.Env["TERM"]; !exists {
			command.Env = append(command.Env, [2]string{"TERM", "xterm-256color"})
		}
	}
	sort.Slice(command.Env, func(i, j int) bool { return command.Env[i][0] < command.Env[j][0] })
	if err := writeGuestFrame(socket, struct {
		Method  string       `json:"method"`
		Session guestCommand `json:"session"`
	}{"vm_exec_controlled", command}); err != nil {
		return nil, err
	}
	response, err := readGuestFrame(socket)
	if err != nil {
		return nil, err
	}
	if response.Status == "error" {
		if response.Code == "RESOURCE_LIMIT" {
			return nil, ErrGuestCapacity
		}
		return nil, fmt.Errorf("%w: %s", ErrGuestStart, response.Message)
	}
	if response.Status != "started" {
		return nil, fmt.Errorf("%w: Guest Agent did not acknowledge the command", rt.ErrUnsupported)
	}
	interactive := req.Interactive || req.TTY
	if !interactive {
		// The Agent streams output in either mode. For a non-interactive
		// command the stdin pipe should see EOF without a client attached.
		if err := guestStdin(socket, []byte{}); err != nil && !errors.Is(err, syscall.EPIPE) {
			return nil, err
		}
	}
	stop()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := socket.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	output.Observe(rt.ExecSession{State: "running"})
	ready = true
	return &GuestSession{socket: socket, output: output, interactive: interactive, tty: req.TTY}, nil
}

func (s *GuestSession) Run() {
	defer s.socket.Close()
	var offset uint64
	var reason *string
	for {
		response, err := readGuestFrame(s.socket)
		if err != nil {
			s.output.Lost("Guest exec connection ended before an exit status was received")
			return
		}
		switch response.Status {
		case "stdout", "stderr":
			s.output.Observe(rt.ExecSession{State: "running", Error: reason,
				Output: []rt.OutputChunk{{Stream: response.Status, Offset: offset, Data: response.Data}}})
			offset += uint64(len(response.Data))
		case "error":
			// The Agent sends a timeout explanation before the final exit code.
			message := response.Message
			reason = &message
		case "exited":
			if response.ExitCode == nil {
				s.output.Lost("Guest Agent exited without a command status")
				return
			}
			s.output.Observe(rt.ExecSession{State: "exited", Done: true, ExitCode: response.ExitCode,
				Error: reason, Next: offset, End: offset})
			return
		default:
			s.output.Lost("Guest Agent returned an unexpected exec response")
			return
		}
	}
}

func (s *GuestSession) Input(ctx context.Context, seq uint64, data []byte, eof bool) (uint64, error) {
	if !s.interactive || seq == 0 || len(data) > 64<<10 || len(data) == 0 && !eof {
		return 0, rt.ErrInvalid
	}
	s.input.Lock()
	defer s.input.Unlock()
	if current, _ := s.output.Read(0, 0); current.Done {
		return 0, rt.ErrNotFound
	}
	if seq <= s.inputSeq {
		return s.inputSeq, nil
	}
	if s.inputEOF || seq != s.inputSeq+1 {
		return s.inputSeq, ErrStdinSequence
	}
	err := s.writeInput(ctx, func() error {
		if len(data) > 0 {
			if err := guestStdin(s.socket, data); err != nil {
				return err
			}
		}
		if eof {
			return guestStdin(s.socket, []byte{})
		}
		return nil
	})
	if err != nil {
		return s.inputSeq, err
	}
	s.inputSeq, s.inputEOF = seq, eof
	s.output.Applied(seq)
	return seq, nil
}

func (s *GuestSession) Resize(ctx context.Context, cols, rows uint16) error {
	if !s.tty || cols == 0 || rows == 0 {
		return rt.ErrInvalid
	}
	s.input.Lock()
	defer s.input.Unlock()
	if current, _ := s.output.Read(0, 0); current.Done {
		return rt.ErrNotFound
	}
	return s.writeInput(ctx, func() error {
		return writeGuestFrame(s.socket, struct {
			Method string `json:"method"`
			guestTerminal
		}{"resize", guestTerminal{Cols: cols, Rows: rows}})
	})
}

func (s *GuestSession) Close(message string) {
	s.output.Lost(message)
	_ = s.socket.Close()
}

func (s *GuestSession) writeInput(ctx context.Context, write func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	if requestDeadline, ok := ctx.Deadline(); ok && requestDeadline.Before(deadline) {
		deadline = requestDeadline
	}
	if err := s.socket.SetWriteDeadline(deadline); err != nil {
		return rt.ErrNotFound
	}
	if err := write(); err != nil {
		// A partial frame has no retry boundary. Close the owned Agent
		// stream so the input cannot be applied again after reconnecting.
		s.Close("Guest exec connection failed while sending terminal input")
		return rt.ErrNotFound
	}
	return nil
}

func guestStdin(socket net.Conn, data []byte) error {
	return writeGuestFrame(socket, struct {
		Method string `json:"method"`
		Data   []byte `json:"data"`
	}{"stdin", data})
}

func writeGuestFrame(socket net.Conn, message any) error {
	body, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if len(body) > guestFrameLimit {
		return rt.ErrInvalid
	}
	frame := make([]byte, 4, len(body)+4)
	binary.BigEndian.PutUint32(frame, uint32(len(body)))
	frame = append(frame, body...)
	n, err := socket.Write(frame)
	if err == nil && n != len(frame) {
		return io.ErrShortWrite
	}
	return err
}

func readGuestFrame(socket net.Conn) (guestResponse, error) {
	var header [4]byte
	if _, err := io.ReadFull(socket, header[:]); err != nil {
		return guestResponse{}, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > guestFrameLimit {
		return guestResponse{}, errors.New("invalid Guest Agent frame size")
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(socket, body); err != nil {
		return guestResponse{}, err
	}
	var reply guestResponse
	err := json.Unmarshal(body, &reply)
	return reply, err
}
