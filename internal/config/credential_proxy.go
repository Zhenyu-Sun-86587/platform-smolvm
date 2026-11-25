package config

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"strings"
)

type CredentialProxy struct {
	ID                string                 `yaml:"id"`
	Upstream          string                 `yaml:"upstream"`
	CredentialFile    string                 `yaml:"credential_file"`
	CAFile            string                 `yaml:"ca_file"`
	AllowPrivate      bool                   `yaml:"allow_private"`
	Routes            []CredentialProxyRoute `yaml:"routes"`
	RequestsPerMinute int                    `yaml:"requests_per_minute"`
	RequestBytes      int64                  `yaml:"request_bytes"`
	ResponseBytes     int64                  `yaml:"response_bytes"`
	TimeoutSeconds    int                    `yaml:"timeout_seconds"`
}

type CredentialProxyRoute struct {
	Method string `yaml:"method"`
	Path   string `yaml:"path"`
}

func (c *Config) CredentialProxy(id string) *CredentialProxy {
	for i := range c.CredentialProxies {
		if c.CredentialProxies[i].ID == id {
			return &c.CredentialProxies[i]
		}
	}
	return nil
}

func ValidCredentialProxyID(id string) bool {
	return id != "" && len(id) <= 64 && strings.Trim(id, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-") == ""
}

func (p CredentialProxy) Allows(method, requestPath string) bool {
	for _, rule := range p.Routes {
		if rule.Method == method && rule.Path == requestPath {
			return true
		}
	}
	return false
}

func (c *Config) validateCredentialProxies() error {
	seen := map[string]bool{}
	for index := range c.CredentialProxies {
		p := &c.CredentialProxies[index]
		if !ValidCredentialProxyID(p.ID) || seen[p.ID] {
			return errors.New("credential proxies require distinct policy IDs")
		}
		seen[p.ID] = true
		upstream, err := url.Parse(p.Upstream)
		if err != nil || upstream.Scheme != "https" || upstream.Hostname() == "" || upstream.User != nil ||
			(upstream.Path != "" && upstream.Path != "/") || upstream.RawPath != "" || upstream.RawQuery != "" || upstream.Fragment != "" || upstream.ForceQuery {
			return fmt.Errorf("credential proxy %q requires a fixed HTTPS origin", p.ID)
		}
		platform, _ := url.Parse(c.API.PublicOrigin)
		if platform != nil && strings.EqualFold(platform.Hostname(), upstream.Hostname()) && ProxyPort(platform) == ProxyPort(upstream) {
			return fmt.Errorf("credential proxy %q points to the Platform API", p.ID)
		}
		if !filepath.IsAbs(p.CredentialFile) || filepath.Clean(p.CredentialFile) != p.CredentialFile ||
			(p.CAFile != "" && (!filepath.IsAbs(p.CAFile) || filepath.Clean(p.CAFile) != p.CAFile)) {
			return fmt.Errorf("credential proxy %q requires canonical host file paths", p.ID)
		}
		if len(p.Routes) == 0 || len(p.Routes) > 64 {
			return fmt.Errorf("credential proxy %q requires explicit routes", p.ID)
		}
		for _, rule := range p.Routes {
			if !strings.HasPrefix(rule.Path, "/") || path.Clean(rule.Path) != rule.Path || strings.ContainsAny(rule.Path, "?#%\\\r\n ") {
				return fmt.Errorf("credential proxy %q has an invalid route path", p.ID)
			}
			switch rule.Method {
			case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE":
			default:
				return fmt.Errorf("credential proxy %q has an invalid route method", p.ID)
			}
		}
		if p.RequestsPerMinute == 0 {
			p.RequestsPerMinute = 60
		}
		if p.RequestBytes == 0 {
			p.RequestBytes = 2 << 20
		}
		if p.ResponseBytes == 0 {
			p.ResponseBytes = 16 << 20
		}
		if p.TimeoutSeconds == 0 {
			p.TimeoutSeconds = 60
		}
		if p.RequestsPerMinute < 1 || p.RequestBytes < 1 || p.RequestBytes > 16<<20 ||
			p.ResponseBytes < 1 || p.ResponseBytes > 256<<20 || p.TimeoutSeconds < 1 || p.TimeoutSeconds > 290 {
			return fmt.Errorf("credential proxy %q has invalid request limits", p.ID)
		}
	}
	return nil
}

func ProxyPort(origin *url.URL) string {
	if port := origin.Port(); port != "" {
		return port
	}
	if origin.Scheme == "https" {
		return "443"
	}
	return "80"
}
