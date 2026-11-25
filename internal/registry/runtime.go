package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	rt "sandbox-platform/internal/runtime"
)

// RegisterRuntime records the immutable endpoint identity and bundle pin.
func (s *Store) RegisterRuntime(ctx context.Context, route rt.BundleRoute, endpoint string, manifest json.RawMessage) error {
	if route.InstanceID == "" || route.BundleID == "" || endpoint == "" || !json.Valid(manifest) {
		return fmt.Errorf("%w: runtime instance, bundle, endpoint and manifest are required", ErrInvalid)
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO runtime_bundles(id, manifest_digest, manifest_json, verification_status, security_status, created_at)
		VALUES (?, ?, ?, 'manifest_pinned', 'unknown', ?) ON CONFLICT (id) DO NOTHING`,
		route.BundleID, route.BundleID, string(manifest), time.Now().UnixMilli()); err != nil {
		return uniqueConflict(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO runtime_instances(id, lane, runtime_bundle_id, endpoint_key, status, revision)
		VALUES (?, 'deep', ?, ?, 'registered', 1) ON CONFLICT (id) DO NOTHING`, route.InstanceID, route.BundleID, endpoint); err != nil {
		return uniqueConflict(err)
	}
	var bundle, existingEndpoint string
	if err := tx.QueryRowContext(ctx, `SELECT runtime_bundle_id, endpoint_key FROM runtime_instances WHERE id = ?`, route.InstanceID).
		Scan(&bundle, &existingEndpoint); err != nil {
		return err
	}
	if bundle != route.BundleID || existingEndpoint != endpoint {
		return fmt.Errorf("%w: runtime instance %q already has another endpoint or bundle", ErrConflict, route.InstanceID)
	}
	return tx.Commit()
}

func loadRuntimeRoute(ctx context.Context, tx *sql.Tx, instanceID string) (rt.BundleRoute, error) {
	route := rt.BundleRoute{InstanceID: instanceID}
	err := tx.QueryRowContext(ctx, `SELECT runtime_bundle_id FROM runtime_instances WHERE id = ?`, instanceID).Scan(&route.BundleID)
	if err == sql.ErrNoRows {
		return rt.BundleRoute{}, &rt.Error{Code: "NOT_SUPPORTED", Message: "runtime instance is not registered"}
	}
	return route, err
}
