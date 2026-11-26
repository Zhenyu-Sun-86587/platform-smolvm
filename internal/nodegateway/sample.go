package nodegateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
	"sandbox-platform/internal/nodeproto"
	"sandbox-platform/internal/placement"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/workerclient"
)

func stalled(resource string) (bool, error) {
	raw, err := os.ReadFile("/proc/pressure/" + resource)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "full" {
			for _, f := range fields[1:] {
				if value, ok := strings.CutPrefix(f, "avg10="); ok {
					v, err := strconv.ParseFloat(value, 64)
					return v >= 1, err
				}
			}
		}
	}
	return false, errors.New("missing PSI full average")
}

func hostPressure(diskFree int64) (*placement.Pressure, error) {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return nil, err
	}
	p := placement.Pressure{DiskFreeBytes: diskFree}
	var available int64
	found := false
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "MemAvailable:") {
			if _, err = fmt.Sscanf(line, "MemAvailable: %d kB", &available); err != nil || available < 0 {
				return nil, errors.New("invalid available memory observation")
			}
			p.AvailableMemoryMiB, found = available/1024, true
			break
		}
	}
	if !found {
		return nil, errors.New("missing available memory observation")
	}
	if p.MemoryStalled, err = stalled("memory"); err != nil {
		return nil, err
	}
	if p.DiskStalled, err = stalled("io"); err != nil {
		return nil, err
	}
	return &p, nil
}

func kvmReady() bool {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	const kvmGetAPIVersion = 0xae00
	version, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), kvmGetAPIVersion, 0)
	return errno == 0 && version == 12
}

func (s *Server) registerSample(mux *http.ServeMux) {
	var mu sync.Mutex
	var sequence int64
	rootRPC(s, mux, nodeproto.SamplePath, func(ctx context.Context, req nodeproto.SampleRequest) (nodeproto.SampleReply, error) {
		if !mu.TryLock() {
			return nodeproto.SampleReply{}, &nodeproto.Failure{Code: nodeproto.CodeBusy}
		}
		defer mu.Unlock()
		sequence++
		return s.collectSample(ctx, req, sequence)
	})
}

