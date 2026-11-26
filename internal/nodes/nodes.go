// Package nodes connects a single registry writer to operator-enrolled Workers.
package nodes

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"sandbox-platform/internal/config"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/nodeclient"
	"sandbox-platform/internal/nodeproto"
	"sandbox-platform/internal/placement"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/customsmolvm"
	wc "sandbox-platform/internal/runtime/workerclient"
)

type Worker struct {
	reached                   bool
	config                    config.Node
	DB                        *registry.Store
	Client                    *nodeclient.Client
	Authority                 nodeproto.Authority
	routes                    []config.NodeRoute
	lastSession               placement.Session
	lastProcesses             map[string]string
	pendingOffset, wakeOffset int
	engine                    *lifecycle.Engine
}

func NewClient(n config.Node, control registry.Control, db *registry.Store) (*nodeclient.Client, nodeproto.Authority, error) {
	authority := nodeproto.Authority{WorkerID: n.ID, EnrollmentGeneration: n.Generation, ControllerID: control.InstallationID, ControlEpoch: control.ControlEpoch}
	cert, roots, err := nodeproto.LoadTLS(n.Certificate, n.Key, n.CA)
	if err != nil {
		return nil, authority, err
	}
	c, err := nodeclient.New(nodeclient.Config{Authority: authority, Endpoint: n.Endpoint, NodeSPKIPin: n.Pin, TLS: &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: roots},
		Authorize: func(ctx context.Context) error {
			var active bool
			err := db.ReadDB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM worker_enrollments e JOIN workers w ON w.id=e.worker_id JOIN meta m ON m.id=1
                WHERE e.worker_id=? AND e.generation=? AND e.certificate_fingerprint=? AND e.status='active' AND e.authority=? AND m.control_epoch=e.authority AND m.installation_id=?
                AND w.desired_mode<>'decommissioned' AND w.observed_status<>'identity_conflict')`, n.ID, n.Generation, n.Pin, authority.ControlEpoch, authority.ControllerID).Scan(&active)
			if err != nil {
				return err
			}
			if !active {
				return &nodeproto.Failure{Code: nodeproto.CodeAuthority}
			}
			return nil
		}})
	return c, authority, err
}

func Register(ctx context.Context, db *registry.Store, configNodes []config.Node, adapters map[rt.BundleRoute]rt.Adapter) ([]*Worker, error) {
	control, err := db.Control(ctx)
	if err != nil {
		return nil, err
	}
	var nodes []*Worker
	for _, n := range configNodes {
		name := n.Name
		if name == "" {
			name = n.ID
		}
		if err := db.RegisterWorker(ctx, registry.Worker{ID: n.ID, Name: name, Endpoint: n.Endpoint}); err != nil {
			return nil, err
		}
		if _, err = db.DB().ExecContext(ctx, `UPDATE workers SET observed_status='recovering' WHERE id=? AND observed_status IN ('ready','degraded')`, n.ID); err != nil {
			return nil, err
		}
		client, authority, err := NewClient(n, control, db)
		if err != nil {
			return nil, fmt.Errorf("Worker %s: %w", n.ID, err)
		}
		worker := &Worker{DB: db, Client: client, Authority: authority, config: n}
		for _, r := range n.Routes {
			manifest, err := (customsmolvm.Route{Bundle: r.BundleDigest, Reader: r.Reader, Manifest: r.Manifest}).LoadManifest()
			if err != nil {
				return nil, err
			}
			key := rt.BundleRoute{WorkerID: n.ID, InstanceID: r.RuntimeInstanceID, LocalInstanceID: r.LocalInstanceID, BundleID: r.BundleDigest}
			if err = db.RegisterRuntime(ctx, key, r.LocalInstanceID, manifest); err != nil {
				return nil, err
			}
			allowed, err := db.RuntimeBundleRouteAllowed(ctx, key.BundleID)
			if err != nil {
				return nil, err
			}
			if allowed {
				adapters[key] = client.Adapter(r.Route)
				worker.routes = append(worker.routes, r)
			}
		}
		nodes = append(nodes, worker)
	}
	return nodes, nil
}

func Run(ctx context.Context, nodes []*Worker, engine *lifecycle.Engine) {
	var wg sync.WaitGroup
	for _, n := range nodes {
		n.engine = engine
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer n.Client.CloseIdleConnections()
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			for {
				call, cancel := context.WithTimeout(ctx, 12*time.Second)
				err := n.refresh(call)
				cancel()
				if err != nil && ctx.Err() == nil {
					// Stop new admission immediately. Last confirmed machine power
					// state and reservation evidence survive the link failure.
					status := "unreachable"
					if n.reached {
						status = "degraded"
					}
					_, dbErr := n.DB.DB().ExecContext(ctx, `UPDATE workers SET observed_status=? WHERE id=? AND observed_status NOT IN ('identity_conflict',?)`, status, n.Authority.WorkerID, status)
					if dbErr != nil {
						slog.Warn("record Worker reachability", "worker", n.Authority.WorkerID, "error", dbErr)
					}
					slog.Debug("Worker refresh", "worker", n.Authority.WorkerID, "error", err)
					n.lastSession = placement.Session{}
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	wg.Wait()
}

func (n *Worker) refresh(ctx context.Context) error {
	n.reached = false
	hello, err := n.Client.Handshake(ctx)
	if err != nil {
		return err
	}
	n.reached = true
	for _, r := range n.routes {
		if !slices.ContainsFunc(hello.Routes, func(i nodeproto.RouteInfo) bool { return i.Route == r.Route }) {
			return errors.New("Worker lacks a registered runtime route")
		}
	}
	if err = n.DB.EnrollWorker(ctx, registry.WorkerEnrollment{WorkerID: n.Authority.WorkerID, Generation: int64(n.Authority.EnrollmentGeneration), CertificateFingerprint: n.config.Pin, Authority: n.Authority.ControlEpoch}); err != nil {
		return err
	}
	// Close the Node's admission gates as soon as the enrollment is verified.
	// Even an unavailable budget must not delay a requested cordon or drain.
	mode, err := n.DB.GetWorkerMaintenance(ctx, n.config.ID)
	if err != nil {
		return err
	}
	if mode.Mode == "cordoned" || mode.Mode == "draining" {
		if _, err = n.syncGate(ctx, mode.Mode); err != nil {
			return err
		}
	}
	pending, err := n.DB.ListPlacementRechecks(ctx, n.Authority.WorkerID)
	if err != nil {
		return err
	}
	query := nodeproto.SampleRequest{}
	query.Nonce, err = registry.NewID("sample-")
	if err != nil {
		return err
	}
	for _, r := range n.routes {
		wanted := nodeproto.RequestedRoute{Route: r.Route}
		for digest, generation := range r.Artifacts {
			wanted.Artifacts = append(wanted.Artifacts, nodeproto.Artifact{Digest: digest, Generation: generation})
		}
		query.Routes = append(query.Routes, wanted)
	}
	checks, err := n.DB.ListAccessWakeChecks(ctx, n.config.ID)
	if err != nil {
		return err
	}
	wakes := make(map[nodeproto.AccessWakeQuery][]registry.AccessWakeCheck)
	for _, w := range pageChecks(checks, &n.wakeOffset, nodeproto.ReconcileLimit/2) {
		key := nodeproto.AccessWakeQuery{RuntimeInstanceID: w.RuntimeInstanceID, OperationID: w.OperationID}
		if len(wakes[key]) == 0 {
			query.AccessWakes = append(query.AccessWakes, key)
		}
		wakes[key] = append(wakes[key], w)
	}
	for _, p := range pageChecks(pending, &n.pendingOffset, nodeproto.ReconcileLimit-len(query.AccessWakes)) {
		query.Pending = append(query.Pending, nodeproto.AdmissionQuery{RuntimeInstanceID: p.RuntimeInstanceID, OperationID: p.OperationID, Digest: p.NativeDigest, Attempt: p.Attempt, NativeAttempt: p.NativeAttempt})
	}
	window := placement.Query{Nonce: query.Nonce, StartedAt: time.Now()}
	sampleCtx, cancel := context.WithTimeout(ctx, placement.MaxQueryDuration)
	result, err := n.Client.Sample(sampleCtx, query)
	cancel()
	window.CompletedAt = time.Now()
	if err != nil {
		return err
	}
	s := result.Sample
	if err = s.Validate(window, time.Now()); err != nil {
		return err
	}
	if s.WorkerID != n.Authority.WorkerID || s.ControlEpoch != n.Authority.ControlEpoch || s.EnrollmentGeneration != n.Authority.EnrollmentGeneration {
		return errors.New("Worker sample authority mismatch")
	}
	if s.Session != n.lastSession {
		if err = n.DB.CheckWorkerSampleSession(ctx, s, window); err != nil {
			return err
		}
	}
	if s.Session.NodeSessionID != hello.NodeSession || s.Session.HostBootID != hello.HostBootID || len(s.Profiles) != len(n.routes) {
		return errors.New("Worker sample differs from its handshake")
	}
	processes := make(map[string]string, len(s.Profiles))
	changed := s.Session != n.lastSession
	for _, r := range s.Profiles {
		processes[r.RuntimeInstanceID] = r.RuntimeProcessID
		if n.lastProcesses[r.RuntimeInstanceID] != r.RuntimeProcessID {
			changed = true
		}
	}
	if changed {
		if err = n.reconcile(ctx, pending); err != nil {
			return err
		}
	}
	s.Reconciled = true
	for _, profile := range s.Profiles {
		index := slices.IndexFunc(n.routes, func(r config.NodeRoute) bool {
			return r.RuntimeInstanceID == profile.RuntimeInstanceID && r.LocalInstanceID == profile.LocalInstanceID && r.BundleDigest == profile.BundleID && (!profile.Ready || r.Reader == profile.Reader)
		})
		if index < 0 {
			return errors.New("unregistered Worker sample profile")
		}
		verified := result.Artifacts[profile.RuntimeInstanceID]
		if len(verified) != len(profile.Artifacts) {
			return errors.New("template observations differ from placement sample")
		}
		for _, a := range verified {
			if !slices.Contains(profile.Artifacts, a.Digest) || n.routes[index].Artifacts[a.Digest] != a.Generation || a.Bytes <= 0 {
				return errors.New("unexpected Worker template receipt")
			}
			if err = n.DB.RecordArtifactPlacement(ctx, s.WorkerID, profile.RuntimeInstanceID, profile.BundleID, a.Digest, a.Generation, a.Bytes); err != nil {
				return err
			}
		}
	}
	if err = n.DB.RecordWorkerSample(ctx, s, window); err != nil {
		return err
	}
	n.lastSession, n.lastProcesses = s.Session, processes
	for _, proof := range result.Admissions {
		if proof.Session != s.Session || proof.WorkerID != s.WorkerID || proof.ConfirmedThroughSequence != s.Sequence-1 {
			return errors.New("Worker admission evidence differs from its budget sample")
		}
		if err = n.DB.RecordPlacementAdmission(ctx, proof); err != nil && !errors.Is(err, registry.ErrOperationState) {
			return err
		}
	}
	for _, w := range result.AccessWakes {
		known := wakes[w.AccessWakeQuery]
		if len(known) == 0 {
			return errors.New("unsolicited Worker wake receipt")
		}
		for _, check := range known {
			if err := n.DB.RecordAccessWakeAdmission(ctx, check, w.Receipt, s, s.Sequence-1); err != nil &&
				!errors.Is(err, registry.ErrInvalid) && !errors.Is(err, registry.ErrOperationState) {
				return err
			}
		}
	}
	return n.maintenance(ctx, s)
}

// Cycle across a bounded window so a permanently unresolved operation cannot starve later receipts.
func pageChecks[T any](rows []T, offset *int, limit int) []T {
	if len(rows) <= limit {
		*offset = 0
		return rows
	}
	start := *offset % len(rows)
	end := start + limit
	*offset = end % len(rows)
	if end <= len(rows) {
		return rows[start:end]
	}
	out := make([]T, 0, limit)
	out = append(out, rows[start:]...)
	return append(out, rows[:end-len(rows)]...)
}

// Reconcile only IDs retained by platform or the enrolled Node. A missing
// Hibernate generation or a foreign journal entry holds the Worker in recovery.
func (n *Worker) reconcile(ctx context.Context, pending []placement.Reservation) error {
	for _, r := range n.routes {
		rows, err := n.DB.ReadDB().QueryContext(ctx, `SELECT runtime_name,runtime_instance_object_id,COALESCE(execution_epoch,0),COALESCE(incarnation_id,''),power_state,COALESCE(last_capture_id,'') FROM runtime_bindings
			WHERE worker_id=? AND runtime_instance_id=? AND status='current' AND COALESCE(runtime_instance_object_id,'')<>'' ORDER BY id`, n.config.ID, r.RuntimeInstanceID)
		if err != nil {
			return err
		}
		type binding struct {
			instance       string
			epoch          int64
			incarnation    string
			power, capture string
		}
		known := make(map[string]binding)
		var names []string
		for rows.Next() {
			var name string
			var b binding
			if err = rows.Scan(&name, &b.instance, &b.epoch, &b.incarnation, &b.power, &b.capture); err != nil {
				rows.Close()
				return err
			}
			known[name] = b
			names = append(names, name)
		}
		if err = errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		ops := map[string]string{}
		var ids []string
		for _, p := range pending {
			if p.RuntimeInstanceID == r.RuntimeInstanceID {
				if _, ok := ops[p.OperationID]; !ok {
					ids = append(ids, p.OperationID)
				}
				ops[p.OperationID] = p.NativeDigest
			}
		}
		a := n.Client.Adapter(r.Route)
		for len(names)+len(ids) > 0 {
			bn := min(len(names), nodeproto.ReconcileLimit)
			on := min(len(ids), nodeproto.ReconcileLimit-bn)
			q := nodeproto.ReconcileRequest{KnownIDsOnly: true, Operations: ids[:on], Bindings: names[:bn]}
			got, err := a.ReconcileKnown(ctx, q)
			if err != nil {
				return err
			}
			if got.Coverage != nodeproto.KnownOnly || got.JournalLimited || len(got.Bindings) != bn || len(got.Operations) != on {
				return errors.New("incomplete Worker reconciliation")
			}
			for i, b := range got.Bindings {
				p := known[q.Bindings[i]]
				if b.Name != q.Bindings[i] || b.Binding == nil || b.Issue != "" || b.Binding.InstanceID != p.instance || b.Binding.ExecutionEpoch < p.epoch ||
					b.Binding.ExecutionEpoch == p.epoch && b.Binding.IncarnationID != nil && p.incarnation != "" && *b.Binding.IncarnationID != p.incarnation {
					return errors.New("known binding differs from Worker evidence")
				}
				if p.power == string(registry.PowerHibernated) {
					execution := wc.Execution{InstanceID: b.Binding.InstanceID, Epoch: b.Binding.ExecutionEpoch}
					if b.Binding.IncarnationID != nil {
						execution.IncarnationID = *b.Binding.IncarnationID
					}
					var status lifecycle.WorkerLifecycleStatus
					if err := a.Lifecycle(ctx, "GetLifecycleStatus", "", execution, struct{}{}, &status); err != nil {
						return err
					}
					if err := checkHibernatedBinding(execution, p.capture, status); err != nil {
						return err
					}
				}
			}
			for _, op := range got.Operations {
				digest, ok := ops[op.OperationID]
				if !ok || op.Receipt != nil && (op.Receipt.OperationID != op.OperationID || op.Receipt.BundleDigest != r.BundleDigest || op.Receipt.RequestDigest != digest) {
					return errors.New("known operation differs from Worker evidence")
				}
			}
			names, ids = names[bn:], ids[on:]
		}
		var after string
		for {
			got, err := a.ReconcileKnown(ctx, nodeproto.ReconcileRequest{JournalAfter: after})
			if err != nil {
				return err
			}
			if got.Coverage != nodeproto.KnownOnly {
				return errors.New("invalid Worker reconciliation coverage")
			}
			for _, op := range got.Operations {
				var belongs bool
				err := n.DB.ReadDB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM operations WHERE id=? AND worker_id=? AND runtime_instance_id=?)`, op.OperationID, n.config.ID, r.RuntimeInstanceID).Scan(&belongs)
				if err != nil {
					return err
				}
				if !belongs {
					return fmt.Errorf("unresolved journal operation %s on Worker %s", op.OperationID, n.config.ID)
				}
			}
			if !got.JournalLimited {
				break
			}
			if got.NextJournalAfter <= after {
				return errors.New("Worker journal page did not advance")
			}
			after = got.NextJournalAfter
		}
	}
	return nil
}

