package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	rt "sandbox-platform/internal/runtime"
)

// RegisterRuntime records the immutable endpoint identity and bundle pin.
func (s *Store) RegisterRuntime(ctx context.Context, route rt.BundleRoute, endpoint string, manifest json.RawMessage) error {
	if !validWorkerID(route.Owner()) || route.InstanceID == "" || route.LocalID() == "" ||
		route.BundleID == "" || endpoint == "" || !json.Valid(manifest) {
		return fmt.Errorf("%w: runtime instance, bundle, endpoint and manifest are required", ErrInvalid)
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var mode string
	if err := tx.QueryRowContext(ctx, `SELECT desired_mode FROM workers WHERE id=?`, route.Owner()).Scan(&mode); err != nil {
		return notFound(err)
	}
	if mode == "decommissioned" {
		return ErrMaintenance
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO runtime_bundles(id, manifest_digest, manifest_json, verification_status, security_status, created_at)
		VALUES (?, ?, ?, 'manifest_pinned', 'unknown', ?) ON CONFLICT (id) DO NOTHING`,
		route.BundleID, route.BundleID, string(manifest), time.Now().UnixMilli()); err != nil {
		return uniqueConflict(err)
	}
	endpointKey := "worker/" + route.Owner() + "/runtime/" + route.InstanceID
	if _, err := tx.ExecContext(ctx, `INSERT INTO runtime_instances
		(id,lane,runtime_bundle_id,endpoint_key,status,revision,worker_id,local_instance_id,worker_endpoint_key)
		VALUES (?, 'deep', ?, ?, 'registered', 1, ?, ?, ?) ON CONFLICT (id) DO NOTHING`,
		route.InstanceID, route.BundleID, endpointKey, route.Owner(), route.LocalID(), endpoint); err != nil {
		return uniqueConflict(err)
	}
	var bundle, existingEndpoint, owner, localID string
	if err := tx.QueryRowContext(ctx, `SELECT runtime_bundle_id,worker_endpoint_key,worker_id,local_instance_id FROM runtime_instances WHERE id = ?`, route.InstanceID).
		Scan(&bundle, &existingEndpoint, &owner, &localID); err != nil {
		return err
	}
	if bundle != route.BundleID || existingEndpoint != endpoint || owner != route.Owner() || localID != route.LocalID() {
		return fmt.Errorf("%w: runtime instance %q already has another Worker, Serve identity, endpoint or bundle", ErrConflict, route.InstanceID)
	}
	return tx.Commit()
}

func (s *Store) GetRuntimeRoute(ctx context.Context, instanceID string) (rt.BundleRoute, error) {
	return loadRuntimeRoute(ctx, s.reader, instanceID)
}

func (s *Store) RuntimeBundleRouteAllowed(ctx context.Context, bundle string) (bool, error) {
	var status string
	err := s.reader.QueryRowContext(ctx, `SELECT security_status FROM runtime_bundles WHERE id = ?`, bundle).Scan(&status)
	if err != nil {
		return false, err
	}
	switch status {
	case "unknown", "active", "legacy-allowed", "ready":
		return true, nil
	default:
		return false, nil
	}
}

func loadRuntimeRoute(ctx context.Context, db queryer, instanceID string) (rt.BundleRoute, error) {
	instance, err := readRuntimeInstance(ctx, db, instanceID)
	if errors.Is(err, ErrNotFound) {
		return rt.BundleRoute{}, &rt.Error{Code: "NOT_SUPPORTED", Message: "runtime instance is not registered"}
	}
	return instance.BundleRoute(), err
}

type RuntimeInstance struct {
	ID              string `json:"runtime_instance_id"`
	WorkerID        string `json:"worker_id"`
	LocalInstanceID string `json:"local_instance_id"`
	BundleID        string `json:"runtime_bundle_id"`
	EndpointKey     string `json:"endpoint_key"`
	Status          string `json:"status"`
	Revision        int64  `json:"revision"`
}

func (instance RuntimeInstance) BundleRoute() rt.BundleRoute {
	return rt.BundleRoute{WorkerID: instance.WorkerID, InstanceID: instance.ID, BundleID: instance.BundleID, LocalInstanceID: instance.LocalInstanceID}
}

func readRuntimeInstance(ctx context.Context, db queryer, id string) (RuntimeInstance, error) {
	var instance RuntimeInstance
	err := db.QueryRowContext(ctx, `SELECT id,worker_id,local_instance_id,runtime_bundle_id,endpoint_key,status,revision
		FROM runtime_instances WHERE id=?`, id).Scan(&instance.ID, &instance.WorkerID, &instance.LocalInstanceID,
		&instance.BundleID, &instance.EndpointKey, &instance.Status, &instance.Revision)
	return instance, notFound(err)
}

func (s *Store) GetRuntimeInstance(ctx context.Context, id string) (RuntimeInstance, error) {
	return readRuntimeInstance(ctx, s.reader, id)
}

func snapshotRuntimeRoute(ctx context.Context, tx *sql.Tx, request string) (rt.BundleRoute, error) {
	var route rt.BundleRoute
	err := tx.QueryRowContext(ctx, `SELECT i.worker_id,i.id,i.local_instance_id,i.runtime_bundle_id
		FROM vm_snapshot_provenance p JOIN runtime_instances i ON i.id=p.runtime_instance_id AND i.runtime_bundle_id=p.runtime_bundle_id
		JOIN runtime_bundles b ON b.id=p.runtime_bundle_id
		WHERE p.snapshot_id=json_extract(?,'$.snapshot_id')
		AND json_extract(b.manifest_json,'$.contracts.state_reader_id')=p.state_reader_id`, request).
		Scan(&route.WorkerID, &route.InstanceID, &route.LocalInstanceID, &route.BundleID)
	if errors.Is(err, sql.ErrNoRows) {
		return rt.BundleRoute{}, fmt.Errorf("%w: StatePoint requires its original Worker route, Bundle and reader", ErrConflict)
	}
	return route, err
}

type RuntimeProfile struct {
	WorkerID             string                   `json:"worker_id"`
	LocalInstanceID      string                   `json:"local_instance_id"`
	RuntimeInstanceID    string                   `json:"runtime_instance_id"`
	RuntimeBundleID      string                   `json:"runtime_bundle_id"`
	StateReaderID        string                   `json:"state_reader_id"`
	Status               string                   `json:"status"`
	VerificationStatus   string                   `json:"verification_status"`
	SecurityStatus       string                   `json:"security_status"`
	SecurityReason       string                   `json:"security_reason,omitempty"`
	BootProfiles         []string                 `json:"boot_profiles"`
	RunningBindings      int64                    `json:"running_bindings"`
	StandbyBindings      int64                    `json:"standby_bindings"`
	HibernatedBindings   int64                    `json:"hibernated_bindings"`
	BundleBindingRefs    int64                    `json:"bundle_binding_refs"`
	BundleSnapshotRefs   int64                    `json:"bundle_snapshot_refs"`
	BundleStatePointRefs int64                    `json:"bundle_statepoint_refs"`
	Capabilities         map[string]rt.Capability `json:"capabilities"`
}

func (s *Store) ListRuntimeProfiles(ctx context.Context) ([]RuntimeProfile, error) {
	rows, err := s.reader.QueryContext(ctx, `
	WITH current_bindings AS (
		SELECT b.runtime_instance_id,
			COUNT(*) FILTER (WHERE b.power_state='running') AS running,
			COUNT(*) FILTER (WHERE b.power_state='standby') AS standby,
			COUNT(*) FILTER (WHERE b.power_state='hibernated') AS hibernated
		FROM machines m JOIN runtime_bindings b ON b.id = m.current_binding_id
		WHERE m.deleted_at IS NULL AND b.runtime_instance_id IS NOT NULL
		GROUP BY b.runtime_instance_id
	), bundle_refs AS (
		SELECT bundle_id, COUNT(*) FILTER (WHERE owner_kind='binding') AS bindings,
			COUNT(*) FILTER (WHERE owner_kind='snapshot') AS snapshots,
			COUNT(*) FILTER (WHERE owner_kind='statepoint') AS statepoints
		FROM runtime_bundle_refs GROUP BY bundle_id
	)
	SELECT i.id, i.worker_id, i.local_instance_id, b.id, COALESCE(json_extract(b.manifest_json, '$.contracts.state_reader_id'), ''),
		i.status, b.verification_status, b.security_status, COALESCE(s.reason, ''),
		COALESCE(json_extract(b.manifest_json, '$.guest.boot_profiles'), '[]'),
		COALESCE(c.running, 0), COALESCE(c.standby, 0), COALESCE(c.hibernated, 0),
		COALESCE(r.bindings, 0), COALESCE(r.snapshots, 0), COALESCE(r.statepoints, 0)
	FROM runtime_instances i JOIN runtime_bundles b ON b.id = i.runtime_bundle_id
	LEFT JOIN current_bindings c ON c.runtime_instance_id = i.id
	LEFT JOIN bundle_refs r ON r.bundle_id = b.id
	LEFT JOIN runtime_bundle_security s ON s.bundle_id = b.id
	ORDER BY i.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	profiles := []RuntimeProfile{}
	for rows.Next() {
		var profile RuntimeProfile
		var bootProfiles string
		if err = rows.Scan(&profile.RuntimeInstanceID, &profile.WorkerID, &profile.LocalInstanceID, &profile.RuntimeBundleID, &profile.StateReaderID,
			&profile.Status, &profile.VerificationStatus, &profile.SecurityStatus, &profile.SecurityReason, &bootProfiles,
			&profile.RunningBindings, &profile.StandbyBindings, &profile.HibernatedBindings,
			&profile.BundleBindingRefs, &profile.BundleSnapshotRefs, &profile.BundleStatePointRefs); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(bootProfiles), &profile.BootProfiles); err != nil {
			return nil, fmt.Errorf("decode runtime bundle %s boot profiles: %w", profile.RuntimeBundleID, err)
		}
		profile.Capabilities = rt.FullStateCapabilities(false)
		profiles = append(profiles, profile)
	}
	return profiles, rows.Err()
}
