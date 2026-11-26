// Package guestfiles transfers VM files over a dedicated, Worker-attested
// System Agent stream. Callers hold the platform access lease until Close.
package guestfiles

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path"
	"slices"
	"strings"
	"time"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/workerclient"
)

const (
	Capability     = "system-file-transfer-v1"
	MaxUpload      = 64 << 20
	MaxDownload    = 4 << 30
	chunkSize      = 256 << 10
	frameLimit     = (chunkSize+2)/3*4 + 8192 // Base64 expansion and JSON.
	listLimit      = 8 << 20
	maxEntries     = 4096
	frameTimeout   = 30 * time.Second
	sessionTimeout = 5 * time.Minute
	authorizeEvery = time.Second
)

type Entry struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Size int64  `json:"size"`
}

type Session struct {
	conn         net.Conn
	ctx          context.Context
	cancel       context.CancelFunc
	stop         func() bool
	reauthorize  func(context.Context) error
	authorizedAt time.Time
}

type vmTarget struct {
	Kind string `json:"kind"`
}

type request struct {
	Method    string          `json:"method"`
	Path      string          `json:"path,omitempty"`
	Target    *vmTarget       `json:"target,omitempty"`
	TotalSize *int64          `json:"total_size,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	Done      *bool           `json:"done,omitempty"`
}

type response struct {
	Status       string          `json:"status"`
	Code         string          `json:"code"`
	Message      string          `json:"message"`
	Version      int             `json:"version"`
	Capabilities []string        `json:"capabilities"`
	Data         json.RawMessage `json:"data"`
	Done         *bool           `json:"done"`
}

// Open binds the session to the exact running execution returned by Access.
// Reauthorize verifies the live credential and that the binding is still
// current. The caller must Close the session even after a failed operation.
func Open(ctx context.Context, adapter rt.Adapter, binding registry.Binding, reauthorize func(context.Context) error) (*Session, error) {
	if reauthorize == nil {
		return nil, rt.ErrInvalid
	}
	client, ok := lifecycle.PolicyClient(adapter, binding)
	execution := lifecycle.WorkerExecution(binding)
	if !ok || binding.PowerState != registry.PowerRunning || execution.IncarnationID == "" || execution.Epoch <= 0 {
		return nil, rt.ErrUnsupported
	}
	conn, err := client.OpenGuestAgent(ctx, execution)
	if err != nil {
		return nil, workerError(err)
	}
	return open(ctx, conn, reauthorize)
}

func open(ctx context.Context, conn net.Conn, reauthorize func(context.Context) error) (*Session, error) {
	ctx, cancel := context.WithTimeout(ctx, sessionTimeout)
	s := &Session{conn: conn, ctx: ctx, cancel: cancel, reauthorize: reauthorize}
	s.stop = context.AfterFunc(ctx, func() { _ = conn.Close() })
	pong, err := s.exchange(request{Method: "ping"}, frameLimit)
	if err == nil && (pong.Status != "pong" || pong.Version != 1 || !slices.Contains(pong.Capabilities, Capability)) {
		err = fmt.Errorf("%w: System Guest Agent does not advertise %s", rt.ErrUnsupported, Capability)
	}
	if err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Session) Close() error {
	s.stop()
	s.cancel()
	return s.conn.Close()
}

func (s *Session) start(method, target string, directory bool) (request, error) {
	if !path.IsAbs(target) || path.Clean(target) != target || len(target) > 4096 ||
		strings.ContainsRune(target, 0) || !directory && target == "/" {
		return request{}, rt.ErrInvalid
	}
	if err := s.authorize(); err != nil {
		return request{}, err
	}
	return request{Method: method, Path: target, Target: &vmTarget{Kind: "vm"}}, nil
}

func (s *Session) authorize() error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if err := s.reauthorize(s.ctx); err != nil {
		return err
	}
	s.authorizedAt = time.Now()
	return nil
}

func (s *Session) recheck() error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if time.Since(s.authorizedAt) >= authorizeEvery {
		return s.authorize()
	}
	return nil
}

func (s *Session) List(target string) ([]Entry, error) {
	req, err := s.start("list_directory", target, true)
	if err != nil {
		return nil, err
	}
	reply, err := s.exchange(req, listLimit)
	if err != nil {
		return nil, err
	}
	if err := reply.expect("ok"); err != nil {
		return nil, err
	}
	if err := s.recheck(); err != nil {
		return nil, err
	}
	var listing struct {
		Entries []struct {
			Name string `json:"name"`
			Kind string `json:"kind"`
			Size int64  `json:"size"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(reply.Data, &listing); err != nil || listing.Entries == nil || len(listing.Entries) > maxEntries {
		return nil, protocolError("invalid directory listing")
	}
	entries := make([]Entry, 0, len(listing.Entries))
	names := make(map[string]struct{}, len(listing.Entries))
	for _, item := range listing.Entries {
		if item.Name == "" || item.Name == "." || item.Name == ".." || strings.ContainsAny(item.Name, "/\x00") || item.Size < 0 {
			return nil, protocolError("invalid directory entry")
		}
		if _, exists := names[item.Name]; exists {
			return nil, protocolError("duplicate directory entry")
		}
		names[item.Name] = struct{}{}
		switch item.Kind {
		case "dir":
			item.Kind = "directory" // Existing platform listing type.
		case "file", "symlink", "other":
		default:
			return nil, protocolError("invalid directory entry type")
		}
		entries = append(entries, Entry{Name: item.Name, Type: item.Kind, Size: item.Size})
	}
	return entries, nil
}

