package platformapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"time"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
)

type Lease struct {
	ID          string    `json:"lease_id"`
	MachineID   string    `json:"machine_id"`
	PrincipalID string    `json:"principal_id"`
	Generation  int64     `json:"binding_generation"`
	State       string    `json:"state"`
	Revision    int64     `json:"revision"`
	Reason      string    `json:"reason"`
	Expires     time.Time `json:"expires_at"`
	Token       string    `json:"lease_token,omitempty"`
}

const leaseCols = "id,machine_id,principal_id,binding_generation,state,revision,reason,expires_at"

func scanLease(r row) (Lease, error) {
	var l Lease
	var at int64
	err := r.Scan(&l.ID, &l.MachineID, &l.PrincipalID, &l.Generation, &l.State, &l.Revision, &l.Reason, &at)
	l.Expires = time.UnixMilli(at).UTC()
	return l, err
}
func (s *Server) leaseRoutes() {
	s.route("GET /v1/machines/{machine}/leases", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		id := r.PathValue("machine")
		if err := p.Authorize("read", id); err != nil {
			return err
		}
		rows, err := s.DB.ReadDB().QueryContext(r.Context(), "SELECT "+leaseCols+" FROM leases WHERE machine_id=? ORDER BY id", id)
		if err != nil {
			return err
		}
		defer rows.Close()
		out := []Lease{}
		for rows.Next() {
			v, err := scanLease(rows)
			if err != nil {
				return err
			}
			out = append(out, v)
		}
		if err = rows.Err(); err != nil {
			return err
		}
		return list(w, r, out, func(l Lease) string { return l.ID })
	})
	for _, path := range []string{"POST /v1/machines/{machine}/leases", "PATCH /v1/leases/{lease}", "DELETE /v1/leases/{lease}"} {
		s.route(path, func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
			var v struct {
				Reason string `json:"reason,omitempty"`
				TTL    int64  `json:"ttl_seconds,omitempty"`
				Token  string `json:"lease_token,omitempty"`
			}
			if err := decode(r, &v); err != nil {
				return err
			}
			if r.Method != "DELETE" && (v.TTL <= 0 || v.TTL > 365*86400) {
				return registry.ErrInvalid
			}
			mid := r.PathValue("machine")
			id := r.PathValue("lease")
			if id != "" {
				if err := s.DB.ReadDB().QueryRowContext(r.Context(), "SELECT machine_id FROM leases WHERE id=?", id).Scan(&mid); err != nil {
					return err
				}
			}
			if err := p.Authorize("manage", mid); err != nil {
				return err
			}
			rev, err := revision(r)
			if err != nil {
				return err
			}
			return s.mutate(w, r, p, v, func(ctx context.Context, tx *sql.Tx) (registry.MutationResult, error) {
				var l Lease
				var token string
				if id == "" {
					if len(v.Reason) > 512 || v.Reason == "" {
						return registry.MutationResult{}, registry.ErrInvalid
					}
					l.ID, err = registry.NewID("lease_")
					if err != nil {
						return registry.MutationResult{}, err
					}
					var mrev int64
					var lock int
					if err = tx.QueryRowContext(ctx, "SELECT binding_generation,revision,(SELECT COUNT(*) FROM operation_locks WHERE machine_id=machines.id) FROM machines WHERE id=? AND deleted_at IS NULL", mid).Scan(&l.Generation, &mrev, &lock); err != nil {
						return registry.MutationResult{}, err
					}
					if rev != mrev {
						return registry.MutationResult{}, registry.ErrRevision
					}
					if l.Generation == 0 || lock != 0 {
						return registry.MutationResult{}, registry.ErrOperationActive
					}
					var random [32]byte
					if _, err = rand.Read(random[:]); err != nil {
						return registry.MutationResult{}, err
					}
					token = base64.RawURLEncoding.EncodeToString(random[:])
					hash := sha256.Sum256([]byte(token))
					l.MachineID = mid
					l.PrincipalID = p.ID
					l.State = "active"
					l.Revision = 1
					l.Reason = v.Reason
					l.Expires = time.Now().UTC().Add(time.Duration(v.TTL) * time.Second)
					_, err = tx.ExecContext(ctx, "INSERT INTO leases(id,machine_id,binding_generation,principal_id,token_hash,reason,expires_at,state) VALUES(?,?,?,?,?,?,?,'active')", l.ID, mid, l.Generation, p.ID, hex.EncodeToString(hash[:]), v.Reason, l.Expires.UnixMilli())
				} else {
					l, err = scanLease(tx.QueryRowContext(ctx, "SELECT "+leaseCols+" FROM leases WHERE id=?", id))
					if err != nil {
						return registry.MutationResult{}, err
					}
					if l.Revision != rev {
						return registry.MutationResult{}, registry.ErrRevision
					}
					var hash string
					if err = tx.QueryRowContext(ctx, "SELECT token_hash FROM leases WHERE id=?", id).Scan(&hash); err != nil {
						return registry.MutationResult{}, err
					}
					input := sha256.Sum256([]byte(v.Token))
					if l.PrincipalID != p.ID || subtle.ConstantTimeCompare([]byte(hex.EncodeToString(input[:])), []byte(hash)) != 1 {
						return registry.MutationResult{}, auth.ErrForbidden
					}
					if l.State == "released" {
						return registry.MutationResult{}, registry.ErrConflict
					}
					if r.Method == "DELETE" {
						l.State = "released"
					} else {
						l.State = "active"
						l.Expires = time.Now().UTC().Add(time.Duration(v.TTL) * time.Second)
					}
					l.Revision++
					_, err = tx.ExecContext(ctx, "UPDATE leases SET state=?,revision=?,expires_at=? WHERE id=?", l.State, l.Revision, l.Expires.UnixMilli(), id)
				}
				if err != nil {
					return registry.MutationResult{}, err
				}
				if err = registry.AppendEvent(ctx, tx, "lease", mid, "", map[string]string{"lease_id": l.ID, "state": l.State}, false); err != nil {
					return registry.MutationResult{}, err
				}
				redacted := lifecycle.JSON(l)
				l.Token = token
				return registry.MutationResult{Response: lifecycle.JSON(l), RetryResponse: redacted}, nil
			})
		})
	}
}
