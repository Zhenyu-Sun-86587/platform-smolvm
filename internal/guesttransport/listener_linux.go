// Package guesttransport manages host-owned, execution-specific relay sockets.
package guesttransport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"syscall"
	"time"
)

var incarnationID = regexp.MustCompile(`^vmm-[a-f0-9]{32}$`)

func ValidIncarnation(id string) bool {
	return incarnationID.MatchString(id)
}

func EnsureDirectory(path string) error {
	if err := os.Mkdir(path, 0o751); err == nil {
		// The service umask is private while VMM UIDs need directory search.
		if err := os.Chmod(path, 0o751); err != nil {
			return err
		}
	} else if !os.IsExist(err) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || int(stat.Uid) != os.Geteuid() || info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("Guest relay directory must belong to the listener user and exclude other writers")
	}
	return nil
}

func Listen(ctx context.Context, path string) (*net.UnixListener, error) {
	if info, err := os.Lstat(path); err == nil {
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(stat.Uid) != os.Geteuid() || info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("Guest relay path is occupied")
		}
		probe, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", path)
		if err == nil {
			_ = probe.Close()
			return nil, fmt.Errorf("Guest relay is already listening")
		}
		if !errors.Is(err, syscall.ECONNREFUSED) {
			return nil, err
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	// The Worker validates the kernel identity of each accepted connection.
	if err := os.Chmod(path, 0o666); err != nil {
		_ = listener.Close()
		return nil, err
	}
	return listener, nil
}
