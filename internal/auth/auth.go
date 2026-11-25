package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"

	"sandbox-platform/internal/registry"
)

var (
	ErrUnauthenticated = errors.New("authentication required")
	ErrForbidden       = errors.New("permission denied")
)

const (
	AccessRead     = "read"
	AccessManage   = "manage"
	AccessTunnel   = "tunnel"
	ActionRead     = "read"
	ActionManage   = "manage"
	ActionTunnel   = "tunnel"
	ActionTokens   = "tokens"
	ActionAdmin    = "admin"
	kindAPIToken   = "api_token"
	kindWebSession = "web_session"
)

type Scope struct {
	Access    string `json:"access"`
	MachineID string `json:"machine_id,omitempty"`
}

type Principal struct {
	ID           string `json:"principal_id"`
	Username     string `json:"username"`
	CredentialID string `json:"-"`
	Scope        Scope  `json:"scope"`
}

func (p Principal) Allows(action, machineID string) bool {
	if p.ID == "" || p.Scope.MachineID != "" && p.Scope.MachineID != machineID {
		return false
	}
	switch p.Scope.Access {
	case AccessManage:
		return action == ActionRead || action == ActionManage || action == ActionTunnel ||
			(p.Scope.MachineID == "" && (action == ActionTokens || action == ActionAdmin))
	case AccessRead:
		return action == ActionRead
	case AccessTunnel:
		return p.Scope.MachineID != "" && action == ActionTunnel
	default:
		return false
	}
}

func (p Principal) Authorize(action, machineID string) error {
	if p.ID == "" {
		return ErrUnauthenticated
	}
	if !p.Allows(action, machineID) {
		return ErrForbidden
	}
	return nil
}

type Token struct {
	ID        string     `json:"token_id"`
	Revision  int64      `json:"revision"`
	Label     string     `json:"label"`
	Scope     Scope      `json:"scope"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

type Service struct {
	store *registry.Store
}

func New(store *registry.Store) *Service { return &Service{store: store} }

const selectOwner = `SELECT p.id, p.username FROM installation_owner i
	JOIN principals p ON p.id = i.principal_id WHERE i.singleton = 1`

func (s *Service) Bootstrap(ctx context.Context, username, password string) (Principal, error) {
	if username == "" || len(username) > 128 || strings.TrimSpace(username) != username || strings.ContainsAny(username, "\x00\r\n") {
		return Principal{}, fmt.Errorf("%w: invalid owner username", registry.ErrInvalid)
	}
	if len(password) < 1 || len(password) > 1024 {
		return Principal{}, fmt.Errorf("%w: owner password is required", registry.ErrInvalid)
	}
	var existing Principal
	err := s.store.ReadDB().QueryRowContext(ctx, selectOwner).Scan(&existing.ID, &existing.Username)
	if err == nil {
		if existing.Username != username {
			return Principal{}, fmt.Errorf("%w: installation owner %q already exists", registry.ErrConflict, existing.Username)
		}
		existing.Scope = Scope{Access: AccessManage}
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Principal{}, err
	}
	passwordHash, err := hashPassword(password)
	if err != nil {
		return Principal{}, err
	}
	id, err := registry.NewID("p_")
	if err != nil {
		return Principal{}, err
	}
	tx, err := s.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return Principal{}, err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, selectOwner).Scan(&id, &existing.Username)
	switch {
	case err == nil:
		if existing.Username != username {
			return Principal{}, fmt.Errorf("%w: installation owner %q already exists", registry.ErrConflict, existing.Username)
		}
	case errors.Is(err, sql.ErrNoRows):
		var historicalOwner bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM principals WHERE password_hash IS NOT NULL)
			OR EXISTS (SELECT 1 FROM events WHERE type = 'auth' AND audit = 1
				AND json_extract(payload_json, '$.action') = 'owner_created')`).Scan(&historicalOwner); err != nil {
			return Principal{}, err
		}
		if historicalOwner {
			return Principal{}, registry.ErrOwnerUnverified
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO principals (id, username, password_hash, created_at)
			VALUES (?, ?, ?, ?)`, id, username, passwordHash, time.Now().UnixMilli()); err != nil {
			return Principal{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO installation_owner VALUES (1, ?)`, id); err != nil {
			return Principal{}, err
		}
		if err := registry.AppendEvent(ctx, tx, "auth", "", "", map[string]string{"action": "owner_created", "principal_id": id}, true); err != nil {
			return Principal{}, err
		}
	default:
		return Principal{}, err
	}
	return Principal{ID: id, Username: username, Scope: Scope{Access: AccessManage}}, tx.Commit()
}

