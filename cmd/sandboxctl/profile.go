package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
)

var profileNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

type localProfile struct {
	Origin string `json:"origin"`
	Token  string `json:"token"`
	CAFile string `json:"ca_file,omitempty"`
}

func platformOrigin(origin string) (*url.URL, error) {
	base, err := url.Parse(origin)
	if err != nil || base.Hostname() == "" || base.User != nil || base.ForceQuery || base.RawQuery != "" ||
		base.Fragment != "" || (base.Path != "" && base.Path != "/") || base.Opaque != "" {
		return nil, errors.New("platform URL must be an HTTP or HTTPS origin")
	}
	ip := net.ParseIP(base.Hostname())
	if base.Scheme != "https" && !(base.Scheme == "http" && (strings.EqualFold(base.Hostname(), "localhost") || ip.IsLoopback())) {
		return nil, errors.New("HTTPS is required for a non-loopback platform URL")
	}
	base.Host = strings.ToLower(base.Host)
	base.Path, base.RawPath = "", ""
	return base, nil
}

func (c *client) localDirectory() (string, error) {
	if c.configDir != "" {
		if !filepath.IsAbs(c.configDir) {
			return "", errors.New("configuration directory must be absolute")
		}
		return filepath.Clean(c.configDir), nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "sandboxctl"), nil
}

func (c *client) profileDirectory() (string, error) {
	if !profileNamePattern.MatchString(c.profileName) {
		return "", errors.New("invalid profile name")
	}
	dir, err := c.localDirectory()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "profiles", c.profileName), nil
}

func (c *client) readProfile() (localProfile, error) {
	dir, err := c.profileDirectory()
	if err != nil {
		return localProfile{}, err
	}
	raw, err := readPrivate(filepath.Join(dir, "profile.json"), 64<<10)
	if err != nil {
		return localProfile{}, err
	}
	var profile localProfile
	if err := json.Unmarshal(raw, &profile); err != nil {
		return localProfile{}, errors.New("invalid saved Platform profile")
	}
	base, err := platformOrigin(profile.Origin)
	if err != nil || !validPlatformToken(profile.Token) {
		return localProfile{}, errors.New("invalid saved Platform profile")
	}
	profile.Origin = base.String()
	return profile, nil
}

func (c *client) applyProfile() error {
	profile, err := c.readProfile()
	if errors.Is(err, os.ErrNotExist) && !c.explicitProfile {
		return nil
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("profile %q does not exist; run sandboxctl login", c.profileName)
		}
		return err
	}
	if !c.explicitOrigin {
		c.origin = profile.Origin
	}
	base, err := platformOrigin(c.origin)
	if err != nil {
		return err
	}
	if base.String() == profile.Origin {
		if c.token == "" {
			c.token = profile.Token
		}
		if c.caFile == "" {
			c.caFile = profile.CAFile
		}
	}
	return nil
}

// Configured OpenSSH callbacks stay pinned to their saved origin and token
// even when another shell uses SANDBOX_URL or SANDBOX_TOKEN.
func (c *client) pinnedClient() (*client, error) {
	profile, err := c.readProfile()
	if err != nil {
		return nil, err
	}
	return &client{origin: profile.Origin, token: profile.Token, caFile: profile.CAFile}, nil
}

func validPlatformToken(value string) bool {
	return value != "" && len(value) <= 8192 && strings.IndexFunc(value, func(r rune) bool {
		return r <= 0x20 || r >= 0x7f
	}) == -1
}

func (c *client) login() *cobra.Command {
	var stdin, noSSH, installSSH bool
	var sshConfig string
	cmd := sub("login", "Save a local Platform profile and configure its SSH aliases", cobra.NoArgs, func(cmd *cobra.Command, _ []string) error {
		if noSSH && cmd.Flags().Changed("install-ssh-config") && installSSH {
			return errors.New("--no-ssh-config and --install-ssh-config are mutually exclusive")
		}
		if sshConfig != "" && (noSSH || !installSSH) {
			return errors.New("--ssh-config requires alias installation")
		}
		if cmd.Flags().Changed("token") {
			return errors.New("read the token with --token-stdin or SANDBOX_TOKEN")
		}
		if stdin {
			raw, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 8194))
			if err != nil {
				return err
			}
			c.token = strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
		}
		if !validPlatformToken(c.token) {
			return errors.New("provide one Platform API token via --token-stdin or SANDBOX_TOKEN")
		}
		dir, err := c.profileDirectory()
		if err != nil {
			return err
		}
		if existing, err := c.readProfile(); err == nil {
			if !c.explicitOrigin {
				c.origin = existing.Origin
			}
			base, err := platformOrigin(c.origin)
			if err != nil {
				return err
			}
			if base.String() != existing.Origin {
				return errors.New("this profile is bound to a different origin; choose a new profile name")
			}
			if c.caFile == "" {
				c.caFile = existing.CAFile
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if c.caFile != "" {
			c.caFile, err = filepath.Abs(c.caFile)
			if err != nil {
				return err
			}
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
		defer cancel()
		c.useProfiles = false
		var control struct {
			Epoch string `json:"control_epoch"`
		}
		if err := c.get(ctx, "control", &control); err != nil {
			return err
		}
		if control.Epoch == "" {
			return errors.New("Platform login response did not contain a control epoch")
		}
		if err := ensurePrivateDirectory(dir); err != nil {
			return err
		}
		profile := localProfile{Origin: c.origin, Token: c.token, CAFile: c.caFile}
		raw, err := json.Marshal(profile)
		if err != nil {
			return err
		}
		if err := writePrivate(filepath.Join(dir, "profile.json"), raw); err != nil {
			return err
		}
		result := map[string]any{"profile": c.profileName, "origin": c.origin}
		if installSSH && !noSSH {
			path, count, err := c.syncSSHAliases(ctx, true, sshConfig)
			if err != nil {
				return fmt.Errorf("profile saved; could not configure SSH aliases: %w", err)
			}
			result["ssh_config"], result["ssh_aliases"] = path, count
		}
		return c.print(cmd, result)
	})
	cmd.Flags().BoolVar(&stdin, "token-stdin", false, "Read one Platform API token from stdin")
	cmd.Flags().BoolVar(&noSSH, "no-ssh-config", false, "Save the profile without installing OpenSSH aliases")
	cmd.Flags().BoolVar(&installSSH, "install-ssh-config", true, "Install NAME.sandbox aliases in your OpenSSH configuration")
	cmd.Flags().StringVar(&sshConfig, "ssh-config", "", "Override the OpenSSH configuration file")
	return cmd
}

func ensurePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("directory %s must be private (0700)", path)
	}
	return nil
}

func readPrivate(path string, limit int64) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > limit {
		return nil, fmt.Errorf("%s must be a bounded private regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s exceeds its size limit", path)
	}
	return data, nil
}

func writePrivate(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".sandboxctl-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
