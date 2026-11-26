package nodegateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"

	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/sys/unix"

	"sandbox-platform/internal/nodeproto"
	rt "sandbox-platform/internal/runtime"
)

// Journal fences duplicate sends across process restarts. Local execution state
// is still owned by Serve and resourced.
type Journal struct {
	db   *sql.DB
	lock *os.File
}

const journalSchema = `
CREATE TABLE IF NOT EXISTS authority (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS commands (
	operation_id TEXT NOT NULL,
	step TEXT NOT NULL,
	scope TEXT NOT NULL,
	digest TEXT NOT NULL,
	initial_receipt TEXT,
	receipt TEXT,
	revision INTEGER NOT NULL DEFAULT 0,
	refusal TEXT,
	PRIMARY KEY (operation_id, step)
);
CREATE TABLE IF NOT EXISTS volume_commands (
	operation_id TEXT PRIMARY KEY,
	scope TEXT NOT NULL,
	digest TEXT NOT NULL,
	receipt TEXT,
	refusal TEXT
);
CREATE TABLE IF NOT EXISTS entrance (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	mode TEXT NOT NULL CHECK (mode IN ('active','cordoned','draining')),
	revision INTEGER NOT NULL CHECK (revision > 0)
);
INSERT OR IGNORE INTO entrance (id, mode, revision) VALUES (1, 'active', 1);
`

func OpenJournal(stateDir string, authority nodeproto.Authority) (*Journal, error) {
	return openJournal(stateDir, authority, false)
}

