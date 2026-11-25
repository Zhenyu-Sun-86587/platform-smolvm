package templates

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sandbox-platform/internal/registry"
	"strings"
)

// VerifyLocal checks administrator-registered image bytes before dispatch to serve.
func VerifyLocal(dir string, t registry.Template) error {
	var spec struct {
		Local string `json:"local_image"`
	}
	if len(t.Spec) == 0 {
		return nil
	}
	if err := json.Unmarshal(t.Spec, &spec); err != nil {
		return err
	}
	if spec.Local == "" {
		return nil
	}
	if spec.Local != filepath.Base(spec.Local) || spec.Local == "." || spec.Local == ".." {
		return fmt.Errorf("invalid local image name")
	}
	path := filepath.Join(dir, spec.Local)
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("registered local image must be a regular archive")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return err
	}
	if strings.TrimPrefix(t.ImageDigest, "sha256:") != hex.EncodeToString(h.Sum(nil)) {
		return fmt.Errorf("image digest differs from registered template")
	}
	return nil
}
