package platformapi

import (
	"context"
	"database/sql"
	"encoding/json"
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

func workerServiceAttachment(ctx context.Context, socket string, binding registry.Binding) (workerclient.Client, workerclient.NetworkAttachment, error) {
	worker := workerclient.Client{Socket: socket, Bundle: binding.RuntimeBundleID}
	execution := workerclient.Execution{
		InstanceID: binding.RuntimeInstanceObjectID, IncarnationID: binding.IncarnationID,
		Epoch: binding.ExecutionEpoch,
	}
	attachment, err := worker.LookupAttachment(ctx, execution)
	if err != nil || attachment.Lease.State != "attached" || attachment.Lease.Gate != "open" {
		return worker, attachment, &lifecycle.Failure{Code: "SERVICE_NOT_READY", Message: "Worker network attachment is not published"}
	}
	return worker, attachment, nil
}

func workerServiceBackend(ctx context.Context, socket string, binding registry.Binding, service service) (gateway.Backend, error) {
	worker, attachment, err := workerServiceAttachment(ctx, socket, binding)
	if err != nil {
		return gateway.Backend{}, err
	}
	return gateway.Backend{
		Port: service.GuestPort, Protocol: service.Protocol, ServerName: service.PublicHost,
		DialPrivate: func(ctx context.Context) (net.Conn, error) {
			return worker.DialAttachment(ctx, attachment, service.GuestPort, "tcp")
		},
	}, nil
}

func (s *Server) editWorkerService(ctx context.Context, j *lifecycle.Job, worker string) error {
	if j.Op.Kind == "service_create" {
		svc, err := s.requestedService(j)
		if err != nil {
			return err
		}
		if j.Binding.PowerState == registry.PowerStopped && j.Binding.ExecutionEpoch == 0 && svc.Protocol == "vnc" {
			if err := s.admitOfflineDesktopService(ctx, j, svc); err != nil {
				return err
			}
		} else if _, _, err := workerServiceAttachment(ctx, worker, j.Binding); err != nil {
			return err
		}
		return j.Done(ctx, registry.TransitionRequest{
			Effect: registry.EffectApplied,
			Result: lifecycle.JSON(map[string]string{"service_id": svc.ID}),
			Apply: func(ctx context.Context, tx *sql.Tx) error {
				var present bool
				if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM services WHERE machine_id=? AND guest_port=?)", svc.MachineID, svc.GuestPort).Scan(&present); err != nil {
					return err
				}
				if present {
					return registry.ErrConflict
				}
				if err := s.insertService(ctx, tx, svc, true); err != nil {
					return err
				}
				return registry.AppendEvent(ctx, tx, "service", j.Op.MachineID, j.Op.ID, map[string]string{"service_id": svc.ID}, false)
			},
		})
	}
	var request struct {
		ID       string `json:"service_id"`
		Revision int64  `json:"revision"`
	}
	if err := json.Unmarshal(j.Op.Request, &request); err != nil {
		return err
	}
	return j.Done(ctx, registry.TransitionRequest{
		Effect: registry.EffectApplied,
		Result: lifecycle.JSON(map[string]string{"service_id": request.ID}),
		Apply: func(ctx context.Context, tx *sql.Tx) error {
			result, err := tx.ExecContext(ctx, "DELETE FROM services WHERE id=? AND machine_id=? AND revision=?", request.ID, j.Op.MachineID, request.Revision)
			if err != nil {
				return err
			}
			n, err := result.RowsAffected()
			if err != nil || n != 1 {
				return registry.ErrRevision
			}
			return registry.AppendEvent(ctx, tx, "service", j.Op.MachineID, j.Op.ID, map[string]string{"service_id": request.ID}, false)
		},
	})
}

func (s *Server) admitOfflineDesktopService(ctx context.Context, j *lifecycle.Job, svc service) error {
	notReady := &lifecycle.Failure{Code: "SERVICE_NOT_READY", Message: "an offline desktop declaration requires a first-start System VNC profile and a private network"}
	if svc.PublicHost != "" || svc.PublicPort != 0 {
		return notReady
	}
	desktop, err := s.requiresDesktopSession(ctx, svc)
	if err != nil {
		return err
	}
	client, managed := lifecycle.PolicyClient(j.Runtime, j.Binding)
	launcher, ok := j.Runtime.(rt.NativeLauncher)
	if !desktop || !managed || !ok {
		return notReady
	}
	machine, err := s.DB.GetMachine(ctx, svc.MachineID)
	if err != nil {
		return err
	}
	plan, err := s.FirstSystemLaunch(ctx, machine, j.Binding)
	if err != nil {
		return err
	}
	network := plan.Network
	if network == nil || network.NetworkID == "" || network.RouteDomainID == "" || network.CIDR == "" {
		return notReady
	}
	binding, err := launcher.DiscoverLaunchBinding(ctx, j.Binding.RuntimeName)
	if err != nil {
		return err
	}
	if binding.InstanceID != j.Binding.RuntimeInstanceObjectID || binding.IncarnationID != nil || binding.ExecutionEpoch != 0 {
		return registry.ErrStaleObservation
	}
	status, err := s.ObservePolicy(ctx, j.Binding, client)
	if err != nil {
		return err
	}
	if !status.FirstLaunchSupported || status.Power != "STOPPED" || status.Execution() != lifecycle.WorkerExecution(j.Binding) {
		return notReady
	}
	return nil
}