// RebindJournal compares the locally supplied prior authority while the daemon
// is stopped. Rotation preserves the gate. Recovery accounts for a changed
// platform installation or control epoch and enters Drain with the journal intact.
func RebindJournal(stateDir string, previous, next nodeproto.Authority) error {
	if !previous.Valid() || !next.Valid() || next.WorkerID != previous.WorkerID ||
		next.EnrollmentGeneration <= previous.EnrollmentGeneration {
		return errors.New("rebind requires a newer enrollment generation for the same Worker")
	}
	recovering := next.ControllerID != previous.ControllerID || next.ControlEpoch != previous.ControlEpoch
	if !recovering && next.EnrollmentGeneration != previous.EnrollmentGeneration+1 {
		return errors.New("certificate rotation must advance enrollment by exactly one generation")
	}
	j, err := openJournal(stateDir, previous, true)
	if err != nil {
		return err
	}
	defer j.Close()
	tx, err := j.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE authority SET value=? WHERE id=1", string(raw)); err != nil {
		return err
	}
	if recovering {
		if _, err = tx.Exec("UPDATE entrance SET mode='draining', revision=revision+1 WHERE id=1"); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func openJournal(stateDir string, authority nodeproto.Authority, existing bool) (_ *Journal, err error) {
	if !authority.Valid() || !filepath.IsAbs(stateDir) || filepath.Clean(stateDir) != stateDir || stateDir == "/" {
		return nil, errors.New("journal requires a valid authority and a private absolute state directory")
	}
	if !existing {
		if err := os.MkdirAll(stateDir, 0700); err != nil {
			return nil, err
		}
	}
	dir, err := os.Lstat(stateDir)
	if err != nil {
		return nil, err
	}
	if !dir.IsDir() || dir.Mode().Perm()&0077 != 0 {
		return nil, errors.New("journal state directory must be owned and accessible only by the Node account")
	}
	fd, err := unix.Open(filepath.Join(stateDir, "node.lock"), unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	lock := os.NewFile(uintptr(fd), "node.lock")
	j := &Journal{lock: lock}
	defer func() {
		if err != nil {
			j.Close()
		}
	}()
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, fmt.Errorf("lock Node journal: %w", err)
	}
	name := filepath.Join(stateDir, "journal.db")
	flags := unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW
	mode := "rw"
	if !existing {
		flags |= unix.O_CREAT
	}
	dbfd, err := unix.Open(name, flags, 0600)
	if err != nil {
		return nil, err
	}
	if err := unix.Close(dbfd); err != nil {
		return nil, err
	}
	dsn := url.URL{Scheme: "file", Path: name, RawQuery: "mode=" + mode + "&_busy_timeout=3000&_journal_mode=WAL&_synchronous=FULL&_txlock=immediate"}
	j.db, err = sql.Open("sqlite3", dsn.String())
	if err != nil {
		return nil, err
	}
	j.db.SetMaxOpenConns(1)
	if _, err := j.db.Exec(journalSchema); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(authority)
	if err != nil {
		return nil, err
	}
	if !existing {
		if _, err := j.db.Exec("INSERT OR IGNORE INTO authority(id,value) VALUES(1,?)", string(raw)); err != nil {
			return nil, err
		}
	}
	var saved string
	if err := j.db.QueryRow("SELECT value FROM authority WHERE id=1").Scan(&saved); err != nil {
		return nil, err
	}
	if saved != string(raw) {
		return nil, errors.New("Node journal authority differs from configuration")
	}
	return j, nil
}

func (j *Journal) Close() error {
	var err error
	if j.db != nil {
		err = j.db.Close()
	}
	return errors.Join(err, j.lock.Close())
}

type commandScope struct {
	Route       nodeproto.Route
	InstanceID  string
	Incarnation *string
	Epoch       int64
	Kind        string
	Native      string
}

func (s commandScope) verifies(id string, receipt rt.OperationReceipt) bool {
	if receipt.OperationID != id || receipt.InstanceID != s.InstanceID || receipt.ExpectedExecutionEpoch != s.Epoch ||
		receipt.Kind != s.Kind || receipt.RequestDigest != s.Native ||
		receipt.BundleDigest != s.Route.BundleDigest || receipt.RouteID != "deep" || receipt.Revision < 1 {
		return false
	}
	if s.Incarnation == nil || receipt.ExpectedIncarnationID == nil {
		return s.Incarnation == nil && receipt.ExpectedIncarnationID == nil
	}
	return *s.Incarnation == *receipt.ExpectedIncarnationID
}

type journalEntry struct {
	Scope   commandScope
	Digest  string
	Receipt *rt.OperationReceipt
	Refusal string
}

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func journalLookup(ctx context.Context, q queryer, id, step string) (entry journalEntry, err error) {
	var scope string
	var receipt, refusal sql.NullString
	err = q.QueryRowContext(ctx, "SELECT scope,digest,COALESCE(initial_receipt,receipt),refusal FROM commands WHERE operation_id=? AND step=?",
		id, step).Scan(&scope, &entry.Digest, &receipt, &refusal)
	entry.Refusal = refusal.String
	if err != nil {
		return entry, err
	}
	if err = json.Unmarshal([]byte(scope), &entry.Scope); err != nil {
		return entry, err
	}
	if receipt.Valid {
		entry.Receipt = new(rt.OperationReceipt)
		err = json.Unmarshal([]byte(receipt.String), entry.Receipt)
	}
	return entry, err
}

const submitStep = "submit"

func (j *Journal) begin(ctx context.Context, id, step string, scope commandScope, digest string, requiredActive, blockedByDrain bool) (entry journalEntry, send bool, err error) {
	raw, err := json.Marshal(scope)
	if err != nil {
		return entry, false, err
	}
	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return entry, false, err
	}
	defer tx.Rollback()
	var volume bool
	if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM volume_commands WHERE operation_id=?)", id).Scan(&volume); err != nil {
		return entry, false, err
	}
	if volume {
		return entry, false, &nodeproto.Failure{Code: nodeproto.CodeConflict}
	}
	current, err := journalLookup(ctx, tx, id, step)
	if err == nil {
		currentScope, err := json.Marshal(current.Scope)
		if err != nil {
			return entry, false, err
		}
		if string(currentScope) != string(raw) || current.Digest != digest {
			return entry, false, &nodeproto.Failure{Code: nodeproto.CodeConflict}
		}
		return current, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return entry, false, err
	}
	if err := gateAllows(ctx, tx, requiredActive, blockedByDrain); err != nil {
		return entry, false, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO commands(operation_id,step,scope,digest) VALUES(?,?,?,?)", id, step, string(raw), digest); err != nil {
		return entry, false, err
	}
	return journalEntry{Scope: scope, Digest: digest}, true, tx.Commit()
}

