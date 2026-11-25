// Package config loads operator-owned settings for the single-node platform.
package config

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"sandbox-platform/internal/runtime/customsmolvm"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Version       int
	TemplatesFile string `yaml:"templates_file"`
	StateDir      string `yaml:"state_dir"`
	API           struct {
		Listen       string
		PublicOrigin string `yaml:"public_origin"`
		TLSCert      string `yaml:"tls_cert_file"`
		TLSKey       string `yaml:"tls_key_file"`
	}
	Runtime struct {
		LocalImageDir               string `yaml:"local_image_dir"`
		StatusSocket                string `yaml:"status_socket"`
		VerifySocket                string `yaml:"verify_socket"`
		Version, Socket, Executable string
		DataDir                     string `yaml:"data_dir"`
		Custom                      []customsmolvm.Route
	}
	Database struct {
		Path        string
		BusyTimeout int `yaml:"busy_timeout_ms"`
	}
	Lifecycle struct {
		IdleSeconds  int  `yaml:"default_idle_seconds"`
		StartupGuard int  `yaml:"startup_idle_guard_seconds"`
		StatusStale  int  `yaml:"guest_status_stale_seconds"`
		WakeStopped  bool `yaml:"wake_stopped"`
		Heavy        int  `yaml:"max_heavy_operations"`
	}
	Gateway struct {
		AppsDomain     string `yaml:"apps_domain"`
		MaxWaiters     int    `yaml:"max_waiters_per_machine"`
		MaxConnections int    `yaml:"max_connections"`
		WaitSeconds    int    `yaml:"wake_wait_seconds"`
		PrefixBytes    int    `yaml:"queued_prefix_bytes"`
		BackendRange   []int  `yaml:"backend_port_range"`
		TCPRange       []int  `yaml:"tcp_mtls_port_range"`
		ClientCA       string `yaml:"client_ca_file"`
		ClientCAKey    string `yaml:"client_ca_key_file"`
	}
	Storage struct {
		MinBytes   uint64 `yaml:"minimum_free_bytes"`
		MinPercent int    `yaml:"minimum_free_percent"`
		Encoding   string `yaml:"snapshot_encoding"`
		KeyID      string `yaml:"manifest_signing_key_id"`
		KeyFile    string `yaml:"manifest_signing_key_file"`
		Trusted    []struct {
			ID   string `yaml:"key_id"`
			File string `yaml:"public_key_file"`
		} `yaml:"trusted_manifest_signers"`
	}
	Tasks struct {
		Protocol     int    `yaml:"protocol_version"`
		OutputLimit  int64  `yaml:"default_output_limit_bytes"`
		SegmentBytes int    `yaml:"output_segment_bytes"`
		GraceSeconds int    `yaml:"cancel_grace_seconds"`
		Policy       string `yaml:"default_output_policy"`
	}
	Observability struct {
		LogFormat  string `yaml:"log_format"`
		EventsDays int    `yaml:"events_retention_days"`
		AuditDays  int    `yaml:"audit_retention_days"`
	}
	Backups []BackupPolicy `yaml:"backups"`
}

const BackupPortable = "platform_snapshots"
const BackupNative = "durable_runtime"

type BackupPolicy struct {
	ID              string `yaml:"id" json:"policy_id"`
	Scope           string `yaml:"scope" json:"scope"`
	Directory       string `yaml:"directory" json:"-"`
	IntervalSeconds int    `yaml:"interval_seconds" json:"interval_seconds"`
	Retain          int    `yaml:"retain" json:"retain"`
	RecoveryKeyFile string `yaml:"recovery_key_file" json:"-"`
}