func (s *Session) Download(target string, dst io.Writer) error {
	req, err := s.start("file_read", target, false)
	if err != nil {
		return err
	}
	if _, err := s.write(req); err != nil {
		return err
	}
	var total int64
	for {
		reply, err := s.read(frameLimit)
		if err != nil {
			return err
		}
		if err := reply.expect("data_chunk"); err != nil {
			return err
		}
		if err := s.recheck(); err != nil {
			return err
		}
		var data []byte
		if err := json.Unmarshal(reply.Data, &data); err != nil || data == nil ||
			reply.Done == nil || len(data) > chunkSize || (len(data) == 0 && !*reply.Done) {
			return protocolError("invalid file data chunk")
		}
		if int64(len(data)) > MaxDownload-total {
			return tooLarge("download", "4 GiB")
		}
		if len(data) > 0 {
			n, err := dst.Write(data)
			if err != nil {
				return err
			}
			if n != len(data) {
				return io.ErrShortWrite
			}
			total += int64(n)
		}
		if *reply.Done {
			return nil
		}
	}
}

// Upload stages exactly size bytes. Its separate empty terminal frame is the
// only commit boundary. A lost commit receipt reports an uncertain effect.
func (s *Session) Upload(target string, source io.Reader, size int64) error {
	if size < 0 {
		return rt.ErrInvalid
	}
	if size > MaxUpload {
		return tooLarge("upload", "64 MiB")
	}
	req, err := s.start("file_write_begin", target, false)
	if err != nil {
		return err
	}
	req.TotalSize = &size
	if err := s.ack(req); err != nil {
		return err
	}
	commit := false
	buf := make([]byte, min(size, chunkSize))
	for remaining := size; remaining > 0; remaining -= int64(len(buf)) {
		if err := s.recheck(); err != nil {
			return err
		}
		buf = buf[:min(remaining, chunkSize)]
		if _, err := io.ReadFull(source, buf); err != nil {
			return fmt.Errorf("%w: upload source ended before its intended size: %v", rt.ErrInvalid, err)
		}
		data, err := json.Marshal(buf)
		if err != nil {
			return err
		}
		if err := s.ack(request{Method: "file_write_chunk", Data: data, Done: &commit}); err != nil {
			return err
		}
	}
	var extra [1]byte
	if n, err := io.ReadFull(source, extra[:]); n != 0 || !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: upload source does not match its intended size", rt.ErrInvalid)
	}
	if err := s.authorize(); err != nil {
		return err
	}

	commit = true
	sent, err := s.write(request{Method: "file_write_chunk", Data: json.RawMessage(`""`), Done: &commit})
	if err != nil {
		if sent {
			return uncertain(err)
		}
		return err
	}
	for {
		reply, err := s.read(frameLimit)
		if err != nil {
			return uncertain(err)
		}
		if reply.Status == "progress" {
			continue
		}
		if err := reply.expect("ok"); err != nil {
			// A structured System Agent failure on finalize precedes rename.
			if reply.Status == "error" {
				switch reply.Code {
				case "INVALID_REQUEST", "NOT_FOUND", "FILE_IO_FAILED":
					return err
				}
			}
			return uncertain(err)
		}
		var receipt struct {
			BytesWritten *int64 `json:"bytes_written"`
		}
		if err := json.Unmarshal(reply.Data, &receipt); err != nil || receipt.BytesWritten == nil || *receipt.BytesWritten != size {
			return uncertain(protocolError("invalid upload receipt"))
		}
		return nil
	}
}

