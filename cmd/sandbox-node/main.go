package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"sandbox-platform/internal/nodegateway"
	"sandbox-platform/internal/nodeproto"
)

func main() {
	configPath := flag.String("config", "/etc/sandbox-node/config.yaml", "Node gateway configuration")
	from := flag.String("rebind-from", "", "offline authority rebind or certificate rotation using the previous configuration")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, *configPath, *from); err != nil {
		slog.Error("sandbox-node", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, configPath, rebindFrom string) error {
	c, err := nodegateway.LoadConfig(configPath)
	if err != nil {
		return err
	}
	if rebindFrom != "" {
		previous, err := nodegateway.LoadConfig(rebindFrom)
		if err != nil {
			return err
		}
		if previous.StateDir != c.StateDir {
			return errors.New("authority rebind requires the same Node state directory")
		}
		certificate, _, err := nodeproto.LoadTLS(c.TLS.Certificate, c.TLS.Key, c.TLS.ClientCA)
		if err != nil {
			return err
		}
		if err := nodeproto.VerifyCertificateURI(certificate.Leaf, nodeproto.WorkerURI(c.Authority)); err != nil {
			return err
		}
		return nodegateway.RebindJournal(c.StateDir, previous.Authority, c.Authority)
	}
	routes, err := c.RuntimeRoutes()
	if err != nil {
		return err
	}
	certificate, roots, err := nodeproto.LoadTLS(c.TLS.Certificate, c.TLS.Key, c.TLS.ClientCA)
	if err != nil {
		return err
	}
	journal, err := nodegateway.OpenJournal(c.StateDir, c.Authority)
	if err != nil {
		return err
	}
	defer journal.Close()
	gateway, err := nodegateway.New(c.Authority, routes, journal)
	if err != nil {
		return err
	}
	go gateway.RunGuestRelays(ctx)
	tlsConfig, err := gateway.TLSConfig(certificate, roots, c.TLS.ControllerSPKIPin)
	if err != nil {
		return err
	}
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	server := &http.Server{
		Handler: gateway.Handler(c.TLS.ControllerSPKIPin), TLSConfig: tlsConfig, Protocols: protocols,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	listener, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return err
	}
	bounded := &limitedListener{Listener: listener, slots: make(chan struct{}, 128), closed: make(chan struct{})}
	defer bounded.Close()
	serve := make(chan error, 1)
	go func() { serve <- server.ServeTLS(bounded, "", "") }()
	slog.Info("NodeGateway/1 listening", "worker_id", c.Authority.WorkerID, "enrollment_generation", c.Authority.EnrollmentGeneration)
	select {
	case err := <-serve:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	}
}

type limitedListener struct {
	net.Listener
	slots  chan struct{}
	closed chan struct{}
	once   sync.Once
}

func (l *limitedListener) Accept() (net.Conn, error) {
	select {
	case l.slots <- struct{}{}:
	case <-l.closed:
		return nil, net.ErrClosed
	}
	conn, err := l.Listener.Accept()
	if err != nil {
		<-l.slots
		return nil, err
	}
	return &limitedConn{Conn: conn, release: func() { <-l.slots }}, nil
}

func (l *limitedListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return l.Listener.Close()
}

type limitedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}