func (s *Server) collectSample(ctx context.Context, req nodeproto.SampleRequest, sequence int64) (nodeproto.SampleReply, error) {
	var reply nodeproto.SampleReply
	if !nodeproto.ValidID(req.Nonce) || len(req.Routes) == 0 || len(req.Routes) > 64 || len(req.Pending)+len(req.AccessWakes) > nodeproto.ReconcileLimit {
		return reply, rt.ErrInvalid
	}
	var worker workerclient.Client
	profiles := map[string]Route{}
	for _, p := range req.Routes {
		r, ok := s.routes[p.Route]
		if !ok || len(p.Artifacts) > 128 || r.Worker.Socket == "" || worker.Socket != "" && worker.Socket != r.Worker.Socket {
			return reply, rt.ErrInvalid
		}
		if _, dup := profiles[p.RuntimeInstanceID]; dup {
			return reply, rt.ErrInvalid
		}
		profiles[p.RuntimeInstanceID] = r
		worker = r.Worker
	}
	started := time.Now()
	h, err := worker.Health(ctx)
	if err != nil || h.HostBootID != s.bootID {
		return reply, rt.ErrUnavailable
	}
	sample := placement.Sample{WorkerID: s.authority.WorkerID, ControlEpoch: s.authority.ControlEpoch, EnrollmentGeneration: s.authority.EnrollmentGeneration,
		Session:  placement.Session{HostBootID: h.HostBootID, BrokerSessionID: h.BrokerSessionID, NodeSessionID: s.session},
		Sequence: sequence, Source: placement.SourceWorkerBudget, QueryNonce: req.Nonce}
	reply.Artifacts = map[string][]nodeproto.Artifact{}
	availableKVM := kvmReady()
	for _, requested := range req.Routes {
		route := profiles[requested.RuntimeInstanceID]
		measured, ok := route.Adapter.(interface {
			PlacementProfile(context.Context) (placement.RuntimeProfile, error)
		})
		p := placement.RuntimeProfile{}
		if ok {
			if verified, e := measured.PlacementProfile(ctx); e == nil && verified.LocalInstanceID == requested.LocalInstanceID && verified.BundleID == requested.BundleDigest {
				p = verified
			}
		}
		p.RuntimeInstanceID, p.LocalInstanceID, p.BundleID = requested.RuntimeInstanceID, requested.LocalInstanceID, requested.BundleDigest
		p.Ready = p.Ready && availableKVM
		for _, candidate := range requested.Artifacts {
			if !nodeproto.ValidDigest(candidate.Digest) || !nodeproto.ValidID(candidate.Generation) {
				return reply, rt.ErrInvalid
			}
			if !route.RootGenerations[candidate.Generation] {
				continue
			}
			if size, e := route.Worker.VerifyRoot(ctx, candidate.Generation, candidate.Digest); e == nil {
				candidate.Bytes = size
				p.Artifacts = append(p.Artifacts, candidate.Digest)
				reply.Artifacts[p.RuntimeInstanceID] = append(reply.Artifacts[p.RuntimeInstanceID], candidate)
			}
		}
		sample.Profiles = append(sample.Profiles, p)
	}
	// Only a native operation that has transactionally passed resourced admission
	// can be covered by the budget read below. A WakeDemand has no such receipt.
	for _, q := range req.Pending {
		route, ok := profiles[q.RuntimeInstanceID]
		if !ok || !nodeproto.ValidID(q.OperationID) || !nodeproto.ValidDigest(q.Digest) || q.Attempt < 1 {
			continue
		}
		native, ok := route.Adapter.(rt.NativeControl)
		if !ok {
			continue
		}
		receipt, e := native.QueryOperation(ctx, q.OperationID)
		if e != nil || receipt.OperationID != q.OperationID || receipt.RequestDigest != q.Digest || receipt.BundleDigest != route.Worker.Bundle ||
			receipt.RouteID != "deep" || receipt.Revision < 1 || receipt.InstanceID == "" {
			continue
		}
		kind := placement.AdmissionOperation
		switch receipt.Kind {
		case "launch":
			if receipt.Phase != "COMPLETED" || receipt.Execution == nil {
				continue
			}
		case "independent_fork", "fork_batch":
			if receipt.Phase != "COMPLETED" || receipt.Fork == nil || receipt.Fork.Stage != "complete" ||
				slices.ContainsFunc(receipt.Fork.Children, func(c rt.ForkChildReceipt) bool { return c.Phase != "COMPLETED" && c.Phase != "ABORTED" }) ||
				!slices.ContainsFunc(receipt.Fork.Children, func(c rt.ForkChildReceipt) bool { return c.Attempt == q.NativeAttempt && c.AdmissionError == "" }) {
				continue
			}
		case "vm_restore":
			if receipt.Phase != "COMPLETED" || receipt.StatePoint == nil || receipt.StatePoint.ActivationDecision != "commit" || receipt.StatePoint.Stage != "released" {
				continue
			}
			kind = placement.AdmissionRestore
		default:
			continue
		}
		reply.Admissions = append(reply.Admissions, placement.Admission{OperationID: q.OperationID, Attempt: q.Attempt, WorkerID: sample.WorkerID, RuntimeInstanceID: q.RuntimeInstanceID, BundleID: receipt.BundleDigest, ControlEpoch: sample.ControlEpoch,
			EnrollmentGeneration: sample.EnrollmentGeneration, NativeDigest: q.Digest, Kind: kind, ReceiptID: receipt.OperationID, Session: sample.Session, ConfirmedThroughSequence: sequence - 1})
	}
	// Access wake receipts are native launch/restore decisions observed before
	// this budget. The controller also checks the original demand and execution.
	for _, q := range req.AccessWakes {
		route, ok := profiles[q.RuntimeInstanceID]
		if !ok || !nodeproto.ValidID(q.OperationID) {
			return reply, rt.ErrInvalid
		}
		native, ok := route.Adapter.(rt.NativeControl)
		if !ok {
			continue
		}
		receipt, err := native.QueryOperation(ctx, q.OperationID)
		if err == nil && receipt.OperationID == q.OperationID && receipt.Phase == "COMPLETED" &&
			(receipt.Kind == "launch" || receipt.Kind == rt.VMRestore) && receipt.BundleDigest == route.Worker.Bundle {
			reply.AccessWakes = append(reply.AccessWakes, nodeproto.AccessWakeAdmission{AccessWakeQuery: q, Receipt: receipt})
		}
	}
	sample.Budget, err = worker.Budget(ctx)
	if err != nil {
		return reply, err
	}
	// The Node admits one placement-heavy operation at a time. The journal also
	// charges unknown or in-flight command receipts until the native query settles.
	sample.Budget.Capacity.HeavySlots = 1
	if err = s.journal.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM commands WHERE refusal IS NULL AND (receipt IS NULL OR json_extract(receipt,'$.phase') NOT IN ('COMPLETED','ABORTED')))`).Scan(&sample.Budget.Allocated.HeavySlots); err != nil {
		return reply, err
	}
	sample.Pressure, err = hostPressure(sample.Budget.Capacity.StorageBytes)
	if err != nil {
		return reply, err
	}
	end, err := worker.Health(ctx)
	if err != nil || end != h {
		return reply, rt.ErrUnavailable
	}
	sample.SampleAge = time.Since(started)
	reply.Sample = sample
	return reply, nil
}