func (s *Session) ack(req request) error {
	reply, err := s.exchange(req, frameLimit)
	if err != nil {
		return err
	}
	return reply.expect("ok")
}

func (s *Session) exchange(req request, limit uint32) (response, error) {
	if _, err := s.write(req); err != nil {
		return response{}, err
	}
	return s.read(limit)
}

func (s *Session) deadline() (time.Time, error) {
	if err := s.ctx.Err(); err != nil {
		return time.Time{}, err
	}
	session, _ := s.ctx.Deadline()
	idle := time.Now().Add(frameTimeout)
	if session.Before(idle) {
		return session, nil
	}
	return idle, nil
}

func (s *Session) write(req request) (sent bool, err error) {
	frame, err := json.Marshal(req)
	if err != nil {
		return false, err
	}
	if len(frame) > frameLimit {
		return false, rt.ErrInvalid
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(frame)))
	deadline, err := s.deadline()
	if err != nil {
		return false, err
	}
	if err := s.conn.SetWriteDeadline(deadline); err != nil {
		return false, transportError(err)
	}
	frame = append(header[:], frame...)
	n, err := s.conn.Write(frame)
	if err == nil && n != len(frame) {
		err = io.ErrShortWrite
	}
	return n == len(frame), transportError(err)
}

func (s *Session) read(limit uint32) (response, error) {
	var reply response
	deadline, err := s.deadline()
	if err != nil {
		return reply, err
	}
	if err := s.conn.SetReadDeadline(deadline); err != nil {
		return reply, transportError(err)
	}
	var header [4]byte
	if _, err := io.ReadFull(s.conn, header[:]); err != nil {
		return reply, transportError(err)
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > limit {
		return reply, protocolError("invalid frame size")
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(s.conn, data); err != nil {
		return reply, transportError(err)
	}
	if err := json.Unmarshal(data, &reply); err != nil {
		return reply, protocolError("invalid response frame")
	}
	return reply, nil
}

func (r response) expect(status string) error {
	switch r.Status {
	case status:
		return nil
	case "error":
		switch r.Code {
		case "NOT_FOUND":
			return fmt.Errorf("%w: %s", rt.ErrNotFound, r.Message)
		case "INVALID_REQUEST":
			return fmt.Errorf("%w: %s", rt.ErrInvalid, r.Message)
		case "RESOURCE_LIMIT":
			return &lifecycle.Failure{Code: "RESOURCE_EXHAUSTED", Message: r.Message}
		case "FILE_TOO_LARGE":
			return &lifecycle.Failure{Code: "FILE_TOO_LARGE", Message: r.Message}
		case "FILE_IO_FAILED":
			return &lifecycle.Failure{Code: "GUEST_FILE_IO_FAILED", Message: r.Message}
		}
	}
	return protocolError("unexpected response")
}

func workerError(err error) error {
	var fault *workerclient.Fault
	if errors.As(err, &fault) {
		switch fault.Code {
		case "E_CAPABILITY_UNSUPPORTED", "E_UNSUPPORTED_METHOD", "E_INVALID_REQUEST", "E_NATIVE_UNBOUND":
			return fmt.Errorf("%w: %s", rt.ErrUnsupported, fault.Message)
		case "E_STALE_EXECUTION", "E_STALE_REVISION", "E_STATE_CONFLICT", "E_NOT_FOUND":
			return registry.ErrGeneration
		case "E_PERMISSION_DENIED":
			return auth.ErrForbidden
		}
	}
	return transportError(err)
}

func tooLarge(direction, limit string) error {
	return &lifecycle.Failure{Code: "FILE_TOO_LARGE", Message: direction + " limit is " + limit}
}

func protocolError(message string) error {
	return fmt.Errorf("%w: System Guest Agent file protocol: %s", rt.ErrUnavailable, message)
}

func transportError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: Guest Agent file connection: %w", rt.ErrUnavailable, err)
}

func uncertain(err error) error {
	return fmt.Errorf("%w: Guest Agent did not confirm the upload commit: %v", rt.ErrUncertain, err)
}
