package registry

import (
	"context"
	"database/sql"
	"errors"
)

var ErrOwnerUnverified = errors.New("installation owner requires verification")

// OwnerID identifies the active installation owner for internal operations.
func (s *Store) OwnerID(ctx context.Context) (string, error) {
	var id string
	err := s.ReadDB().QueryRowContext(ctx, `SELECT p.id FROM installation_owner i
		JOIN principals p ON p.id = i.principal_id WHERE i.singleton = 1 AND p.disabled = 0`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrOwnerUnverified
	}
	return id, err
}
