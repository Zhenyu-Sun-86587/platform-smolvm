package snapshots

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sandbox-platform/internal/config"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"time"
)

type Catalog struct {
	DB                 *registry.Store
	Root, Stage, KeyID string
	Key                ed25519.PrivateKey
	Trust              map[string]ed25519.PublicKey
}
type CaptureMode string

const (
	CaptureManagedTasks CaptureMode = "managed_tasks"
	CaptureVM           CaptureMode = "vm"
)

type Snapshot struct {
	ID          string          `json:"snapshot_id"`
	MachineID   string          `json:"source_machine_id,omitempty"`
	Generation  int64           `json:"source_generation,omitempty"`
	Purpose     string          `json:"purpose"`
	Content     string          `json:"task_content"`
	State       string          `json:"state"`
	Hash        string          `json:"sha256,omitempty"`
	Size        int64           `json:"size_bytes"`
	Provenance  string          `json:"provenance"`
	Revision    int64           `json:"revision"`
	Label       string          `json:"label"`
	Manifest    json.RawMessage `json:"manifest,omitempty"`
	Created     time.Time       `json:"created_at"`
	Closure     *time.Time      `json:"closure_verified_at"`
	Restored    *time.Time      `json:"restore_verified_at"`
	CaptureMode CaptureMode     `json:"capture_mode"`
	VM          *VMProvenance   `json:"vm_provenance,omitempty"`
}

type VMProvenance struct {
	OperationID       string `json:"operation_id"`
	RuntimeInstanceID string `json:"runtime_instance_id"`
	RuntimeBundleID   string `json:"runtime_bundle_id"`
	StateReaderID     string `json:"state_reader_id"`
	RuntimeCaptureID  string `json:"runtime_capture_id"`
}

func Open(db *registry.Store, c config.Config) (*Catalog, error) {
	catalog := &Catalog{DB: db, Root: filepath.Join(c.StateDir, "artifacts"), Stage: filepath.Join(c.StateDir, "staging"), KeyID: c.Storage.KeyID, Trust: map[string]ed25519.PublicKey{}}
	for _, d := range []string{catalog.Root, catalog.Stage, filepath.Dir(c.Storage.KeyFile)} {
		if err := os.MkdirAll(d, 0700); err != nil {
			return nil, err
		}
	}
	raw, err := os.ReadFile(c.Storage.KeyFile)
	if errors.Is(err, os.ErrNotExist) {
		_, key, e := ed25519.GenerateKey(rand.Reader)
		if e != nil {
			return nil, e
		}
		der, e := x509.MarshalPKCS8PrivateKey(key)
		if e != nil {
			return nil, e
		}
		raw = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
		_, _, e = writeFile(filepath.Dir(c.Storage.KeyFile), filepath.Base(c.Storage.KeyFile), bytes.NewReader(raw), 8192)
		if e != nil {
			return nil, e
		}
		if err = syncDir(filepath.Dir(c.Storage.KeyFile)); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	key, err := readPrivateSigner(c.Storage.KeyFile, raw)
	if err != nil {
		return nil, err
	}
	catalog.Key = key
	catalog.Trust, err = readTrust(c, key.Public().(ed25519.PublicKey))
	if err != nil {
		return nil, err
	}
	return catalog, nil
}

func readPrivateSigner(path string, raw []byte) (ed25519.PrivateKey, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("snapshot private key must have mode 0600")
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("invalid snapshot key PEM")
	}
	priv, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := priv.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("Ed25519 signing key required")
	}
	return key, nil
}

// LoadTrust reads configured trust without creating a private signing key.
func LoadTrust(c config.Config) (map[string]ed25519.PublicKey, error) {
	raw, err := os.ReadFile(c.Storage.KeyFile)
	if errors.Is(err, os.ErrNotExist) {
		return readTrust(c, nil)
	}
	if err != nil {
		return nil, err
	}
	key, err := readPrivateSigner(c.Storage.KeyFile, raw)
	if err != nil {
		return nil, err
	}
	return readTrust(c, key.Public().(ed25519.PublicKey))
}

func LoadRecoveryTrust(c config.Config, signer []byte) (map[string]ed25519.PublicKey, error) {
	if len(signer) == 0 {
		return LoadTrust(c)
	}
	block, _ := pem.Decode(signer)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, errors.New("invalid recovered snapshot signer")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	local, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("recovered signer must be an Ed25519 private key")
	}
	return readTrust(c, local.Public().(ed25519.PublicKey))
}

