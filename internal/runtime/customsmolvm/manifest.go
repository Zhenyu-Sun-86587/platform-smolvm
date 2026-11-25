package customsmolvm

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// LoadManifest reads the operator-pinned manifest for durable route registration.
func (r Route) LoadManifest() (json.RawMessage, error) {
	f, err := os.Open(r.Manifest)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	const limit = 16 << 20
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > limit || !json.Valid(raw) {
		return nil, fmt.Errorf("invalid RuntimeBundle/1 manifest")
	}
	var manifest struct {
		Schema       string
		Platform     string
		BundleDigest string `json:"bundle_digest"`
		Contracts    struct {
			StateReader string `json:"state_reader_id"`
		}
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, err
	}
	if manifest.Schema != "RuntimeBundle/1" || manifest.Platform != "linux-x86_64" ||
		manifest.BundleDigest != r.Bundle || manifest.Contracts.StateReader != r.Reader {
		return nil, fmt.Errorf("RuntimeBundle/1 manifest differs from configured digest and reader")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return nil, err
	}
	delete(payload, "bundle_digest")
	delete(payload, "signature")
	var canonical bytes.Buffer
	encoder := json.NewEncoder(&canonical)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(payload); err != nil {
		return nil, err
	}
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(bytes.TrimSuffix(canonical.Bytes(), []byte("\n"))))
	if digest != r.Bundle {
		return nil, fmt.Errorf("RuntimeBundle/1 manifest digest mismatch")
	}
	return raw, nil
}
