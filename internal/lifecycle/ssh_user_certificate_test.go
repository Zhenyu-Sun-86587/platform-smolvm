package lifecycle

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"golang.org/x/crypto/ssh"
	"sandbox-platform/internal/registry"
)

func TestManagedSystemSSHResyncsCertificateTrust(t *testing.T) {
	ctx := context.Background()
	e, _, _ := runtimeTestEngine(t, ctx, nil)
	e.Config.StateDir = t.TempDir()
	binding := registry.Binding{
		MachineID: "original", Status: "current", RuntimeInstanceObjectID: "runtime", IncarnationID: "first", ExecutionEpoch: 1,
	}
	_, host, err := e.SSHHostKey(binding.MachineID)
	if err != nil {
		t.Fatal(err)
	}
	evidence := nativeSSHEvidence{Ready: true, Key: string(host), Execution: binding.ExecutionRef()}
	old, err := e.authorizedSSHKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	evidence.KeysetDigest = keysetDigest(old)
	binding.GuestManagementEvidence = JSON(evidence)
	if key, err := e.VerifiedNativeSSHKey(ctx, binding, SystemSSHPort); err != nil || key != "" {
		t.Fatalf("legacy digest verified before managed CA sync: %q %v", key, err)
	}
	caPath := filepath.Join(e.Config.StateDir, "secrets", "ssh-user-ca", binding.MachineID)
	if _, err := os.Stat(caPath); !os.IsNotExist(err) {
		t.Fatalf("passive inspection created CA: %v", err)
	}
	payload, err := e.systemAuthorizedSSHKeys(ctx, binding.MachineID, true)
	if err != nil {
		t.Fatal(err)
	}
	ca, _, options, _, err := ssh.ParseAuthorizedKey(payload)
	if err != nil || !slices.Equal(options, []string{"cert-authority", "restrict", "pty", "port-forwarding"}) {
		t.Fatalf("unexpected Guest authorized_keys format: %q, %v", options, err)
	}
	if bytes.Equal(ssh.MarshalAuthorizedKey(ca), host) {
		t.Fatal("Guest host identity reused as a user signing authority")
	}
	file, err := os.Stat(caPath)
	if err != nil {
		t.Fatal(err)
	}
	if file.Mode().Perm() != 0600 {
		t.Fatalf("signing authority permissions: %o", file.Mode().Perm())
	}
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	subject, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	reply, _, err := e.SignManagedSSHCertificate(binding.MachineID, subject)
	if err != nil {
		t.Fatal(err)
	}
	signed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(reply))
	if err != nil {
		t.Fatal(err)
	}
	cert, ok := signed.(*ssh.Certificate)
	if !ok || !bytes.Equal(cert.SignatureKey.Marshal(), ca.Marshal()) {
		t.Fatal("certificate was not signed by the synchronized authority")
	}
	if key, err := e.VerifiedNativeSSHKey(ctx, binding, SystemSSHPort); err != nil || key != "" {
		t.Fatalf("legacy digest verified after managed authority created: %q %v", key, err)
	}
	evidence.KeysetDigest = keysetDigest(payload)
	binding.GuestManagementEvidence = JSON(evidence)
	if key, err := e.VerifiedNativeSSHKey(ctx, binding, SystemSSHPort); err != nil || key != string(host) {
		t.Fatalf("current evidence did not verify: %q %v", key, err)
	}
	child := binding
	child.MachineID = "fork-or-restore"
	childKeys, err := e.systemAuthorizedSSHKeys(ctx, child.MachineID, true)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(payload, childKeys) {
		t.Fatal("new logical machine retained source authority")
	}
	if key, err := e.VerifiedNativeSSHKey(ctx, child, SystemSSHPort); err != nil || key != "" {
		t.Fatalf("inherited credentials verified for fork or restore: %q %v", key, err)
	}
	binding.IncarnationID = "replacement"
	binding.ExecutionEpoch++
	if key, err := e.VerifiedNativeSSHKey(ctx, binding, SystemSSHPort); err != nil || key != "" {
		t.Fatalf("earlier incarnation evidence verified after rollback: %q %v", key, err)
	}
}