func (j *Journal) remember(ctx context.Context, id, step string, receipt rt.OperationReceipt) error {
	raw, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	_, err = j.db.ExecContext(ctx, `UPDATE commands SET initial_receipt=COALESCE(initial_receipt,?),
		receipt=?,revision=?,refusal=NULL WHERE operation_id=? AND step=? AND revision < ?`, string(raw), string(raw), receipt.Revision, id, step, receipt.Revision)
	return err
}

func (j *Journal) refuse(ctx context.Context, id, step, code string) error {
	result, err := j.db.ExecContext(ctx, `UPDATE commands SET refusal=? WHERE operation_id=? AND step=? AND revision=0`, code, id, step)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return rt.ErrConflict
	}
	return nil
}

func (j *Journal) operationIDs(ctx context.Context, route nodeproto.Route, after string, limit int) ([]string, error) {
	query := `SELECT operation_id FROM commands WHERE step='submit' AND operation_id>? AND
		json_extract(scope,'$.Route.runtime_instance_id')=? AND json_extract(scope,'$.Route.local_instance_id')=?
		AND json_extract(scope,'$.Route.bundle_digest')=?
		AND refusal IS NULL AND (receipt IS NULL OR json_extract(receipt,'$.phase') NOT IN ('COMPLETED','ABORTED'))
		UNION ALL
		SELECT operation_id FROM volume_commands WHERE operation_id>? AND receipt IS NULL AND refusal IS NULL
		AND json_extract(scope,'$.runtime_instance_id')=? AND json_extract(scope,'$.local_instance_id')=?
		AND json_extract(scope,'$.bundle_digest')=?
		ORDER BY operation_id LIMIT ?`
	rows, err := j.db.QueryContext(ctx, query, after, route.RuntimeInstanceID, route.LocalInstanceID, route.BundleDigest,
		after, route.RuntimeInstanceID, route.LocalInstanceID, route.BundleDigest, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func readGate(ctx context.Context, q queryer) (gate nodeproto.Gate, err error) {
	gate.Coverage = nodeproto.KnownOnly
	err = q.QueryRowContext(ctx, "SELECT mode,revision FROM entrance WHERE id=1").Scan(&gate.Mode, &gate.Revision)
	return gate, err
}

func gateAllows(ctx context.Context, q queryer, requiredActive, blockedByDrain bool) error {
	if !requiredActive && !blockedByDrain {
		return nil
	}
	gate, err := readGate(ctx, q)
	if err != nil {
		return err
	}
	if (requiredActive && gate.Mode != nodeproto.GateActive) || (blockedByDrain && gate.Mode == nodeproto.GateDraining) {
		return &nodeproto.Failure{Code: nodeproto.CodeGateClosed}
	}
	return nil
}

func (j *Journal) setGate(ctx context.Context, request nodeproto.SetGate) (nodeproto.Gate, error) {
	if request.ExpectedRevision < 1 || request.ExpectedRevision >= math.MaxInt64 {
		return nodeproto.Gate{}, rt.ErrInvalid
	}
	switch request.Mode {
	case nodeproto.GateActive, nodeproto.GateCordoned, nodeproto.GateDraining:
	default:
		return nodeproto.Gate{}, rt.ErrInvalid
	}
	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return nodeproto.Gate{}, err
	}
	defer tx.Rollback()
	gate, err := readGate(ctx, tx)
	if err != nil {
		return nodeproto.Gate{}, err
	}
	if gate.Revision != request.ExpectedRevision {
		return nodeproto.Gate{}, rt.ErrConflict
	}
	if gate.Mode == request.Mode {
		return gate, nil
	}
	if _, err := tx.ExecContext(ctx, "UPDATE entrance SET mode=?,revision=revision+1 WHERE id=1", request.Mode); err != nil {
		return nodeproto.Gate{}, err
	}
	gate.Mode, gate.Revision = request.Mode, gate.Revision+1
	return gate, tx.Commit()
}
