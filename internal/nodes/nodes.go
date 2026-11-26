// Package nodes connects a single registry writer to operator-enrolled Workers.
package nodes

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"sandbox-platform/internal/config"
	"sandbox-platform/internal/nodeclient"
	"sandbox-platform/internal/nodeproto"
	"sandbox-platform/internal/placement"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/customsmolvm"
)

type Worker struct {
	config      config.Node
	DB          *registry.Store
	Client      *nodeclient.Client
	Authority   nodeproto.Authority
	routes      []config.NodeRoute
	lastSession placement.Session
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
                WHERE e.worker_id=? AND e.generation=? AND e.certificate_fingerprint=? AND e.status='active' AND e.authority=? AND m.control_epoch=e.authority
                AND w.desired_mode<>'decommissioned' AND w.observed_status<>'identity_conflict')`, n.ID, n.Generation, n.Pin, authority.ControlEpoch).Scan(&active)
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

func Run(ctx context.Context, nodes []*Worker) {
	var wg sync.WaitGroup
	for _, n := range nodes {
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
					_, dbErr := n.DB.DB().ExecContext(ctx, `UPDATE workers SET observed_status='unreachable' WHERE id=? AND observed_status NOT IN ('identity_conflict','unreachable')`, n.Authority.WorkerID)
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
	hello, err := n.Client.Handshake(ctx)
	if err != nil {
		return err
	}
	for _, r := range n.routes {
		if !slices.ContainsFunc(hello.Routes, func(i nodeproto.RouteInfo) bool { return i.Route == r.Route }) {
			return errors.New("Worker lacks a registered runtime route")
		}
	}
	if err = n.DB.EnrollWorker(ctx, registry.WorkerEnrollment{WorkerID: n.Authority.WorkerID, Generation: int64(n.Authority.EnrollmentGeneration), CertificateFingerprint: n.config.Pin, Authority: n.Authority.ControlEpoch}); err != nil {
		return err
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
	for _, p := range pending {
		if len(query.Pending) >= nodeproto.ReconcileLimit {
			break
		}
		query.Pending = append(query.Pending, nodeproto.AdmissionQuery{RuntimeInstanceID: p.RuntimeInstanceID, OperationID: p.OperationID, Digest: p.NativeDigest, Attempt: p.Attempt})
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
	if s.WorkerID != n.Authority.WorkerID || s.ControlEpoch != n.Authority.ControlEpoch || s.EnrollmentGeneration != n.Authority.EnrollmentGeneration ||
		s.Session.NodeSessionID != hello.NodeSession || s.Session.HostBootID != hello.HostBootID || len(s.Profiles) != len(n.routes) {
		return errors.New("Worker sample differs from its handshake")
	}
	if s.Session != n.lastSession {
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
	n.lastSession = s.Session
	for _, proof := range result.Admissions {
		if proof.Session != s.Session || proof.WorkerID != s.WorkerID || proof.ConfirmedThroughSequence != s.Sequence-1 {
			return errors.New("Worker admission evidence differs from its budget sample")
		}
		if err = n.DB.RecordPlacementAdmission(ctx, proof); err != nil && !errors.Is(err, registry.ErrOperationState) {
			return err
		}
	}
	return n.maintenance(ctx, s)
}

func (n *Worker) reconcile(ctx context.Context, pending []placement.Reservation) error {
	for _, r := range n.routes {
		rows, err := n.DB.ReadDB().QueryContext(ctx, "SELECT runtime_name FROM runtime_bindings WHERE worker_id=? AND runtime_instance_id=? AND status='current' ORDER BY id", n.config.ID, r.RuntimeInstanceID)
		if err != nil {
			return err
		}
		var query nodeproto.ReconcileRequest
		for rows.Next() {
			var name string
			if err = rows.Scan(&name); err != nil {
				rows.Close()
				return err
			}
			query.Bindings = append(query.Bindings, name)
		}
		err = errors.Join(rows.Err(), rows.Close())
		if err != nil {
			return err
		}
		for _, p := range pending {
			if p.RuntimeInstanceID == r.RuntimeInstanceID {
				query.Operations = append(query.Operations, p.OperationID)
			}
		}
		adapter := n.Client.Adapter(r.Route)
		for {
			batch := nodeproto.ReconcileRequest{Operations: query.Operations[:min(len(query.Operations), nodeproto.ReconcileLimit)], Bindings: query.Bindings[:min(len(query.Bindings), nodeproto.ReconcileLimit)]}
			got, err := adapter.ReconcileKnown(ctx, batch)
			if err != nil {
				return err
			}
			if got.Coverage != nodeproto.KnownOnly || got.JournalLimited {
				return errors.New("known Worker reconciliation was incomplete")
			}
			for _, b := range got.Bindings {
				if b.Issue != "" || b.Binding == nil {
					return errors.New("known binding is unresolved on its Worker")
				}
			}
			query.Operations = query.Operations[len(batch.Operations):]
			query.Bindings = query.Bindings[len(batch.Bindings):]
			if len(query.Operations)+len(query.Bindings) == 0 {
				break
			}
		}
	}
	return nil
}

func (n *Worker) maintenance(ctx context.Context, s placement.Sample) error {
	m, err := n.DB.GetWorkerMaintenance(ctx, s.WorkerID)
	if err != nil {
		return err
	}
	gate, err := n.Client.Gate(ctx)
	if err != nil {
		return err
	}
	control, err := n.DB.Control(ctx)
	if err != nil {
		return err
	}
	if control.Maintenance {
		return nil
	}
	if gate.Mode != m.Mode {
		switch m.Mode {
		case "active", "cordoned", "draining":
		default:
			return nil
		}
		gate, err = n.Client.SetGate(ctx, nodeproto.SetGate{Mode: m.Mode, ExpectedRevision: gate.Revision})
		if err != nil {
			return err
		}
	}
	if m.Phase == "active" && m.Mode == "active" {
		return nil
	}
	evidence := registry.WorkerMaintenanceEvidence{Coverage: registry.InventoryKnownOnly, GateMode: gate.Mode, GateRevision: uint64(gate.Revision),
		ControlEpoch: s.ControlEpoch, Session: s.Session, BudgetSequence: s.Sequence}
	_, err = n.DB.RecordWorkerMaintenanceEvidence(ctx, s.WorkerID, m.Revision, evidence)
	if errors.Is(err, sql.ErrNoRows) {
		return registry.ErrNotFound
	}
	return err
}