func readTrust(c config.Config, local ed25519.PublicKey) (map[string]ed25519.PublicKey, error) {
	trust := make(map[string]ed25519.PublicKey)
	if local != nil {
		trust[c.Storage.KeyID] = local
	}
	for _, s := range c.Storage.Trusted {
		b, err := os.ReadFile(s.File)
		if err != nil {
			return nil, err
		}
		p, _ := pem.Decode(b)
		if p == nil {
			return nil, errors.New("invalid signer PEM")
		}
		k, err := x509.ParsePKIXPublicKey(p.Bytes)
		if err != nil {
			return nil, err
		}
		pub, ok := k.(ed25519.PublicKey)
		if !ok {
			return nil, errors.New("invalid signer type")
		}
		if prev, exists := trust[s.ID]; exists && !prev.Equal(pub) {
			return nil, errors.New("conflicting trusted signer")
		}
		trust[s.ID] = pub
	}
	return trust, nil
}

const columns = `id,COALESCE(source_machine_id,''),COALESCE(source_generation,0),purpose,task_content,state,COALESCE(sha256,''),COALESCE(size_bytes,0),provenance,revision,label,COALESCE(manifest_json,'null'),created_at,closure_verified_at,restore_verified_at,
	COALESCE(v.operation_id,''),COALESCE(v.runtime_instance_id,''),COALESCE(v.runtime_bundle_id,''),
	COALESCE(v.state_reader_id,''),COALESCE(v.runtime_capture_id,'')`
const fromSnapshots = ` FROM snapshots LEFT JOIN vm_snapshot_provenance v ON v.snapshot_id=snapshots.id`

type scanner interface{ Scan(...any) error }

