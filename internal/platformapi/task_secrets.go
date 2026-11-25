package platformapi

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

const taskEnvFormat = "SAE1"

func (s *Server) taskEnvCipher(create bool) (cipher.AEAD, error) {
	dir := filepath.Join(s.Config.StateDir, "secrets")
	const name = "task-env-key-v1"
	path := filepath.Join(dir, name)
	key, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) && create {
		var fresh [32]byte
		if _, err := rand.Read(fresh[:]); err != nil {
			return nil, err
		}
		if err := persistSecret(dir, name, fresh[:]); err != nil {
			return nil, err
		}
		key, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, errors.New("invalid task environment encryption key")
	}
	block, err := aes.NewCipher(key)
	clear(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (s *Server) storeEnv(env map[string]string) (string, error) {
	if len(env) == 0 {
		return "", nil
	}
	raw, err := json.Marshal(env)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(raw)
	id := hex.EncodeToString(h[:])
	key, err := s.taskEnvCipher(true)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, key.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	body := append([]byte(taskEnvFormat), nonce...)
	body = key.Seal(body, nonce, raw, []byte(id))
	if err := persistSecret(filepath.Join(s.Config.StateDir, "secrets", "task-env-v1"), id, body); err != nil {
		return "", err
	}
	return id, nil
}

func persistSecret(dir, id string, raw []byte) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".new-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	e := f.Close()
	if err != nil {
		return err
	}
	if e != nil {
		return e
	}
	if err = os.Link(f.Name(), filepath.Join(dir, id)); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	if err := syncSecretDirectory(dir); err != nil {
		return err
	}
	return syncSecretDirectory(filepath.Dir(dir))
}

func syncSecretDirectory(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

type storedTask struct {
	taskRequest
	EnvDigest string `json:"env_digest,omitempty"`
}

func (s *Server) loadTask(raw json.RawMessage) (taskRequest, error) {
	var req storedTask
	if err := json.Unmarshal(raw, &req); err != nil {
		return req.taskRequest, err
	}
	if req.EnvDigest == "" {
		return req.taskRequest, nil
	}
	digest, err := hex.DecodeString(req.EnvDigest)
	if err != nil || len(digest) != 32 {
		return req.taskRequest, errors.New("invalid environment reference")
	}
	b, err := s.openTaskEnvironment(req.EnvDigest)
	if err != nil {
		return req.taskRequest, err
	}
	h := sha256.Sum256(b)
	if hex.EncodeToString(h[:]) != req.EnvDigest {
		return req.taskRequest, errors.New("task environment integrity check failed")
	}
	err = json.Unmarshal(b, &req.Env)
	return req.taskRequest, err
}

func (s *Server) openTaskEnvironment(id string) ([]byte, error) {
	dir := filepath.Join(s.Config.StateDir, "secrets")
	sealed, err := os.ReadFile(filepath.Join(dir, "task-env-v1", id))
	if errors.Is(err, os.ErrNotExist) {
		return os.ReadFile(filepath.Join(dir, "task-env", id))
	}
	if err != nil {
		return nil, err
	}
	key, err := s.taskEnvCipher(false)
	if err != nil {
		return nil, err
	}
	prefix := len(taskEnvFormat)
	nonceEnd := prefix + key.NonceSize()
	if len(sealed) < nonceEnd+key.Overhead() || string(sealed[:prefix]) != taskEnvFormat {
		return nil, errors.New("invalid encrypted task environment")
	}
	return key.Open(nil, sealed[prefix:nonceEnd], sealed[nonceEnd:], []byte(id))
}