func Defaults() Config {
	var c Config
	c.Version = 1
	c.StateDir = "/var/lib/sandbox-platform"
	c.API.Listen = "127.0.0.1:8443"
	c.API.PublicOrigin = "https://console.example.test"
	c.Runtime.LocalImageDir = "/var/lib/smolvm-platform-images"
	c.Runtime.Version = "1.37.1"
	c.Runtime.Socket = "/run/smolvm-platform/api.sock"
	c.Runtime.StatusSocket = "/run/smolvm-platform/status.sock"
	c.Runtime.Executable = "/opt/sandbox-platform/runtime/1.37.1/bin/smolvm"
	c.Runtime.DataDir = "/var/lib/smolvm-platform-runtime"
	c.Database.BusyTimeout = 5000
	c.Lifecycle.IdleSeconds = 1800
	c.Lifecycle.StartupGuard = 60
	c.Lifecycle.StatusStale = 30
	c.Lifecycle.Heavy = 1
	c.Gateway.MaxConnections = 256
	c.Gateway.MaxWaiters = 64
	c.Gateway.WaitSeconds = 90
	c.Gateway.PrefixBytes = 65536
	c.Gateway.BackendRange = []int{30000, 39999}
	c.Gateway.TCPRange = []int{20000, 20999}
	c.Storage.MinBytes = 2 << 30
	c.Storage.MinPercent = 15
	c.Storage.Encoding = "standalone"
	c.Storage.KeyID = "local-v1"
	c.Tasks.Protocol = 1
	c.Tasks.OutputLimit = 256 << 20
	c.Tasks.SegmentBytes = 8 << 20
	c.Tasks.GraceSeconds = 10
	c.Tasks.Policy = "truncate"
	c.Observability.LogFormat = "json"
	c.Observability.EventsDays = 7
	c.Observability.AuditDays = 90
	return c
}
func Load(path string) (Config, error) {
	c := Defaults()
	if path != "" {
		f, e := os.Open(path)
		if e != nil {
			return c, e
		}
		defer f.Close()
		d := yaml.NewDecoder(f)
		d.KnownFields(true)
		if e = d.Decode(&c); e != nil {
			return c, e
		}
		var extra any
		if e = d.Decode(&extra); e != io.EOF {
			return c, errors.New("expected a single config document")
		}
	}
	if c.Database.Path == "" {
		c.Database.Path = filepath.Join(c.StateDir, "platform.db")
	}
	if c.Storage.KeyFile == "" {
		c.Storage.KeyFile = filepath.Join(c.StateDir, "secrets", "snapshot-ed25519.pem")
	}
	if c.Version != 1 || c.Runtime.Version != "1.37.1" || c.Tasks.Protocol != 1 || c.Storage.Encoding != "standalone" {
		return c, errors.New("unsupported platform, runtime, task protocol or snapshot format")
	}
	for _, p := range []string{c.StateDir, c.Runtime.Socket, c.Runtime.DataDir, c.Database.Path, c.Storage.KeyFile} {
		if !filepath.IsAbs(p) {
			return c, fmt.Errorf("absolute path required for %q", p)
		}
	}
	if c.Runtime.StatusSocket != "" && (!filepath.IsAbs(c.Runtime.StatusSocket) || c.Runtime.StatusSocket == c.Runtime.Socket) {
		return c, errors.New("runtime status_socket must be a separate absolute socket path")
	}
	if c.Runtime.VerifySocket != "" && (!filepath.IsAbs(c.Runtime.VerifySocket) || c.Runtime.VerifySocket == c.Runtime.Socket || c.Runtime.VerifySocket == c.Runtime.StatusSocket) {
		return c, errors.New("runtime verify_socket must be an independent absolute socket path")
	}
	instances := make(map[string]bool)
	sockets := map[string]bool{}
	for _, socket := range []string{c.Runtime.Socket, c.Runtime.StatusSocket, c.Runtime.VerifySocket} {
		if socket != "" {
			sockets[filepath.Clean(socket)] = true
		}
	}
	for _, route := range c.Runtime.Custom {
		if err := route.Validate(); err != nil {
			return c, err
		}
		if !filepath.IsAbs(route.Manifest) || filepath.Clean(route.Manifest) != route.Manifest {
			return c, errors.New("custom runtime manifest must use a canonical absolute path")
		}
		if instances[route.InstanceID] || sockets[route.Socket] {
			return c, errors.New("custom runtime instances and socket paths must be distinct")
		}
		instances[route.InstanceID] = true
		sockets[route.Socket] = true
	}
	if c.Storage.MinPercent < 0 || c.Storage.MinPercent > 100 || c.Lifecycle.Heavy < 1 || c.Gateway.MaxConnections < 1 || c.Gateway.MaxWaiters < 1 || c.Gateway.WaitSeconds < 1 {
		return c, errors.New("invalid capacity settings")
	}
	seenPolicy := map[string]bool{}
	for _, policy := range c.Backups {
		if len(policy.ID) == 0 || len(policy.ID) > 64 || strings.Trim(policy.ID, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-") != "" || seenPolicy[policy.ID] {
			return c, errors.New("backup policies require distinct IDs containing letters, digits, underscores or dashes")
		}
		if (policy.Scope != BackupPortable && policy.Scope != BackupNative) || policy.IntervalSeconds < 1 || policy.IntervalSeconds > 31*86400 || policy.Retain < 1 {
			return c, errors.New("backup policy requires a supported scope, a positive interval and retention of at least one verified backup")
		}
		if !filepath.IsAbs(policy.Directory) || filepath.Clean(policy.Directory) != policy.Directory || policy.Directory == "/" ||
			!filepath.IsAbs(policy.RecoveryKeyFile) {
			return c, errors.New("backup destination and recovery key require absolute paths")
		}
		seenPolicy[policy.ID] = true
	}
	host, _, e := net.SplitHostPort(c.API.Listen)
	if e != nil {
		return c, e
	}
	if (c.API.TLSCert == "") != (c.API.TLSKey == "") {
		return c, errors.New("TLS needs both certificate and key")
	}
	if c.API.TLSKey == "" && !net.ParseIP(host).IsLoopback() {
		return c, errors.New("public listener requires TLS")
	}
	u, e := url.Parse(c.API.PublicOrigin)
	if e != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && net.ParseIP(u.Hostname()).IsLoopback())) || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return c, errors.New("invalid public_origin")
	}
	if c.Gateway.AppsDomain != "" && (u.Hostname() == c.Gateway.AppsDomain || strings.HasSuffix(u.Hostname(), "."+c.Gateway.AppsDomain)) {
		return c, errors.New("management origin must be separate from service domain")
	}
	for _, r := range [][]int{c.Gateway.BackendRange, c.Gateway.TCPRange} {
		if len(r) != 2 || r[0] < 1 || r[0] > r[1] || r[1] > 65535 {
			return c, errors.New("invalid port range")
		}
	}
	return c, nil
}
