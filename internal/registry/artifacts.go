package registry

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// RecordArtifactPlacement pins the local generation once. The import receipt
// must have been checked against the exact registered runtime before this call.
func (s *Store) RecordArtifactPlacement(ctx context.Context, worker, instance, bundle, digest, generation string, size int64) error {
	r, err := s.writer.ExecContext(ctx, `INSERT INTO artifact_placements(runtime_instance_id,content_digest,local_generation_id,capacity_bytes,verified_at_ms)
        SELECT id,?,?,?,? FROM runtime_instances WHERE id=? AND worker_id=? AND runtime_bundle_id=?
        ON CONFLICT(runtime_instance_id,content_digest) DO UPDATE SET verified_at_ms=excluded.verified_at_ms
        WHERE local_generation_id=excluded.local_generation_id AND capacity_bytes=excluded.capacity_bytes`, digest, generation, size, time.Now().UnixMilli(), instance, worker, bundle)
	if err != nil {
		return uniqueConflict(err)
	}
	n, err := r.RowsAffected()
	if err == nil && n != 1 {
		return fmt.Errorf("%w: template placement differs from the pinned import", ErrConflict)
	}
	return err
}

func (s *Store) TemplateGeneration(ctx context.Context, b Binding, digest string) (string, error) {
	var local string
	err := s.reader.QueryRowContext(ctx, `SELECT a.local_generation_id FROM artifact_placements a JOIN runtime_instances i ON i.id=a.runtime_instance_id
        WHERE i.worker_id=? AND i.id=? AND i.runtime_bundle_id=? AND a.content_digest=?`, b.WorkerID, b.RuntimeInstanceID, b.RuntimeBundleID, digest).Scan(&local)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("%w: template root is unavailable on its owner", ErrConflict)
	}
	return local, err
}
