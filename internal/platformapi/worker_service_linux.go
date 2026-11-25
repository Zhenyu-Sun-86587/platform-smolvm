package platformapi

import (
	"context"
	"net"

	"sandbox-platform/internal/gateway"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/workerclient"
)

func privateServiceWorker(adapter rt.Adapter) string {
	if runtime, ok := adapter.(interface{ WorkerSocket() string }); ok {
		return runtime.WorkerSocket()
	}
	return ""
}

func workerServiceBackend(ctx context.Context, socket string, binding registry.Binding, service service) (gateway.Backend, error) {
	worker := workerclient.Client{Socket: socket, Bundle: binding.RuntimeBundleID}
	execution := workerclient.Execution{
		InstanceID: binding.RuntimeInstanceObjectID, IncarnationID: binding.IncarnationID,
		Epoch: binding.ExecutionEpoch,
	}
	attachment, err := worker.LookupAttachment(ctx, execution)
	if err != nil || attachment.Lease.State != "attached" || attachment.Lease.Gate != "open" {
		return gateway.Backend{}, &lifecycle.Failure{Code: "SERVICE_NOT_READY", Message: "Worker network attachment is not published"}
	}
	return gateway.Backend{
		Port: service.GuestPort, Protocol: service.Protocol, ServerName: service.PublicHost,
		DialPrivate: func(ctx context.Context) (net.Conn, error) {
			return worker.DialAttachment(ctx, attachment, service.GuestPort, "tcp")
		},
	}, nil
}
