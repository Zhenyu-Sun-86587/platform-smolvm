package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
)

const aliasSuffix = ".sandbox"

var sshAliasLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

type sshAlias struct {
	name, machine string
}

func (c *client) visibleSSHAliases(ctx context.Context) ([]sshAlias, error) {
	aliases := map[string]string{}
	machines := map[string]string{}
	ambiguous := map[string]bool{}
	cursors := map[string]bool{}
	for cursor := ""; ; {
		var page struct {
			Items []struct {
				ID           string `json:"machine_id"`
				Name         string `json:"name"`
				BootMode     string `json:"boot_mode"`
				Capabilities map[string]struct {
					Status      string `json:"status"`
					Constraints struct {
						ManageAllowed bool `json:"manage_allowed"`
					} `json:"constraints"`
				} `json:"capabilities"`
			} `json:"items"`
			Next string `json:"next_cursor"`
		}
		query := url.Values{"cursor": {cursor}, "limit": {"100"}}
		if err := c.get(ctx, "machines?"+query.Encode(), &page); err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			label := strings.ToLower(item.Name)
			if !sshAliasLabel.MatchString(label) || !safeMachineID.MatchString(item.ID) {
				continue
			}
			host := label + aliasSuffix
			// Reserve collisions even if one entry is not a System machine.
			if previous, exists := machines[host]; exists && previous != item.ID {
				ambiguous[host] = true
			}
			machines[host] = item.ID
			managed, published := item.Capabilities["access.managed_ssh"]
			if item.BootMode == "system" && (!published || managed.Status == "supported" && managed.Constraints.ManageAllowed) {
				aliases[host] = item.ID
			}
		}
		if page.Next == "" {
			break
		}
		if cursors[page.Next] {
			return nil, errors.New("platform repeated a machine inventory cursor")
		}
		cursors[page.Next] = true
		cursor = page.Next
	}
	result := make([]sshAlias, 0, len(aliases))
	for host, machine := range aliases {
		if machine != "" && !ambiguous[host] {
			result = append(result, sshAlias{host, machine})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].name < result[j].name })
	return result, nil
}

func (c *client) requireSSHAlias(ctx context.Context, alias, machine string) error {
	label, allowed := strings.CutSuffix(alias, aliasSuffix)
	if !allowed || !sshAliasLabel.MatchString(label) || !safeMachineID.MatchString(machine) {
		return errors.New("invalid managed SSH alias")
	}
	aliases, err := c.visibleSSHAliases(ctx)
	if err != nil {
		return err
	}
	for _, current := range aliases {
		if current.name == alias && current.machine == machine {
			return nil
		}
	}
	return fmt.Errorf("SSH alias %s no longer identifies its configured machine; run sandboxctl ssh-config sync", alias)
}

func (c *client) sshConfigSync() *cobra.Command {
	var install bool
	var target string
	cmd := sub("sync", "Refresh managed NAME.sandbox aliases from the selected profile", cobra.NoArgs, func(cmd *cobra.Command, _ []string) error {
		ctx, cancel := context.WithTimeout(cmd.Context(), 25*time.Second)
		defer cancel()
		if target != "" && !install {
			return errors.New("--ssh-config requires --install")
		}
		path, count, err := c.syncSSHAliases(ctx, install, target)
		if err != nil {
			return err
		}
		return c.print(cmd, map[string]any{"profile": c.profileName, "ssh_config": path, "ssh_aliases": count})
	})
	cmd.Flags().BoolVar(&install, "install", false, "Install the generated Include in the user's OpenSSH config")
	cmd.Flags().StringVar(&target, "ssh-config", "", "Override the OpenSSH config file used by --install")
	return cmd
}

func (c *client) syncSSHAliases(ctx context.Context, install bool, target string) (string, int, error) {
	pinned, err := c.pinnedClient()
	if err != nil {
		return "", 0, err
	}
	dir, err := c.localDirectory()
	if err != nil {
		return "", 0, err
	}
	if err := ensurePrivateDirectory(dir); err != nil {
		return "", 0, err
	}
	lock, err := sshFileLock(ctx, filepath.Join(dir, ".ssh-config.lock"))
	if err != nil {
		return "", 0, err
	}
	defer lock.Close()

	marker := filepath.Join(dir, "installed-ssh.json")
	generated := filepath.Join(dir, "managed-ssh.conf")
	if install && target == "" {
		ownerDir, err := os.UserHomeDir()
		if err != nil {
			return "", 0, err
		}
		target = filepath.Join(ownerDir, ".ssh", "config")
	}
	if !install {
		state, err := readInstalledSSH(marker)
		if err != nil {
			return "", 0, err
		}
		target = state.Target
	}
	count, err := c.writeSSHAliases(ctx, pinned, dir, generated)
	if err != nil {
		return "", 0, err
	}
	if install {
		if err := installSSHInclude(ctx, target, generated); err != nil {
			return "", 0, err
		}
	}
	if target != "" {
		state := installedSSH{Profile: c.profileName, Target: target}
		raw, err := json.Marshal(state)
		if err != nil {
			return "", 0, err
		}
		if err := writePrivate(marker, raw); err != nil {
			return "", 0, err
		}
	}
	return generated, count, nil
}

