package platformapi

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"sandbox-platform/internal/config"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/registry"
)

func TestMachineWebURLSelectsOnePublishedHTTPService(t *testing.T) {
	ctx := context.Background()
	db, err := registry.Open(ctx, filepath.Join(t.TempDir(), "web.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cfg := config.Defaults()
	cfg.Gateway.AppsDomain = "apps.example.test"
	s := Server{Engine: &lifecycle.Engine{DB: db, Config: cfg}}
	mid := "m_" + strings.Repeat("a", 32)
	host := machineWebHost(mid, cfg.Gateway.AppsDomain)
	if s.webMachine(host) != mid || s.webMachine("x."+host) != "" || machineWebHost(mid, "") != "" || machineWebHost("m_not-a-dns-id", cfg.Gateway.AppsDomain) != "" {
		t.Fatal(host)
	}
	if err := db.EnsureTemplate(ctx, registry.Template{ID: "t", Name: "t", Version: 1, ImageDigest: "sha256:x"}); err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.DB().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("INSERT INTO machines(id,name,template_id,revision,binding_generation,desired_power,spec_json,idle_policy_json,keep_running) VALUES(?, 'app','t',1,0,'stopped','{}','{}',0)", mid)
	add := func(id, name, protocol string, enabled bool) {
		exec("INSERT INTO services(id,machine_id,name,revision,kind,guest_port,public_host,readiness_json,access_json,enabled) VALUES(?,?,?,1,?,80,?,'{}','{}',?)", id, mid, name, protocol, id+".apps.example.test", enabled)
	}
	check := func(host, want string) {
		t.Helper()
		v, err := s.httpServiceForHost(ctx, host)
		if want == "" && errors.Is(err, sql.ErrNoRows) {
			return
		}
		if want == "ambiguous" {
			var reason *lifecycle.Failure
			if errors.As(err, &reason) && reason.Code == "AMBIGUOUS_WEB_SERVICE" {
				return
			}
		}
		if err != nil || v.ID != want || want == "" {
			t.Fatalf("target for %s is %q, %v; want %s", host, v.ID, err, want)
		}
	}
	add("ssh", "ssh", "ssh", true)
	add("disabled", "disabled", "http", false)
	check(host, "")
	add("one", "one", "http", true)
	check(host, "one")
	add("two", "two", "https", true)
	check(host, "ambiguous")
	check("one.apps.example.test", "one")
	add("three", "web", "https", true)
	check(host, "three")
	exec("UPDATE machines SET name='renamed' WHERE id=?", mid)
	check(host, "three")
	exec("UPDATE services SET enabled=0 WHERE id='three'")
	check(host, "ambiguous")
	exec("UPDATE machines SET deleted_at=1 WHERE id=?", mid)
	check(host, "")
}
