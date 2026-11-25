package registry

import (
	"context"
	"time"
)

// ExpireDesktopLeases retires platform-owned holds even if the browser or
// platformd disappeared. A revoked credential cannot leave a desktop hold.
func (s *Store) ExpireDesktopLeases(ctx context.Context, now time.Time) error {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `UPDATE leases SET state='released',revision=revision+1
		WHERE state!='released' AND EXISTS (
			SELECT 1 FROM desktop_sessions d JOIN meta m ON m.id=1
			LEFT JOIN credentials c ON c.id=d.credential_id LEFT JOIN principals p ON p.id=c.principal_id
			LEFT JOIN services v ON v.id=d.service_id JOIN machines w ON w.id=leases.machine_id
			JOIN runtime_bindings b ON b.id=d.binding_id
			WHERE d.lease_id=leases.id AND (leases.expires_at<=? OR c.id IS NULL OR c.revoked_at IS NOT NULL OR c.expires_at<=?
			OR p.disabled=1 OR d.control_epoch!=m.control_epoch OR v.id IS NULL OR v.enabled=0 OR v.revision!=d.service_revision
			OR w.deleted_at IS NOT NULL OR w.current_binding_id IS NOT d.binding_id OR b.execution_epoch!=d.execution_epoch)
		) RETURNING id,machine_id`, now.UnixMilli(), now.UnixMilli())
	if err != nil {
		return err
	}
	defer rows.Close()
	var ended [][2]string
	for rows.Next() {
		var item [2]string
		if err := rows.Scan(&item[0], &item[1]); err != nil {
			return err
		}
		ended = append(ended, item)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range ended {
		if err := AppendEvent(ctx, tx, "lease", item[1], "", map[string]string{"lease_id": item[0], "state": "released"}, false); err != nil {
			return err
		}
	}
	return tx.Commit()
}
