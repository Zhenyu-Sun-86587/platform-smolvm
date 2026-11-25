package platformapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"sandbox-platform/internal/config"
	"sandbox-platform/internal/lifecycle"
)

func TestTaskEnvironmentSecretSurvivesLegacyReplayAndDetectsTampering(t *testing.T) {
	cfg := config.Defaults()
	cfg.StateDir = t.TempDir()
	server := &Server{Engine: &lifecycle.Engine{Config: cfg}}
	secrets := filepath.Join(cfg.StateDir, "secrets")
	legacy := []byte(`{"TOKEN":"historical-fixture-value"}`)
	legacySum := sha256.Sum256(legacy)
	legacyID := hex.EncodeToString(legacySum[:])
	if err := persistSecret(filepath.Join(secrets, "task-env"), legacyID, legacy); err != nil {
		t.Fatal(err)
	}
	request := json.RawMessage(`{"env_digest":"` + legacyID + `"}`)
	decoded, err := server.loadTask(request)
	if err != nil || decoded.Env["TOKEN"] != "historical-fixture-value" {
		t.Fatal(decoded.Env, err)
	}
	id, err := server.storeEnv(decoded.Env)
	if err != nil || id != legacyID {
		t.Fatalf("legacy replay changed its operation reference: %s %v", id, err)
	}
	path := filepath.Join(secrets, "task-env-v1", id)
	sealed, err := os.ReadFile(path)
	if err != nil || bytes.Contains(sealed, []byte("historical-fixture-value")) {
		t.Fatal("new environment material was not encrypted", err)
	}
	decoded, err = server.loadTask(request)
	if err != nil || decoded.Env["TOKEN"] != "historical-fixture-value" {
		t.Fatal(decoded.Env, err)
	}
	sealed[len(sealed)-1] ^= 1
	if err := os.WriteFile(path, sealed, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := server.loadTask(request); err == nil {
		t.Fatal("tampered encrypted environment was accepted")
	}
	fresh, err := server.storeEnv(map[string]string{"TOKEN": "new-fixture-value"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(secrets, "task-env", fresh)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("new environment left a plaintext file", err)
	}
	key, err := os.Stat(filepath.Join(secrets, "task-env-key-v1"))
	if err != nil || key.Mode().Perm() != 0600 {
		t.Fatal("task environment key permissions", err)
	}
}
