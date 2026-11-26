package nodeclient

import (
	"context"
	"encoding/json"
	"net"
	"time"

	"github.com/coder/websocket"

	"sandbox-platform/internal/nodeproto"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/workerclient"
)

func (a *Adapter) DialService(ctx context.Context, expected rt.ExecutionRef, port uint16) (net.Conn, error) {
	return openStream(ctx, a, nodeproto.ServiceStreamPath, nodeproto.ServiceStream{Expected: expected, Port: port})
}

func (a *Adapter) DialAttachment(ctx context.Context, attachment workerclient.NetworkAttachment, port uint16, transport string) (net.Conn, error) {
	if transport != "tcp" {
		return nil, rt.ErrUnsupported
	}
	return openStream(ctx, a, nodeproto.ServiceStreamPath, nodeproto.ServiceStream{
		Expected: runtimeExecution(attachment.Lease.Execution), Port: port, Attachment: &attachment,
	})
}

func (a *Adapter) OpenScopedGuest(ctx context.Context, expected workerclient.Execution) (net.Conn, error) {
	return openStream(ctx, a, nodeproto.GuestAgentStreamPath, runtimeExecution(expected))
}

func runtimeExecution(in workerclient.Execution) rt.ExecutionRef {
	return rt.ExecutionRef{InstanceID: in.InstanceID, IncarnationID: in.IncarnationID, ExecutionEpoch: in.Epoch}
}

func openStream[In any](ctx context.Context, a *Adapter, path string, in In) (_ *nodeproto.FramedConn, err error) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	if err := a.client.authorizeRequest(ctx); err != nil {
		return nil, err
	}
	envelope := nodeproto.Envelope[In]{Protocol: nodeproto.Version, Authority: a.client.authority, Route: a.scope, Request: in}
	raw, err := json.Marshal(envelope)
	if err != nil || len(raw) > nodeproto.FrameSize {
		return nil, &nodeproto.Failure{Code: nodeproto.CodeInvalid}
	}
	ws, res, err := websocket.Dial(ctx, a.client.endpoint+path, &websocket.DialOptions{
		HTTPClient: a.client.stream, Subprotocols: []string{nodeproto.StreamProtocol},
	})
	if err != nil {
		if res != nil && res.StatusCode == 429 {
			return nil, &nodeproto.Failure{Code: nodeproto.CodeBusy}
		}
		return nil, &nodeproto.Failure{Code: nodeproto.CodeUnavailable}
	}
	defer func() {
		if err != nil {
			_ = ws.CloseNow()
		}
	}()
	if ws.Subprotocol() != nodeproto.StreamProtocol {
		return nil, &nodeproto.Failure{Code: nodeproto.CodeUnsupported}
	}
	if err := ws.Write(ctx, websocket.MessageText, raw); err != nil {
		return nil, &nodeproto.Failure{Code: nodeproto.CodeUnavailable}
	}
	ws.SetReadLimit(nodeproto.FrameSize)
	kind, raw, readErr := ws.Read(ctx)
	var reply nodeproto.StreamReady
	if readErr != nil || kind != websocket.MessageText || json.Unmarshal(raw, &reply) != nil {
		return nil, &nodeproto.Failure{Code: nodeproto.CodeUnavailable}
	}
	if reply.Code != "" {
		return nil, &nodeproto.Failure{Code: reply.Code}
	}
	return nodeproto.NewFramedConn(context.Background(), ws, a.client.authority.ControllerID, a.client.authority.WorkerID), nil
}