const passwordTime, passwordMemory, passwordParallelism, passwordKeyLength = 3, 64 * 1024, 1, 32

func hashPassword(password string) (string, error) {
	var salt [16]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(password), salt[:], passwordTime, passwordMemory, passwordParallelism, passwordKeyLength)
	codec := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", passwordMemory, passwordTime, passwordParallelism,
		codec.EncodeToString(salt[:]), codec.EncodeToString(hash)), nil
}

func verifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	parameters := fmt.Sprintf("m=%d,t=%d,p=%d", passwordMemory, passwordTime, passwordParallelism)
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v=19" || parts[3] != parameters {
		return false
	}
	codec := base64.RawStdEncoding
	salt, err := codec.DecodeString(parts[4])
	if err != nil || len(salt) != 16 {
		return false
	}
	expected, err := codec.DecodeString(parts[5])
	if err != nil || len(expected) != passwordKeyLength {
		return false
	}
	hash := argon2.IDKey([]byte(password), salt, passwordTime, passwordMemory, passwordParallelism, passwordKeyLength)
	return subtle.ConstantTimeCompare(hash, expected) == 1
}

func (s *Service) Login(ctx context.Context, username, password string) (Principal, error) {
	if len(username) > 128 || len(password) > 1024 || password == "" {
		return Principal{}, ErrUnauthenticated
	}
	var p Principal
	var hash string
	var disabled bool
	err := s.store.ReadDB().QueryRowContext(ctx, `SELECT id, username, COALESCE(password_hash, ''), disabled
		FROM principals WHERE username = ?`, username).Scan(&p.ID, &p.Username, &hash, &disabled)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Principal{}, err
	}
	valid := verifyPassword(hash, password)
	if hash == "" {
		// Give unknown usernames comparable password verification cost.
		argon2.IDKey([]byte(password), make([]byte, 16), passwordTime, passwordMemory, passwordParallelism, passwordKeyLength)
	}
	if !valid || disabled {
		return Principal{}, ErrUnauthenticated
	}
	p.Scope = Scope{Access: AccessManage}
	return p, nil
}

func credentialDigest(secret string) string {
	digest := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(digest[:])
}

func (s *Service) Authenticate(ctx context.Context, secret string) (Principal, error) {
	return s.authenticate(ctx, secret, kindAPIToken)
}

func (s *Service) AuthenticateSession(ctx context.Context, secret string) (Principal, error) {
	return s.authenticate(ctx, secret, kindWebSession)
}

func (s *Service) authenticate(ctx context.Context, secret, kind string) (Principal, error) {
	if secret == "" || len(secret) > 256 || strings.HasPrefix(secret, "sbg_") {
		return Principal{}, ErrUnauthenticated
	}
	var p Principal
	var scope string
	err := s.store.ReadDB().QueryRowContext(ctx, `SELECT p.id, p.username, c.id, c.scope_json
		FROM credentials c JOIN principals p ON p.id = c.principal_id
		WHERE c.kind = ? AND c.digest = ? AND p.disabled = 0 AND c.revoked_at IS NULL
		AND (c.expires_at IS NULL OR c.expires_at > ?)`, kind, credentialDigest(secret), time.Now().UnixMilli()).
		Scan(&p.ID, &p.Username, &p.CredentialID, &scope)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, ErrUnauthenticated
	}
	if err != nil {
		return Principal{}, err
	}
	if err := json.Unmarshal([]byte(scope), &p.Scope); err != nil {
		return Principal{}, err
	}
	if !validScope(p.Scope) {
		return Principal{}, ErrUnauthenticated
	}
	return p, nil
}

func validScope(scope Scope) bool {
	switch scope.Access {
	case AccessRead, AccessManage:
		return scope.MachineID == "" || validMachineID(scope.MachineID)
	case AccessTunnel:
		return validMachineID(scope.MachineID)
	default:
		return false
	}
}

func validMachineID(id string) bool {
	if !strings.HasPrefix(id, "m_") || len(id) <= 2 || len(id) > 64 {
		return false
	}
	for _, r := range id[2:] {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '_' && r != '-' {
			return false
		}
	}
	return true
}
