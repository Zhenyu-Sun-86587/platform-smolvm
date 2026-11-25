package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/backups"
	"sandbox-platform/internal/config"
	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/platformapi"
	"sandbox-platform/internal/registry"
	"sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/customsmolvm"
	"sandbox-platform/internal/runtime/smolvm"
	"sandbox-platform/internal/snapshots"
	"sandbox-platform/internal/templates"
	"sandbox-platform/web"
)

func main() {
	if err := run(); err != nil {
		slog.Error("platformd", "error", err)
		os.Exit(1)
	}
}
func run() error {
	cfg := flag.String("config", "/etc/sandbox-platform/config.yaml", "operator configuration")
	check := flag.Bool("check-config", false, "validate config and exit")
	flag.Parse()
	c, err := config.Load(*cfg)
	if err != nil {
		return err
	}
	if *check {
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	db, err := registry.Open(ctx, c.Database.Path)
	if err != nil {
		return err
	}
	defer db.Close()
	var owners int
	if err = db.ReadDB().QueryRowContext(ctx, "SELECT count(*) FROM principals").Scan(&owners); err != nil {
		return err
	}
	if owners == 0 {
		file := os.Getenv("SANDBOX_OWNER_PASSWORD_FILE")
		if file == "" {
			return fmt.Errorf("SANDBOX_OWNER_PASSWORD_FILE is required for first start")
		}
		pw, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		user := os.Getenv("SANDBOX_OWNER_USER")
		if user == "" {
			user = "owner"
		}
		if _, err = auth.New(db).Bootstrap(ctx, user, strings.TrimSuffix(string(pw), "\n")); err != nil {
			return err
		}
	}
	if c.TemplatesFile != "" {
		raw, err := os.ReadFile(c.TemplatesFile)
		if err != nil {
			return err
		}
		var catalog []registry.Template
		if err = json.Unmarshal(raw, &catalog); err != nil {
			return err
		}
		for _, t := range catalog {
			if err = templates.VerifyLocal(c.Runtime.LocalImageDir, t); err != nil {
				return err
			}
			if err = db.EnsureTemplate(ctx, t); err != nil {
				return err
			}
		}
	}
	official, err := smolvm.New(c.Runtime.Socket, smolvm.WithLocalImageDir(c.Runtime.LocalImageDir), smolvm.WithStatusSocket(c.Runtime.StatusSocket))
	if err != nil {
		return err
	}
	custom := make(map[runtime.BundleRoute]runtime.Adapter, len(c.Runtime.Custom))
	for _, route := range c.Runtime.Custom {
		adapter, err := customsmolvm.New(route)
		if err != nil {
			return err
		}
		manifest, err := route.LoadManifest()
		if err != nil {
			return fmt.Errorf("custom runtime %q: %w", route.InstanceID, err)
		}
		if err := db.RegisterRuntime(ctx, route.Key(), route.Socket, manifest); err != nil {
			return err
		}
		custom[route.Key()] = adapter
	}
	engine := lifecycle.New(db, runtime.NewResolver(official, custom), c)
	catalog, err := snapshots.Open(db, c)
	if err != nil {
		return err
	}
	api := platformapi.New(engine, catalog, web.Handler())
	if err := api.StartGuestRelays(ctx); err != nil {
		return err
	}
	httpServer := &http.Server{Addr: c.API.Listen, Handler: api, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 64 << 10}
	go api.StartTCP(ctx)
	backupDone := make(chan struct{})
	go func() { defer close(backupDone); backups.Run(ctx, db, c, *cfg) }()
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); engine.Run(ctx) }()
	errs := make(chan error, 1)
	go func() {
		if c.API.TLSCert != "" {
			errs <- httpServer.ListenAndServeTLS(c.API.TLSCert, c.API.TLSKey)
		} else {
			errs <- httpServer.ListenAndServe()
		}
	}()
	var result error
	select {
	case result = <-errs:
	case <-ctx.Done():
	}
	stop()
	shutdown, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdown)
	_ = httpServer.Close()
	<-workerDone
	<-backupDone
	if errors.Is(result, http.ErrServerClosed) {
		return nil
	}
	return result
}
