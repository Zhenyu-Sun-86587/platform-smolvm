package snapshots

import (
	"archive/tar"
	"bytes"
	"crypto/ed25519"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestArchiveAuthenticatesOriginalManifest(t *testing.T) {
	pub, priv, e := ed25519.GenerateKey(nil)
	if e != nil {
		t.Fatal(e)
	}
	dir := t.TempDir()
	m := Manifest{SourceMachineID: "m_source", SourceGeneration: 3, OperationID: "op_capture", TemplateID: "managed", TemplateDigest: "digest", Purpose: "clone", TaskContent: "clean", GuestStatus: "ready", CaptureReceipt: []byte(`{"ready":true}`)}
	if _, e := Capture(dir, bytes.NewBufferString("native stream"), 1000, m, "one", priv); e != nil {
		t.Fatal(e)
	}
	keys := map[string]ed25519.PublicKey{"one": pub}
	round := func(trusted bool) {
		t.Helper()
		var b bytes.Buffer
		if e := Export(&b, dir); e != nil {
			t.Fatal(e)
		}
		a, e := Import(t.TempDir(), &b, 1000, keys)
		if e != nil || a.Trusted != trusted {
			t.Fatalf("trust=%v, err=%v", a.Trusted, e)
		}
	}
	round(true)
	raw, e := os.ReadFile(filepath.Join(dir, ManifestFile))
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(dir, ManifestFile), bytes.Replace(raw, []byte("op_capture"), []byte("op_forged"), 1), 0600); e != nil {
		t.Fatal(e)
	}
	round(false)
	if e = os.WriteFile(filepath.Join(dir, CheckpointFile), []byte("changed bytes"), 0600); e != nil {
		t.Fatal(e)
	}
	var b bytes.Buffer
	if e := Export(&b, dir); e != nil {
		t.Fatal(e)
	}
	if _, e = Import(t.TempDir(), &b, 1000, keys); e == nil {
		t.Fatal("accepted changed checkpoint")
	}
}
func TestUnsafeArchive(t *testing.T) {
	for _, hdr := range []tar.Header{{Name: "../runtime.checkpoint", Typeflag: tar.TypeReg, Size: 1}, {Name: CheckpointFile, Typeflag: tar.TypeSymlink, Linkname: "/etc/hostname"}, {Name: ManifestFile, Typeflag: tar.TypeReg, Size: 8}} {
		var b bytes.Buffer
		w := tar.NewWriter(&b)
		if e := w.WriteHeader(&hdr); e != nil {
			t.Fatal(e)
		}
		if hdr.Typeflag == tar.TypeReg {
			io.CopyN(w, bytes.NewReader(make([]byte, 8)), hdr.Size)
		}
		w.Close()
		if _, e := Import(t.TempDir(), &b, 4, nil); e == nil {
			t.Fatal("accepted invalid archive", hdr.Name)
		}
	}
}
