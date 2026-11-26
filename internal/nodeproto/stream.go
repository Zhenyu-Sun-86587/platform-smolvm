package nodeproto

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const StreamProtocol = "nodegateway-stream.v1"

type StreamReady struct {
	Code string `json:"code,omitempty"`
}

const (
	frameData = byte(1)
	frameFIN  = byte(2)
)

// FramedConn carries one ordered byte stream over binary websocket messages.
// A FIN closes just the sender's write side. Transport loss is a stream error.
type FramedConn struct {
	ws     *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc
	local  net.Addr
	remote net.Addr
	rmu    sync.Mutex
	wmu    sync.Mutex
	read   opDeadline
	write  opDeadline
	eof    bool
	fin    bool
	buffer []byte
}

func NewFramedConn(ctx context.Context, ws *websocket.Conn, local, remote string) *FramedConn {
	ctx, cancel := context.WithCancel(ctx)
	ws.SetReadLimit(FrameSize + 1)
	return &FramedConn{ws: ws, ctx: ctx, cancel: cancel, local: streamAddr(local), remote: streamAddr(remote)}
}

func (c *FramedConn) Read(p []byte) (int, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	if c.eof {
		return 0, io.EOF
	}
	if len(c.buffer) == 0 {
		ctx, done := c.read.begin(c.ctx)
		kind, frame, err := c.ws.Read(ctx)
		cause := context.Cause(ctx)
		done()
		if err != nil {
			if errors.Is(cause, os.ErrDeadlineExceeded) {
				return 0, cause
			}
			return 0, fmt.Errorf("Node stream interrupted before FIN: %w", err)
		}
		if kind != websocket.MessageBinary || len(frame) == 0 {
			_ = c.Close()
			return 0, errors.New("invalid Node stream frame")
		}
		switch {
		case frame[0] == frameFIN && len(frame) == 1:
			c.eof = true
			return 0, io.EOF
		case frame[0] == frameData && len(frame) > 1:
			c.buffer = frame[1:]
		default:
			_ = c.Close()
			return 0, errors.New("invalid Node stream data or FIN")
		}
	}
	n := copy(p, c.buffer)
	c.buffer = c.buffer[n:]
	return n, nil
}

func (c *FramedConn) Write(p []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.fin {
		return 0, io.ErrClosedPipe
	}
	if len(p) == 0 {
		return 0, nil
	}
	frame := make([]byte, min(len(p), FrameSize)+1)
	frame[0] = frameData
	written := 0
	for len(p) != 0 {
		n := copy(frame[1:], p)
		if err := c.writeFrame(frame[:n+1]); err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

func (c *FramedConn) CloseWrite() error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.fin {
		return nil
	}
	c.fin = true
	return c.writeFrame([]byte{frameFIN})
}

func (c *FramedConn) writeFrame(frame []byte) error {
	ctx, done := c.write.begin(c.ctx)
	defer done()
	err := c.ws.Write(ctx, websocket.MessageBinary, frame)
	if errors.Is(context.Cause(ctx), os.ErrDeadlineExceeded) {
		return os.ErrDeadlineExceeded
	}
	return err
}

func (c *FramedConn) Close() error {
	c.cancel()
	return c.ws.CloseNow()
}

func (c *FramedConn) LocalAddr() net.Addr  { return c.local }
func (c *FramedConn) RemoteAddr() net.Addr { return c.remote }
func (c *FramedConn) SetDeadline(t time.Time) error {
	c.read.set(t)
	c.write.set(t)
	return nil
}
func (c *FramedConn) SetReadDeadline(t time.Time) error {
	c.read.set(t)
	return nil
}
func (c *FramedConn) SetWriteDeadline(t time.Time) error {
	c.write.set(t)
	return nil
}

type streamAddr string

func (a streamAddr) Network() string { return "nodegateway" }
func (a streamAddr) String() string  { return string(a) }

type opDeadline struct {
	mu     sync.Mutex
	at     time.Time
	timer  *time.Timer
	cancel context.CancelCauseFunc
}

func (d *opDeadline) begin(parent context.Context) (context.Context, func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	ctx, cancel := context.WithCancelCause(parent)
	d.cancel = cancel
	d.schedule()
	return ctx, func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.cancel = nil
		d.schedule()
		cancel(nil)
	}
}

func (d *opDeadline) set(t time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.at = t
	d.schedule()
}

func (d *opDeadline) schedule() {
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
	if d.cancel == nil || d.at.IsZero() {
		return
	}
	delay := time.Until(d.at)
	if delay <= 0 {
		d.cancel(os.ErrDeadlineExceeded)
		return
	}
	var timer *time.Timer
	timer = time.AfterFunc(delay, func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.timer == timer && d.cancel != nil {
			d.cancel(os.ErrDeadlineExceeded)
		}
	})
	d.timer = timer
}
