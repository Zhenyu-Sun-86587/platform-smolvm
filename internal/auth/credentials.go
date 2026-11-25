package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"sandbox-platform/internal/registry"
)

func (s *Service) IssueTokenTx(ctx context.Context, tx *sql.Tx, p Principal, label string, scope Scope, expiry *time.Time) (Token, string, error) {
	if err := p.Authorize(ActionTokens, ""); err != nil {
		return Token{}, "", err
	}
	if !validScope(scope) || len(label) > 120 || (expiry != nil && !expiry.After(time.Now())) {
		return Token{}, "", registry.ErrInvalid
	}
	if scope.MachineID != "" {
		var id string
		if err := tx.QueryRowContext(ctx, "SELECT id FROM machines WHERE id=? AND deleted_at IS NULL", scope.MachineID).Scan(&id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				err = registry.ErrNotFound
			}
			return Token{}, "", err
		}
	}
	return issue(ctx, tx, p, label, scope, expiry, kindAPIToken)
}
func issue(ctx context.Context, tx *sql.Tx, p Principal, label string, scope Scope, expiry *time.Time, kind string) (Token, string, error) {
	var t Token
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return t, "", err
	}
	id, err := registry.NewID("cred_")
	if err != nil {
		return t, "", err
	}
	secret := base64.RawURLEncoding.EncodeToString(random[:])
	t = Token{ID: id, Label: label, Scope: scope, ExpiresAt: expiry, CreatedAt: time.Now().UTC()}
	var exp any
	if expiry != nil {
		exp = expiry.UnixMilli()
	}
	raw, err := json.Marshal(scope)
	if err != nil {
		return t, "", err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO credentials(id,principal_id,kind,digest,label,scope_json,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?)`, id, p.ID, kind, credentialDigest(secret), label, string(raw), t.CreatedAt.UnixMilli(), exp)
	if err == nil {
		err = registry.AppendEvent(ctx, tx, "auth", "", "", map[string]string{"action": "credential_issued", "credential_id": id}, true)
	}
	return t, secret, err
}
func (s *Service) Session(ctx context.Context, p Principal) (string, error) {
	tx, err := s.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	exp := time.Now().Add(12 * time.Hour)
	_, secret, err := issue(ctx, tx, p, "browser", p.Scope, &exp, kindWebSession)
	if err != nil {
		return "", err
	}
	return secret, tx.Commit()
}
func (s *Service) RevokeTx(ctx context.Context, tx *sql.Tx, p Principal, id, kind string) error {
	if kind == kindWebSession {
		if id != p.CredentialID {
			return ErrForbidden
		}
	} else if err := p.Authorize(ActionTokens, ""); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, "UPDATE credentials SET revoked_at=COALESCE(revoked_at,?) WHERE id=? AND principal_id=? AND kind=?", time.Now().UnixMilli(), id, p.ID, kind)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return registry.ErrNotFound
	}
	return registry.AppendEvent(ctx, tx, "auth", "", "", map[string]string{"action": "credential_revoked", "credential_id": id}, true)
}
func (s *Service) ListTokens(ctx context.Context, p Principal) ([]Token, error) {
	if err := p.Authorize(ActionTokens, ""); err != nil {
		return nil, err
	}
	rows, err := s.store.ReadDB().QueryContext(ctx, "SELECT id,label,scope_json,created_at,expires_at,revoked_at FROM credentials WHERE principal_id=? AND kind='api_token' ORDER BY id", p.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Token{}
	for rows.Next() {
		var t Token
		var raw string
		var at int64
		var exp, rev sql.NullInt64
		if err = rows.Scan(&t.ID, &t.Label, &raw, &at, &exp, &rev); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &t.Scope); err != nil {
			return nil, err
		}
		t.CreatedAt = time.UnixMilli(at).UTC()
		if exp.Valid {
			x := time.UnixMilli(exp.Int64).UTC()
			t.ExpiresAt = &x
		}
		if rev.Valid {
			x := time.UnixMilli(rev.Int64).UTC()
			t.RevokedAt = &x
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
