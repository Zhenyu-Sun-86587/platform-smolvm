package registry

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"sandbox-platform/migrations"
)

type Store struct {
	writer *sql.DB
	reader *sql.DB
	lock   *os.File
}

// Open takes the installation's process-wide writer lock before migrating or
// serving any requests. Read transactions use their own bounded connection pool.
func Open(ctx context.Context, path string) (_ *Store, err error) {
	if path == "" {
		return nil, fmt.Errorf("%w: database path is empty", ErrInvalid)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	path = filepath.Join(dir, filepath.Base(path))
	if resolved, resolveErr := filepath.EvalSymlinks(path); resolveErr == nil {
		path = resolved
	} else if !errors.Is(resolveErr, os.ErrNotExist) {
		return nil, resolveErr
	}

	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	s := &Store{lock: lock}
	defer func() {
		if err != nil {
			_ = s.Close()
		}
	}()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("lock registry: %w", err)
	}

	markerPath := path + ".installation-id"
	marker, markerErr := os.ReadFile(markerPath)
	if markerErr != nil && !errors.Is(markerErr, os.ErrNotExist) {
		return nil, fmt.Errorf("read installation marker: %w", markerErr)
	}
	info, statErr := os.Lstat(path)
	switch {
	case statErr == nil:
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("registry database must be a regular file")
		}
		if info.Sys().(*syscall.Stat_t).Nlink != 1 {
			return nil, fmt.Errorf("registry database must have a single link")
		}
		if info.Size() == 0 && markerErr == nil {
			return nil, fmt.Errorf("registry database is empty but an installation marker exists")
		}
	case !errors.Is(statErr, os.ErrNotExist):
		return nil, statErr
	case markerErr == nil:
		return nil, fmt.Errorf("registry database is missing but an installation marker exists")
	default:
		f, createErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if createErr != nil {
			return nil, createErr
		}
		if err = f.Close(); err != nil {
			return nil, err
		}
	}

	s.writer, err = sql.Open("sqlite3", sqliteDSN(path, false))
	if err != nil {
		return nil, err
	}
	s.writer.SetMaxOpenConns(1)
	s.writer.SetMaxIdleConns(1)
	if err = s.writer.PingContext(ctx); err != nil {
		return nil, err
	}
	control, err := s.migrate(ctx, strings.TrimSpace(string(marker)), markerErr == nil)
	if err != nil {
		return nil, err
	}
	if markerErr != nil {
		if err = writeMarker(markerPath, control.InstallationID); err != nil {
			return nil, err
		}
	}

	s.reader, err = sql.Open("sqlite3", sqliteDSN(path, true))
	if err != nil {
		return nil, err
	}
	s.reader.SetMaxOpenConns(4)
	s.reader.SetMaxIdleConns(4)
	if err = s.reader.PingContext(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func sqliteDSN(path string, readOnly bool) string {
	u := url.URL{Scheme: "file", Path: path}
	q := url.Values{"_busy_timeout": {"5000"}, "_foreign_keys": {"on"}, "_synchronous": {"FULL"}}
	if readOnly {
		q.Set("mode", "ro")
		q.Set("_query_only", "on")
	} else {
		q.Set("_journal_mode", "WAL")
		q.Set("_txlock", "immediate")
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// DB returns the single writer pool for repositories which need to include
// related state in a short SQLite transaction.
func (s *Store) DB() *sql.DB { return s.writer }

// ReadDB returns the bounded read-only pool for extension repositories.
func (s *Store) ReadDB() *sql.DB { return s.reader }

func (s *Store) Close() error {
	var all []error
	if s.reader != nil {
		all = append(all, s.reader.Close())
	}
	if s.writer != nil {
		all = append(all, s.writer.Close())
	}
	if s.lock != nil {
		all = append(all, s.lock.Close())
	}
	return errors.Join(all...)
}

func (s *Store) migrate(ctx context.Context, marker string, hasMarker bool) (Control, error) {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return Control{}, err
	}
	defer tx.Rollback()

	var current int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return Control{}, err
	}
	// The official Phase 2 route uses versions 3–7 for different migrations.
	files := []struct {
		version int
		name    string
	}{
		{1, "001_registry.sql"},
		{2, "002_product.sql"},
		{1001, "1001_deep_runtime.sql"},
		{1002, "1002_deep_full_state.sql"},
		{1005, "1005_deep_authorization.sql"},
		{1006, "1006_deep_guest_grants.sql"},
		{1007, "1007_deep_volumes.sql"},
		{1008, "1008_native_fork_locks.sql"},
		{1009, "1009_deep_statepoints.sql"},
		{1010, "1010_native_fork_attempts.sql"},
		{1011, "1011_desktop_sessions.sql"},
		{1012, "1012_backup_events.sql"},
		{1013, "1013_deep_lifecycle.sql"},
		{1014, "1014_runtime_bundle_security.sql"},
		{1015, "1015_connection_activity.sql"},
		{1016, "1016_guest_delegation.sql"},
		{1017, "1017_lifecycle_binding.sql"},
		{1018, "1018_native_backup_selection.sql"},
		{1019, "1019_legacy_idle_pressure.sql"},
		{1020, "1020_multi_worker.sql"},
	}
	latest := files[len(files)-1].version
	known := current == 0
	for _, file := range files {
		known = known || current == file.version
	}
	if !known {
		return Control{}, fmt.Errorf("unsupported registry schema version %d", current)
	}
	if current == 0 && hasMarker {
		return Control{}, fmt.Errorf("registry installation marker exists but database is uninitialized")
	}
	for _, file := range files {
		version := file.version
		if version <= current {
			continue
		}
		script, err := migrations.Files.ReadFile(file.name)
		if err != nil {
			return Control{}, err
		}
		if _, err := tx.ExecContext(ctx, string(script)); err != nil {
			return Control{}, fmt.Errorf("apply %s: %w", file.name, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
			return Control{}, err
		}
	}

	if current == 0 {
		installation, err := newUUID()
		if err != nil {
			return Control{}, err
		}
		epoch, err := newUUID()
		if err != nil {
			return Control{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO meta (id, schema_version, installation_id, control_epoch)
			VALUES (1, ?, ?, ?)`, latest, installation, epoch); err != nil {
			return Control{}, err
		}
	}
	control, err := readControl(ctx, tx)
	if err != nil {
		return Control{}, err
	}
	if hasMarker && marker != control.InstallationID {
		return Control{}, fmt.Errorf("registry installation marker does not match database")
	}
	if current > 0 && current != control.SchemaVersion {
		return Control{}, fmt.Errorf("inconsistent registry schema version %d (metadata %d)", current, control.SchemaVersion)
	}
	if control.SchemaVersion < latest {
		if _, err := tx.ExecContext(ctx, "UPDATE meta SET schema_version = ? WHERE id = 1", latest); err != nil {
			return Control{}, err
		}
		control.SchemaVersion = latest
	}
	return control, tx.Commit()
}

func writeMarker(path, installationID string) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".installation-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(installationID + "\n"); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

type scanner interface {
	Scan(...any) error
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func readControl(ctx context.Context, q queryer) (Control, error) {
	var c Control
	err := q.QueryRowContext(ctx, `SELECT schema_version, installation_id, control_epoch, maintenance,
		COALESCE(recovery_reason, '') FROM meta WHERE id = 1`).Scan(&c.SchemaVersion, &c.InstallationID,
		&c.ControlEpoch, &c.Maintenance, &c.RecoveryReason)
	return c, err
}

func (s *Store) Control(ctx context.Context) (Control, error) {
	return readControl(ctx, s.reader)
}

// BeginRecovery rotates the epoch while entering maintenance. A caller must
// have isolated the local control plane before initiating a database restore.
func (s *Store) BeginRecovery(ctx context.Context, reason string) (Control, error) {
	if strings.TrimSpace(reason) == "" {
		return Control{}, fmt.Errorf("%w: recovery reason is required", ErrInvalid)
	}
	epoch, err := newUUID()
	if err != nil {
		return Control{}, err
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return Control{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "UPDATE meta SET control_epoch = ?, maintenance = 1, recovery_reason = ? WHERE id = 1", epoch, reason); err != nil {
		return Control{}, err
	}
	if err := AppendEvent(ctx, tx, "maintenance", "", "", map[string]any{"maintenance": true, "recovery_reason": reason}, true); err != nil {
		return Control{}, err
	}
	c, err := readControl(ctx, tx)
	if err != nil {
		return Control{}, err
	}
	return c, tx.Commit()
}

func newUUID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	id := hex.EncodeToString(raw[:])
	return id[:8] + "-" + id[8:12] + "-" + id[12:16] + "-" + id[16:20] + "-" + id[20:], nil
}

func NewID(prefix string) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(raw[:]), nil
}

func timePtr(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := time.UnixMilli(n.Int64).UTC()
	return &t
}

func timeValue(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixMilli()
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