func scan(row scanner) (Snapshot, error) {
	var s Snapshot
	var raw string
	var created int64
	var closure, restored sql.NullInt64
	var vm VMProvenance
	err := row.Scan(&s.ID, &s.MachineID, &s.Generation, &s.Purpose, &s.Content, &s.State, &s.Hash, &s.Size, &s.Provenance, &s.Revision, &s.Label, &raw, &created, &closure, &restored,
		&vm.OperationID, &vm.RuntimeInstanceID, &vm.RuntimeBundleID, &vm.StateReaderID, &vm.RuntimeCaptureID)
	s.CaptureMode = CaptureManagedTasks
	if vm.OperationID != "" {
		s.CaptureMode, s.VM = CaptureVM, &vm
	}
	s.Manifest = []byte(raw)
	s.Created = time.UnixMilli(created).UTC()
	if closure.Valid {
		t := time.UnixMilli(closure.Int64).UTC()
		s.Closure = &t
	}
	if restored.Valid {
		t := time.UnixMilli(restored.Int64).UTC()
		s.Restored = &t
	}
	return s, err
}
func (c *Catalog) Get(ctx context.Context, id string) (Snapshot, error) {
	return scan(c.DB.ReadDB().QueryRowContext(ctx, "SELECT "+columns+fromSnapshots+" WHERE id=?", id))
}
func (c *Catalog) List(ctx context.Context, mid string) ([]Snapshot, error) {
	rows, err := c.DB.ReadDB().QueryContext(ctx, "SELECT "+columns+fromSnapshots+" WHERE (source_machine_id=? OR ?='') AND state!='deleted' ORDER BY created_at DESC,id", mid, mid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Snapshot{}
	for rows.Next() {
		s, e := scan(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
func (c *Catalog) Reserve(ctx context.Context, id, mid, purpose, label string, gen int64) (string, error) {
	var machine any
	if mid != "" {
		machine = mid
	}
	if _, e := c.DB.DB().ExecContext(ctx, `INSERT INTO snapshots(id,source_machine_id,source_generation,purpose,label,task_content,state,relative_path,provenance,created_at) VALUES(?,?,?,?,?,'unknown','staging',?,'untrusted',?)`, id, machine, gen, purpose, label, id, time.Now().UnixMilli()); e != nil {
		return "", e
	}
	dir := filepath.Join(c.Stage, id)
	return dir, os.Mkdir(dir, 0700)
}
func (c *Catalog) Publish(ctx context.Context, id string, a Artifact, local bool) error {
	stage := filepath.Join(c.Stage, id)
	if err := os.Rename(stage, filepath.Join(c.Root, id)); err != nil {
		return err
	}
	if err := syncDir(c.Root); err != nil {
		return err
	}
	if err := syncDir(c.Stage); err != nil {
		return err
	}
	tx, err := c.DB.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	state, source, content := "quarantined", "untrusted", "unknown"
	if !a.Trusted {
		var n int
		err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM snapshots WHERE sha256=? AND manifest_json=? AND provenance='local_record' AND state='ready'", a.Manifest.Digest, string(a.Raw)).Scan(&n)
		if err != nil {
			return err
		}
		a.Trusted = n > 0
		if a.Trusted {
			source = "local_record"
		}
	}
	if a.Trusted {
		state, content = "ready", a.Manifest.TaskContent
		if local {
			source = "local_record"
		} else if source != "local_record" {
			source = "trusted_signature"
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE snapshots SET purpose=?,task_content=?,state=?,sha256=?,size_bytes=?,manifest_json=?,provenance=?,signer_key_id=?,capture_receipt_json=?,closure_verified_at=?,revision=revision+1 WHERE id=? AND state='staging'`, a.Manifest.Purpose, content, state, a.Manifest.Digest, a.Manifest.Size, string(a.Raw), source, a.KeyID, string(a.Manifest.CaptureReceipt), time.Now().UnixMilli(), id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		return registry.ErrConflict
	}
	var sourceMachineID string
	if err = tx.QueryRowContext(ctx, "SELECT COALESCE(source_machine_id,'') FROM snapshots WHERE id=?", id).Scan(&sourceMachineID); err != nil {
		return err
	}
	if err = registry.AppendEvent(ctx, tx, "snapshot", sourceMachineID, "", map[string]string{"snapshot_id": id, "state": state}, true); err != nil {
		return err
	}
	return tx.Commit()
}
func (c *Catalog) Quarantine(ctx context.Context, id string) error {
	_, err := c.DB.DB().ExecContext(ctx, "UPDATE snapshots SET state='quarantined',revision=revision+1 WHERE id=? AND state='staging'", id)
	return err
}
func (c *Catalog) Pin(ctx context.Context, id, kind, owner string) error {
	return c.pin(ctx, id, kind, owner, false)
}

func (c *Catalog) PinChecksum(ctx context.Context, id, owner string) error {
	return c.pin(ctx, id, "verify", owner, true)
}

func (c *Catalog) pin(ctx context.Context, id, kind, owner string, checksum bool) error {
	res, err := c.DB.DB().ExecContext(ctx, `INSERT INTO snapshot_pins(snapshot_id,owner_kind,owner_id,created_at) SELECT id,?,?,? FROM snapshots WHERE id=? AND (state='ready' OR (? AND state='quarantined' AND sha256 IS NOT NULL)) ON CONFLICT DO NOTHING`, kind, owner, time.Now().UnixMilli(), id, checksum)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 1 {
		return nil
	}
	var one int
	err = c.DB.ReadDB().QueryRowContext(ctx, "SELECT 1 FROM snapshot_pins p JOIN snapshots s ON s.id=p.snapshot_id WHERE s.id=? AND (s.state='ready' OR (? AND s.state='quarantined' AND s.sha256 IS NOT NULL)) AND p.owner_kind=? AND p.owner_id=?", id, checksum, kind, owner).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return registry.ErrConflict
	}
	return err
}
func (c *Catalog) Unpin(ctx context.Context, id, kind, owner string) error {
	_, err := c.DB.DB().ExecContext(ctx, "DELETE FROM snapshot_pins WHERE snapshot_id=? AND owner_kind=? AND owner_id=?", id, kind, owner)
	return err
}
func (c *Catalog) Verify(ctx context.Context, s Snapshot) error {
	if s.VM != nil {
		return rt.ErrUnsupported
	}
	f, err := os.Open(filepath.Join(c.Root, s.ID, CheckpointFile))
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return err
	}
	if n != s.Size || hex.EncodeToString(h.Sum(nil)) != s.Hash {
		_, e := c.DB.DB().ExecContext(ctx, "UPDATE snapshots SET state='quarantined',closure_verified_at=NULL,revision=revision+1 WHERE id=?", s.ID)
		if e != nil {
			return e
		}
		return fmt.Errorf("checkpoint integrity check failed")
	}
	_, err = c.DB.DB().ExecContext(ctx, "UPDATE snapshots SET closure_verified_at=? WHERE id=?", time.Now().UnixMilli(), s.ID)
	return err
}
