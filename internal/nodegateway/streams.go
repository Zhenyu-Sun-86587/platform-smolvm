package nodegateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"sandbox-platform/internal/nodeproto"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/workerclient"
)

const (
	streamOpenTimeout = 10 * time.Second
	streamIdleTimeout = 5 * time.Minute
	streamMaxAge      = time.Hour
)

func workerExecution(ref rt.ExecutionRef) workerclient.Execution {
	return workerclient.Execution{InstanceID: ref.InstanceID, IncarnationID: ref.IncarnationID, Epoch: ref.ExecutionEpoch}
}

func (s *Server) registerStreams(mux *http.ServeMux) {
	stream(s, mux, nodeproto.ServiceStreamPath,
		func(in nodeproto.ServiceStream) rt.ExecutionRef { return in.Expected },
		func(ctx context.Context, route Route, in nodeproto.ServiceStream) (net.Conn, error) {
			if in.Port == 0 {
				return nil, rt.ErrInvalid
			}
			if route.Worker.Socket == "" {
				return nil, rt.ErrUnsupported
			}
			if err := gateAllows(ctx, s.journal.db, false, true); err != nil {
				return nil, err
			}
			a, err := route.Worker.LookupAttachment(ctx, workerExecution(in.Expected))
			if err != nil {
				return nil, err
			}
			if in.Attachment != nil && a != *in.Attachment {
				return nil, rt.ErrConflict
			}
			return route.Worker.DialAttachment(ctx, a, in.Port, "tcp")
		})
	stream(s, mux, nodeproto.GuestAgentStreamPath,
		func(in rt.ExecutionRef) rt.ExecutionRef { return in },
		func(ctx context.Context, route Route, expected rt.ExecutionRef) (net.Conn, error) {
			if route.Worker.Socket == "" {
				return nil, rt.ErrUnsupported
			}
			return route.Worker.OpenGuestAgent(ctx, workerExecution(expected))
		})
}

func stream[In any](s *Server, mux *http.ServeMux, path string, execution func(In) rt.ExecutionRef,
	open func(context.Context, Route, In) (net.Conn, error)) {
	mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			writeFailure(w, nodeproto.CodeInvalid)
			return
		}
		if !takeSlot(s.streams) {
			writeFailure(w, nodeproto.CodeBusy)
			return
		}
		defer func() { <-s.streams }()
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			Subprotocols: []string{nodeproto.StreamProtocol},
		})
		if err != nil {
			return
		}
		defer ws.CloseNow()
		if ws.Subprotocol() != nodeproto.StreamProtocol {
			_ = ws.Close(websocket.StatusPolicyViolation, "stream protocol required")
			return
		}
		ws.SetReadLimit(nodeproto.FrameSize)
		ctx, cancel := context.WithTimeout(r.Context(), streamOpenTimeout)
		defer cancel()
		reply := func(code string) error {
			raw, err := json.Marshal(nodeproto.StreamReady{Code: code})
			if err != nil {
				return err
			}
			return ws.Write(ctx, websocket.MessageText, raw)
		}
		kind, raw, err := ws.Read(ctx)
		var request nodeproto.Envelope[In]
		if err != nil || kind != websocket.MessageText || decodeJSON(bytes.NewReader(raw), &request) != nil {
			_ = reply(nodeproto.CodeInvalid)
			return
		}
		route, err := s.checkScope(request.Protocol, request.Authority, request.Route, true)
		if err != nil {
			_ = reply(nodeproto.FailureCode(err))
			return
		}
		expected := execution(request.Request)
		if !nodeproto.ValidExecution(expected) {
			_ = reply(nodeproto.CodeInvalid)
			return
		}
		key := request.Route.RuntimeInstanceID + "/" + expected.InstanceID
		if !s.takeExecutionStream(key) {
			_ = reply(nodeproto.CodeBusy)
			return
		}
		defer s.releaseExecutionStream(key)
		local, err := open(ctx, route, request.Request)
		if err != nil {
			_ = reply(nodeproto.FailureCode(err))
			return
		}
		defer local.Close()
		half, ok := local.(interface{ CloseWrite() error })
		if !ok {
			_ = reply(nodeproto.CodeUnsupported)
			return
		}
		if err := reply(""); err != nil {
			return
		}
		cancel()
		relay(r.Context(), ws, local, half.CloseWrite)
	})
}

func (s *Server) takeExecutionStream(key string) bool {
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	if s.perVM[key] == nodeproto.StreamPerExecution {
		return false
	}
	s.perVM[key]++
	return true
}

func (s *Server) releaseExecutionStream(key string) {
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	s.perVM[key]--
	if s.perVM[key] == 0 {
		delete(s.perVM, key)
	}
}

func relay(parent context.Context, ws *websocket.Conn, local net.Conn, closeLocalWrite func() error) {
	ctx, cancel := context.WithTimeout(parent, streamMaxAge)
	defer cancel()
	remote := nodeproto.NewFramedConn(ctx, ws, "worker", "controller")
	defer remote.Close()

	progress := make(chan struct{}, 1)
	done := make(chan error, 2)
	go func() {
		_, err := io.CopyBuffer(activityWriter{local, progress}, remote, make([]byte, nodeproto.FrameSize))
		if err == nil {
			err = closeLocalWrite()
			// Once the sender signals FIN, any additional data is a protocol
			// error. Monitor transport closure while the local side finishes.
			peer := ws.CloseRead(ctx)
			go func() {
				<-peer.Done()
				cancel()
			}()
		}
		done <- err
	}()
	go func() {
		_, err := io.CopyBuffer(activityWriter{remote, progress}, local, make([]byte, nodeproto.FrameSize))
		if err == nil {
			err = remote.CloseWrite()
		}
		done <- err
	}()
	timer := time.NewTimer(streamIdleTimeout)
	defer timer.Stop()
	finished := 0
	for finished < 2 {
		select {
		case <-ctx.Done():
			finished = closeRelay(local, remote, done, finished)
		case <-timer.C:
			finished = closeRelay(local, remote, done, finished)
		case <-progress:
			timer.Reset(streamIdleTimeout)
		case err := <-done:
			finished++
			if err != nil && !errors.Is(err, net.ErrClosed) {
				finished = closeRelay(local, remote, done, finished)
			}
		}
	}
}

type activityWriter struct {
	io.Writer
	progress chan<- struct{}
}

func (w activityWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if n > 0 {
		select {
		case w.progress <- struct{}{}:
		default:
		}
	}
	return n, err
}

func closeRelay(local, remote net.Conn, done <-chan error, finished int) int {
	_ = remote.Close()
	_ = local.Close()
	for ; finished < cap(done); finished++ {
		<-done
	}
	return finished
}
