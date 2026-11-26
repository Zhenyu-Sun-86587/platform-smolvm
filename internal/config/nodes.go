package config

import (
	"errors"
	"math"
	"net/url"
	"path/filepath"

	"sandbox-platform/internal/nodeproto"
)

// Node points to an explicitly enrolled Worker. Only platformd reads these
// client credentials; the public API returns the persistent Worker identity.
type Node struct {
	ID           string      `yaml:"worker_id"`
	Name         string      `yaml:"name"`
	Endpoint     string      `yaml:"endpoint"`
	Generation   uint64      `yaml:"enrollment_generation"`
	Pin          string      `yaml:"node_spki_pin"`
	JournalID    string      `yaml:"journal_id"`
	CA           string      `yaml:"ca_file"`
	CASigningKey string      `yaml:"ca_signing_key_file,omitempty"`
	Certificate  string      `yaml:"certificate_file"`
	Key          string      `yaml:"key_file"`
	Routes       []NodeRoute `yaml:"routes"`
}
type NodeRoute struct {
	nodeproto.Route `yaml:",inline"`
	Reader          string `yaml:"state_reader_id"`
	Manifest        string `yaml:"manifest"`
	// Candidate local generations are published only after resourced verifies
	// the immutable content and the committed import on this route.
	Artifacts map[string]string `yaml:"artifacts"`
}

func validateNodes(nodes []Node, instances map[string]bool) error {
	ids, pins := map[string]bool{}, map[string]bool{}
	if len(nodes) > 64 {
		return errors.New("too many configured Workers")
	}
	for _, n := range nodes {
		u, err := url.Parse(n.Endpoint)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
			return errors.New("Worker endpoint requires an HTTPS origin")
		}
		if !nodeproto.ValidID(n.ID) || n.Generation == 0 || n.Generation > math.MaxInt64 || !nodeproto.ValidDigest(n.Pin) || ids[n.ID] || pins[n.Pin] || len(n.Routes) == 0 || len(n.Routes) > 64 {
			return errors.New("Worker enrollment requires a distinct identity and key pin, a generation and registered routes")
		}
		if n.JournalID != "" && !nodeproto.ValidJournalID(n.JournalID) {
			return errors.New("Worker journal_id must use a persistent Node journal identity")
		}
		ids[n.ID], pins[n.Pin] = true, true
		for _, path := range []string{n.CA, n.Certificate, n.Key} {
			if !filepath.IsAbs(path) || filepath.Clean(path) != path {
				return errors.New("Node TLS files require canonical absolute paths")
			}
		}
		if n.CASigningKey != "" {
			if !filepath.IsAbs(n.CASigningKey) || filepath.Clean(n.CASigningKey) != n.CASigningKey {
				return errors.New("Worker CA signing key requires a canonical absolute path")
			}
			for _, other := range nodes {
				if n.CASigningKey == other.Key || n.CASigningKey == other.Certificate || n.CASigningKey == other.CA ||
					(n.ID != other.ID && (n.CASigningKey == other.CASigningKey || n.CA == other.CA)) {
					return errors.New("Worker enrollment signer requires a distinct CA and key")
				}
			}
		}
		locals := map[string]bool{}
		for _, route := range n.Routes {
			if !route.Valid() || !nodeproto.ValidID(route.Reader) || instances[route.RuntimeInstanceID] || locals[route.LocalInstanceID] || !filepath.IsAbs(route.Manifest) || filepath.Clean(route.Manifest) != route.Manifest || len(route.Artifacts) > 128 {
				return errors.New("invalid or conflicting Worker runtime route, manifest or reader")
			}
			for digest, generation := range route.Artifacts {
				if !nodeproto.ValidDigest(digest) || !nodeproto.ValidID(generation) {
					return errors.New("invalid Worker template digest or candidate local generation")
				}
			}
			instances[route.RuntimeInstanceID], locals[route.LocalInstanceID] = true, true
		}
	}
	return nil
}