type installedSSH struct {
	Profile string `json:"profile"`
	Target  string `json:"target"`
}

func readInstalledSSH(marker string) (installedSSH, error) {
	var state installedSSH
	raw, err := readPrivate(marker, 16<<10)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if json.Unmarshal(raw, &state) != nil || !profileNamePattern.MatchString(state.Profile) || !filepath.IsAbs(state.Target) {
		return installedSSH{}, errors.New("invalid installed SSH configuration record")
	}
	return state, nil
}

func (c *client) writeSSHAliases(ctx context.Context, pinned *client, dir, generated string) (int, error) {
	aliases, err := pinned.visibleSSHAliases(ctx)
	if err != nil {
		return 0, err
	}
	profile, err := c.profileDirectory()
	if err != nil {
		return 0, err
	}
	binary, err := os.Executable()
	if err != nil {
		return 0, err
	}
	config, err := managedSSHConfig(binary, dir, c.profileName, filepath.Join(profile, "ssh"), aliases)
	if err != nil {
		return 0, err
	}
	return len(aliases), writePrivate(generated, []byte(config))
}

func (c *client) refreshSSHAliases(cmd *cobra.Command) {
	if !c.useProfiles {
		return
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
	defer cancel()
	if err := c.refreshInstalledSSH(ctx); err != nil {
		warning := "could not refresh managed SSH aliases; run sandboxctl ssh-config sync: " + err.Error()
		if c.jsonOutput {
			_ = json.NewEncoder(cmd.ErrOrStderr()).Encode(map[string]string{"warning": warning})
		} else {
			fmt.Fprintln(cmd.ErrOrStderr(), warning)
		}
	}
}

func (c *client) refreshInstalledSSH(ctx context.Context) error {
	dir, err := c.localDirectory()
	if err != nil {
		// An explicit URL/token can be used without any local configuration.
		return nil
	}
	marker := filepath.Join(dir, "installed-ssh.json")
	state, err := readInstalledSSH(marker)
	if err != nil || state.Profile != c.profileName {
		return err
	}
	lock, err := sshFileLock(ctx, filepath.Join(dir, ".ssh-config.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	state, err = readInstalledSSH(marker)
	if err != nil || state.Profile != c.profileName {
		return err
	}
	generated := filepath.Join(dir, "managed-ssh.conf")
	target, err := readSSHConfig(state.Target)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !strings.Contains(string(target), sshInclude(generated)) {
		return err
	}
	pinned, err := c.pinnedClient()
	if err != nil {
		return err
	}
	if pinned.origin != c.origin {
		return nil
	}
	_, err = c.writeSSHAliases(ctx, pinned, dir, generated)
	return err
}

func managedSSHConfig(binary, dir, profile, keyDir string, aliases []sshAlias) (string, error) {
	for _, value := range []string{binary, dir, keyDir} {
		if !filepath.IsAbs(value) || strings.ContainsAny(value, "\r\n\x00") || strings.Contains(value, "${") {
			return "", errors.New("OpenSSH integration requires absolute paths without newlines or environment substitutions")
		}
	}
	if !profileNamePattern.MatchString(profile) {
		return "", errors.New("invalid profile name")
	}
	exec := openSSHCommandWord(binary) + " --config-dir " + openSSHCommandWord(dir) + " --profile " + profile
	var config strings.Builder
	config.WriteString("# Generated by sandboxctl ssh-config sync.\n")
	for _, alias := range aliases {
		label, ok := strings.CutSuffix(alias.name, aliasSuffix)
		if !ok || !sshAliasLabel.MatchString(label) || !safeMachineID.MatchString(alias.machine) {
			return "", errors.New("unsafe OpenSSH alias")
		}
		identity := credentialPaths(keyDir, alias.machine)
		proxy := exec + " managed-ssh-proxy " + alias.machine + " " + alias.name
		hostKey := exec + " managed-ssh-host-key " + alias.machine
		prepare := exec + " managed-ssh-prepare " + alias.machine + " " + alias.name
		fmt.Fprintf(&config, `Host %s
    HostName %s
    User root
    ConnectTimeout 90
    BatchMode yes
    CanonicalizeHostname no
    IdentityAgent none
    IdentitiesOnly yes
    IdentityFile %s
    CertificateFile %s
    PreferredAuthentications publickey
    StrictHostKeyChecking yes
    CheckHostIP no
    UpdateHostKeys no
    UserKnownHostsFile /dev/null
    GlobalKnownHostsFile /dev/null
    HostKeyAlias sandbox-%s
    ProxyCommand %s
    KnownHostsCommand %s
Match originalhost %s exec %s

`, alias.name, alias.name, sshConfigArg(strings.ReplaceAll(identity.identity, "%", "%%")),
			sshConfigArg(strings.ReplaceAll(identity.certificate, "%", "%%")), alias.machine,
			proxy, hostKey, alias.name, sshConfigArg(prepare))
	}
	fmt.Fprintf(&config, "Host *.sandbox\n    ProxyCommand %s managed-ssh-missing-alias\nHost *\n", exec)
	return config.String(), nil
}

// SSH config argv parsing precedes shell parsing of Match exec. Quote the
// entire expression for the config parser; the command's arguments are
// already safely quoted for the shell and use only static host literals.
func sshConfigArg(value string) string {
	return "\"" + strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(value) + "\""
}

func installSSHInclude(ctx context.Context, target, include string) error {
	if !filepath.IsAbs(target) {
		return errors.New("OpenSSH config path must be absolute")
	}
	if strings.ContainsAny(include, "*?[]%") {
		return errors.New("OpenSSH Include path must be a literal filename")
	}
	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(parent)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0o022 != 0 || !ownsSSHConfig(info) {
		return errors.New("OpenSSH config directory must be owned by the current user and protected from other writers")
	}
	lock, err := sshFileLock(ctx, target+".sandboxctl.lock")
	if err != nil {
		return err
	}
	defer lock.Close()
	raw, err := readSSHConfig(target)
	existed := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	prefix := sshInclude(include)
	if strings.Contains(string(raw), prefix) {
		return nil
	}
	return writePrivateChecked(target, append([]byte(prefix), raw...), func() error {
		current, err := readSSHConfig(target)
		if !existed && errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil || !existed || !bytes.Equal(current, raw) {
			return errors.New("OpenSSH config changed while installing aliases; rerun sandboxctl ssh-config sync --install")
		}
		return nil
	})
}

func sshInclude(path string) string {
	return "# sandboxctl managed aliases\nInclude " + sshConfigArg(path) + "\n"
}

func ownsSSHConfig(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

func readSSHConfig(path string) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 || info.Size() > 1<<20 || !ownsSSHConfig(info) {
		return nil, errors.New("OpenSSH config must be a regular file owned by the current user without group or other write permission")
	}
	raw, err := io.ReadAll(io.LimitReader(file, 1<<20+1))
	if len(raw) > 1<<20 {
		return nil, errors.New("OpenSSH config exceeds 1 MiB")
	}
	return raw, err
}

func (c *client) managedSSHPrepare() *cobra.Command {
	cmd := sub("managed-ssh-prepare MACHINE ALIAS", "Prepare an installed OpenSSH alias", cobra.ExactArgs(2), func(cmd *cobra.Command, args []string) error {
		pinned, err := c.pinnedClient()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
		defer cancel()
		if err := pinned.requireSSHAlias(ctx, args[1], args[0]); err != nil {
			return err
		}
		dir, err := c.profileDirectory()
		if err != nil {
			return err
		}
		_, err = pinned.prepareSSHCredential(ctx, filepath.Join(dir, "ssh"), args[0])
		return err
	})
	cmd.Hidden = true
	return cmd
}

func (c *client) managedSSHProxy() *cobra.Command {
	cmd := sub("managed-ssh-proxy MACHINE ALIAS", "Proxy an installed OpenSSH alias", cobra.ExactArgs(2), func(cmd *cobra.Command, args []string) error {
		pinned, err := c.pinnedClient()
		if err != nil {
			return err
		}
		verify, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
		defer cancel()
		if err := pinned.requireSSHAlias(verify, args[1], args[0]); err != nil {
			return err
		}
		return pinned.proxySSH(cmd, args[0], true)
	})
	cmd.Hidden = true
	return cmd
}

func (c *client) managedSSHHostKey() *cobra.Command {
	cmd := sub("managed-ssh-host-key MACHINE", "Read a host pin for an installed OpenSSH alias", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		pinned, err := c.pinnedClient()
		if err != nil {
			return err
		}
		if !safeMachineID.MatchString(args[0]) {
			return errors.New("invalid machine ID")
		}
		return pinned.printSSHHostKey(cmd, args[0], true)
	})
	cmd.Hidden = true
	return cmd
}

func missingSSHAlias() *cobra.Command {
	cmd := sub("managed-ssh-missing-alias", "Refuse an unsynced .sandbox alias", cobra.NoArgs, func(_ *cobra.Command, _ []string) error {
		return errors.New("managed SSH alias is not configured; run sandboxctl ssh-config sync")
	})
	cmd.Hidden = true
	return cmd
}