func checkHibernatedBinding(expected wc.Execution, capture string, status lifecycle.WorkerLifecycleStatus) error {
	if capture == "" || status.Instance != expected.InstanceID || status.Epoch != expected.Epoch || status.Power != "HIBERNATED" || status.StatePoint != capture {
		return errors.New("Worker cannot confirm the saved Hibernate capture for its binding")
	}
	return nil
}

func (n *Worker) syncGate(ctx context.Context, mode string) (nodeproto.Gate, error) {
	gate, err := n.Client.Gate(ctx)
	if err != nil || gate.Mode == mode {
		return gate, err
	}
	return n.Client.SetGate(ctx, nodeproto.SetGate{Mode: mode, ExpectedRevision: gate.Revision})
}

func (n *Worker) maintenance(ctx context.Context, s placement.Sample) error {
	control, err := n.DB.Control(ctx)
	if err != nil {
		return err
	}
	if control.Maintenance {
		rows, err := n.DB.ListWorkerAuthorityRecovery(ctx)
		if err != nil {
			return err
		}
		for _, r := range rows {
			if r.WorkerID == s.WorkerID && r.ReboundGeneration == nil {
				return n.DB.ConfirmWorkerAuthority(ctx, registry.WorkerAuthorityProof{WorkerID: s.WorkerID,
					ControlEpoch: s.ControlEpoch, EnrollmentGeneration: s.EnrollmentGeneration, Session: s.Session,
					BudgetSequence: s.Sequence, Coverage: registry.InventoryKnownOnly})
			}
		}
		return nil
	}
	m, err := n.DB.GetWorkerMaintenance(ctx, s.WorkerID)
	if err != nil {
		return err
	}
	if m.Mode != "cordoned" && m.Mode != "draining" && m.Mode != "active" {
		return nil
	}
	facts, err := n.engine.ReconcileWorkerMaintenance(ctx, s.WorkerID, m.Revision)
	if err != nil {
		return err
	}
	if m.Phase == "activating" && len(facts.UnresolvedResources) != 0 {
		return nil
	}
	gate, err := n.syncGate(ctx, m.Mode)
	if err != nil {
		return err
	}
	if m.Phase == "activating" {
		unresolved, err := n.engine.RestoreWorkerPacketRules(ctx, s.WorkerID, m.Revision)
		if err != nil {
			return err
		}
		facts.UnresolvedResources = append(facts.UnresolvedResources, unresolved...)
	}
	if m.Phase == "active" && m.Mode == "active" {
		return nil
	}
	evidence := registry.WorkerMaintenanceEvidence{Coverage: registry.InventoryKnownOnly, GateMode: gate.Mode, GateRevision: uint64(gate.Revision),
		ControlEpoch: s.ControlEpoch, Session: s.Session, BudgetSequence: s.Sequence, CollectorsActive: facts.CollectorsActive,
		Rules: facts.Rules, PendingWakeOperations: facts.PendingWakeOperations, UnresolvedResources: facts.UnresolvedResources}
	_, err = n.DB.RecordWorkerMaintenanceEvidence(ctx, s.WorkerID, m.Revision, evidence)
	return err
}
