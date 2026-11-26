package nodegateway

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"sandbox-platform/internal/nodeproto"
	"sandbox-platform/internal/runtime/customsmolvm"
	"sandbox-platform/internal/runtime/workerclient"
)

type Config struct {
	Authority nodeproto.Authority `yaml:"authority"`
	Listen    string              `yaml:"listen"`
	StateDir  string              `yaml:"state_dir"`
	TLS       struct {
		Certificate       string `yaml:"certificate"`
		Key               string `yaml:"key"`
		ClientCA          string `yaml:"client_ca"`
		ControllerSPKIPin string `yaml:"controller_spki_pin"`
	} `yaml:"tls"`
	Routes []RouteConfig `yaml:"routes"`
}

type RouteConfig struct {
	RuntimeInstanceID  string   `yaml:"runtime_instance_id"`
	RootGenerations    []string `yaml:"root_generations,omitempty"`
	customsmolvm.Route `yaml:",inline"`
}

func LoadConfig(path string) (Config, error) {
	var c Config
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	const limit = 1 << 20
	info, err := f.Stat()
	if err != nil {
		return c, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return c, errors.New("Node config must be a regular file of at most 1 MiB")
	}
	decoder := yaml.NewDecoder(io.LimitReader(f, limit))
	decoder.KnownFields(true)
	if err := decoder.Decode(&c); err != nil {
		return c, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return c, errors.New("expected one Node config document")
	}
	if _, port, err := net.SplitHostPort(c.Listen); err != nil || port == "" {
		return c, errors.New("Node listen must be a TCP host and port")
	}
	if !c.Authority.Valid() || !nodeproto.ValidDigest(c.TLS.ControllerSPKIPin) || len(c.Routes) == 0 {
		return c, errors.New("Node requires enrolled authority, TLS pin and registered routes")
	}
	for _, path := range []string{c.StateDir, c.TLS.Key, c.TLS.Certificate, c.TLS.ClientCA} {
		if !canonicalAbsolutePath(path) {
			return c, errors.New("Node state and TLS files require canonical absolute paths")
		}
	}
	return c, nil
}

func canonicalAbsolutePath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && path != "/"
}

func (c Config) RuntimeRoutes() (map[nodeproto.Route]Route, error) {
	routes := make(map[nodeproto.Route]Route, len(c.Routes))
	sockets := make(map[string]bool, len(c.Routes))
	global := make(map[string]bool, len(c.Routes))
	local := make(map[[2]string]bool, len(c.Routes))
	for _, spec := range c.Routes {
		scope := nodeproto.Route{RuntimeInstanceID: spec.RuntimeInstanceID, LocalInstanceID: spec.InstanceID, BundleDigest: spec.Bundle}
		key := [2]string{scope.LocalInstanceID, scope.BundleDigest}
		if !scope.Valid() || sockets[spec.Socket] || global[scope.RuntimeInstanceID] || local[key] ||
			!canonicalAbsolutePath(spec.Manifest) {
			return nil, errors.New("runtime routes require distinct Serve sockets, Worker scopes and verified Bundle manifests")
		}
		sockets[spec.Socket], global[scope.RuntimeInstanceID], local[key] = true, true, true
		if _, err := spec.LoadManifest(); err != nil {
			return nil, fmt.Errorf("verify runtime %s Bundle manifest: %w", scope.RuntimeInstanceID, err)
		}
		a, err := customsmolvm.New(spec.Route)
		if err != nil {
			return nil, fmt.Errorf("configure runtime %s: %w", scope.RuntimeInstanceID, err)
		}
		roots := make(map[string]bool, len(spec.RootGenerations))
		for _, id := range spec.RootGenerations {
			if !nodeproto.ValidID(id) || roots[id] {
				return nil, errors.New("root generations require distinct registered identifiers")
			}
			roots[id] = true
		}
		routes[scope] = Route{Adapter: a, Worker: workerclient.Client{Socket: spec.WorkerSocket, Bundle: spec.Bundle},
			Reader: spec.Reader, LoadManifest: spec.LoadManifest, ResolveNetwork: spec.ResolvePrivateNetwork, RootGenerations: roots}
	}
	return routes, nil
}
