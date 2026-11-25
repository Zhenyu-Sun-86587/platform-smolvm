package platformapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

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
	dir := filepath.Join(s.Config.StateDir, "secrets", "task-env")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, ".new-")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	e := f.Close()
	if err != nil {
		return "", err
	}
	if e != nil {
		return "", e
	}
	if err = os.Link(f.Name(), filepath.Join(dir, id)); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	d, err := os.Open(dir)
	if err != nil {
		return "", err
	}
	defer d.Close()
	return id, d.Sync()
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
	b, err := os.ReadFile(filepath.Join(s.Config.StateDir, "secrets", "task-env", req.EnvDigest))
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
