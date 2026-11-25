// Package snapshots manages platform-owned standalone checkpoint artifacts.
package snapshots

import (
	"archive/tar"
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const (
	ManifestFile   = "manifest.json"
	SignatureFile  = "manifest.sig"
	CheckpointFile = "runtime.checkpoint"
	maxMetadata    = 1 << 20
)

type Manifest struct {
	Version          int             `json:"version"`
	RuntimeVersion   string          `json:"runtime_version"`
	Architecture     string          `json:"architecture"`
	TemplateID       string          `json:"template_id"`
	TemplateDigest   string          `json:"template_digest"`
	SourceMachineID  string          `json:"source_machine_id"`
	SourceGeneration int64           `json:"source_generation"`
	OperationID      string          `json:"operation_id"`
	Purpose          string          `json:"purpose"`
	TaskContent      string          `json:"task_content"`
	GuestStatus      string          `json:"guest_management_status"`
	CaptureReceipt   json.RawMessage `json:"capture_receipt"`
	Encoding         string          `json:"encoding"`
	Dependencies     []string        `json:"external_dependencies"`
	Digest           string          `json:"sha256"`
	Size             int64           `json:"size_bytes"`
	CapturedAt       time.Time       `json:"captured_at"`
}
type Signature struct {
	Version   int    `json:"version"`
	Algorithm string `json:"algorithm"`
	KeyID     string `json:"key_id"`
	Value     string `json:"signature_base64"`
}
type Artifact struct {
	Manifest Manifest
	Raw      []byte
	KeyID    string
	Trusted  bool
	Reason   string
}

// strictJSON rejects ambiguous metadata before interpreting signed claims.
func strictJSON(raw []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var scan func(int) error
	scan = func(depth int) error {
		if depth > 64 {
			return errors.New("metadata nesting exceeds limit")
		}
		t, e := d.Token()
		if e != nil {
			return e
		}
		switch t {
		case json.Delim('{'):
			keys := map[string]bool{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return e
				}
				s, ok := k.(string)
				if !ok || keys[s] {
					return errors.New("invalid or duplicate metadata key")
				}
				keys[s] = true
				if e = scan(depth + 1); e != nil {
					return e
				}
			}
			_, e = d.Token()
		case json.Delim('['):
			for d.More() {
				if e = scan(depth + 1); e != nil {
					return e
				}
			}
			_, e = d.Token()
		}
		return e
	}
	if e := scan(0); e != nil {
		return e
	}
	if _, e := d.Token(); e != io.EOF {
		return errors.New("trailing metadata")
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(out)
}

func syncDir(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}

func writeFile(dir, name string, r io.Reader, max int64) (int64, string, error) {
	f, e := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return 0, "", e
	}
	h := sha256.New()
	n, e := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, max+1))
	if e == nil && n > max {
		e = errors.New("artifact exceeds configured limit")
	}
	if e == nil {
		e = f.Sync()
	}
	if closeErr := f.Close(); e == nil {
		e = closeErr
	}
	return n, hex.EncodeToString(h.Sum(nil)), e
}

func (m Manifest) validate() error {
	if m.Version != 1 || m.Encoding != "standalone" || m.RuntimeVersion != "1.37.1" || m.Architecture != "amd64" || len(m.Dependencies) != 0 {
		return errors.New("incompatible checkpoint profile or external dependencies")
	}
	if m.SourceMachineID == "" || m.SourceGeneration < 1 || m.OperationID == "" || m.TemplateID == "" || m.TemplateDigest == "" || m.CapturedAt.IsZero() || m.Size <= 0 || len(m.Digest) != 64 {
		return errors.New("incomplete capture metadata")
	}
	if (m.Purpose != "clone" && m.Purpose != "recovery") || (m.TaskContent != "clean" && m.TaskContent != "active" && m.TaskContent != "unknown") || (m.TaskContent == "active" && m.Purpose != "recovery") {
		return errors.New("unsupported capture classification")
	}
	if m.TaskContent != "unknown" && (m.GuestStatus != "ready" || len(m.CaptureReceipt) == 0 || bytes.Equal(m.CaptureReceipt, []byte("null"))) {
		return errors.New("missing managed capture evidence")
	}
	return nil
}

// Capture writes only into a newly allocated staging directory. Publication belongs to Catalog.
func Capture(dir string, src io.Reader, max int64, m Manifest, keyID string, key ed25519.PrivateKey) (Artifact, error) {
	var a Artifact
	if len(key) != ed25519.PrivateKeySize || keyID == "" || max <= 0 {
		return a, errors.New("invalid signer or size limit")
	}
	n, h, e := writeFile(dir, CheckpointFile, src, max)
	if e != nil {
		return a, e
	}
	m.Version = 1
	m.RuntimeVersion = "1.37.1"
	m.Encoding = "standalone"
	m.Architecture = "amd64"
	m.CapturedAt = time.Now().UTC()
	m.Size = n
	m.Digest = h
	if e = m.validate(); e != nil {
		return a, e
	}
	raw, e := json.Marshal(m)
	if e != nil {
		return a, e
	}
	sig, e := json.Marshal(Signature{1, "Ed25519", keyID, base64.StdEncoding.EncodeToString(ed25519.Sign(key, raw))})
	if e != nil {
		return a, e
	}
	if _, _, e = writeFile(dir, ManifestFile, bytes.NewReader(raw), maxMetadata); e != nil {
		return a, e
	}
	if _, _, e = writeFile(dir, SignatureFile, bytes.NewReader(sig), maxMetadata); e != nil {
		return a, e
	}
	return Artifact{Manifest: m, Raw: raw, KeyID: keyID, Trusted: true}, syncDir(dir)
}

