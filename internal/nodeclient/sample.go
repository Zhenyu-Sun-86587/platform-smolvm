package nodeclient

import (
	"context"
	"sandbox-platform/internal/nodeproto"
	rt "sandbox-platform/internal/runtime"
)

func (c *Client) Sample(ctx context.Context, in nodeproto.SampleRequest) (nodeproto.SampleReply, error) {
	return request[nodeproto.SampleRequest, nodeproto.SampleReply](ctx, c, nodeproto.Route{}, nodeproto.SamplePath, in)
}

func (a *Adapter) ResolveLaunchNetwork(ctx context.Context, selection *rt.LaunchNetwork) (*rt.LaunchNetwork, error) {
	return call[*rt.LaunchNetwork, *rt.LaunchNetwork](ctx, a, nodeproto.WorkerPath+"resolve-network", selection)
}
