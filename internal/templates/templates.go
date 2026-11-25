package templates

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
)

type Spec struct {
	BootMode         rt.BootMode `json:"boot_mode,omitempty"`
	LocalImage       string      `json:"local_image,omitempty"`
	RootGenerationID string      `json:"root_generation_id,omitempty"`
}

var generationID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var imageDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func ReadSpec(t registry.Template) (Spec, error) {
	var spec Spec
	if len(t.Spec) != 0 {
		if err := json.Unmarshal(t.Spec, &spec); err != nil {
			return spec, fmt.Errorf("%w: malformed template specification: %v", registry.ErrInvalid, err)
		}
	}
	if spec.BootMode == "" {
		spec.BootMode = rt.BootOCI
	}
	switch spec.BootMode {
	case rt.BootOCI:
		if spec.RootGenerationID != "" {
			return spec, fmt.Errorf("%w: root_generation_id requires a System template", registry.ErrInvalid)
		}
	case rt.BootSystem:
		if !generationID.MatchString(spec.RootGenerationID) || !imageDigest.MatchString(t.ImageDigest) ||
			t.ImageDigest == "sha256:"+strings.Repeat("0", 64) || spec.LocalImage != "" {
			return spec, fmt.Errorf("%w: System template requires an imported root generation and its SHA-256 digest", registry.ErrInvalid)
		}
	default:
		return spec, fmt.Errorf("%w: unknown template boot mode", registry.ErrInvalid)
	}
	return spec, nil
}

// VerifyLocal checks administrator-registered image bytes before dispatch to serve.
func VerifyLocal(dir string, t registry.Template) error {
	spec, err := ReadSpec(t)
	if err != nil || spec.LocalImage == "" {
		return err
	}
	if spec.LocalImage != filepath.Base(spec.LocalImage) || spec.LocalImage == "." || spec.LocalImage == ".." {
		return fmt.Errorf("invalid local image name")
	}
	path := filepath.Join(dir, spec.LocalImage)
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