// Import accepts a flat uncompressed tar, rejects links, enforces lengths and never starts a VM.
func Import(dir string, src io.Reader, max int64, keys map[string]ed25519.PublicKey) (Artifact, error) {
	var a Artifact
	if max <= 0 {
		return a, errors.New("invalid checkpoint size limit")
	}
	tr := tar.NewReader(src)
	seen := map[string]bool{}
	var n int64
	var digest string
	for {
		hdr, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return a, e
		}
		limit := int64(maxMetadata)
		switch hdr.Name {
		case ManifestFile, SignatureFile:
		case CheckpointFile:
			limit = max
		default:
			return a, fmt.Errorf("unknown archive member %q", hdr.Name)
		}
		if seen[hdr.Name] || hdr.Typeflag != tar.TypeReg || hdr.Size <= 0 || hdr.Size > limit || len(hdr.PAXRecords) > 0 {
			return a, errors.New("invalid archive member")
		}
		seen[hdr.Name] = true
		size, h, e := writeFile(dir, hdr.Name, tr, limit)
		if e != nil {
			return a, e
		}
		if hdr.Name == CheckpointFile {
			n, digest = size, h
		}
	}
	if !seen[CheckpointFile] || !seen[ManifestFile] {
		return a, errors.New("incomplete archive")
	}
	// A second concatenated archive is not part of the signed object.
	var tail [1]byte
	if _, e := io.ReadFull(src, tail[:]); e != io.EOF {
		return a, errors.New("unexpected bytes after archive")
	}
	raw, e := os.ReadFile(filepath.Join(dir, ManifestFile))
	if e != nil {
		return a, e
	}
	a.Raw = raw
	var sig Signature
	if seen[SignatureFile] {
		b, e := os.ReadFile(filepath.Join(dir, SignatureFile))
		if e != nil {
			return a, e
		}
		if strictJSON(b, &sig) == nil && sig.Version == 1 && sig.Algorithm == "Ed25519" {
			key := keys[sig.KeyID]
			signed, e := base64.StdEncoding.Strict().DecodeString(sig.Value)
			a.Trusted = e == nil && len(key) == ed25519.PublicKeySize && ed25519.Verify(key, raw, signed)
			a.KeyID = sig.KeyID
		}
	}
	if !a.Trusted {
		a.Reason = "untrusted manifest signature"
	}
	if e = strictJSON(raw, &a.Manifest); e != nil {
		return a, e
	}
	if e = a.Manifest.validate(); e != nil {
		return a, e
	}
	if a.Manifest.Digest != digest || a.Manifest.Size != n {
		return a, errors.New("checkpoint digest or length mismatch")
	}
	return a, syncDir(dir)
}
func Export(dst io.Writer, dir string) error {
	tw := tar.NewWriter(dst)
	for _, name := range []string{ManifestFile, SignatureFile, CheckpointFile} {
		f, e := os.Open(filepath.Join(dir, name))
		if e != nil {
			return e
		}
		info, e := f.Stat()
		if e == nil {
			e = tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: info.Size(), Typeflag: tar.TypeReg})
		}
		if e == nil {
			_, e = io.Copy(tw, f)
		}
		f.Close()
		if e != nil {
			return e
		}
	}
	return tw.Close()
}

// VerifyDirectory verifies the immutable manifest and checkpoint files in a
// stored standalone artifact. Its keys must come from local operator trust.
func VerifyDirectory(dir string, keys map[string]ed25519.PublicKey) (Artifact, error) {
	var artifact Artifact
	readMetadata := func(name string) ([]byte, error) {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		defer f.Close()
		data, err := io.ReadAll(io.LimitReader(f, maxMetadata+1))
		if err == nil && len(data) > maxMetadata {
			err = errors.New("artifact metadata exceeds size limit")
		}
		return data, err
	}
	var err error
	artifact.Raw, err = readMetadata(ManifestFile)
	if err != nil {
		return artifact, err
	}
	if err := strictJSON(artifact.Raw, &artifact.Manifest); err != nil {
		return artifact, err
	}
	if err := artifact.Manifest.validate(); err != nil {
		return artifact, err
	}
	signature, err := readMetadata(SignatureFile)
	if err != nil {
		return artifact, err
	}
	var sig Signature
	if err := strictJSON(signature, &sig); err != nil {
		return artifact, err
	}
	key := keys[sig.KeyID]
	signed, err := base64.StdEncoding.Strict().DecodeString(sig.Value)
	if err != nil || sig.Version != 1 || sig.Algorithm != "Ed25519" ||
		len(key) != ed25519.PublicKeySize || !ed25519.Verify(key, artifact.Raw, signed) {
		return artifact, errors.New("untrusted artifact manifest signature")
	}
	artifact.Trusted, artifact.KeyID = true, sig.KeyID
	checkpoint, err := os.Open(filepath.Join(dir, CheckpointFile))
	if err != nil {
		return artifact, err
	}
	defer checkpoint.Close()
	h := sha256.New()
	length, err := io.Copy(h, checkpoint)
	if err != nil {
		return artifact, err
	}
	if length != artifact.Manifest.Size || hex.EncodeToString(h.Sum(nil)) != artifact.Manifest.Digest {
		return artifact, errors.New("checkpoint digest or length mismatch")
	}
	return artifact, nil
}
