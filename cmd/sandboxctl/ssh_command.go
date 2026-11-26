package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func (c *client) ssh() *cobra.Command {
	var user, identity string
	var batch, legacy bool
	command := sub("ssh MACHINE [-- COMMAND [ARG...]]", "Open SSH through the platform using the verified Guest host key", cobra.MinimumNArgs(1), func(cmd *cobra.Command, args []string) error {
		if len(args) > 1 && cmd.ArgsLenAtDash() != 1 {
			return errors.New("put the remote SSH command after --")
		}
		ref := args[0]
		if !safeMachineID.MatchString(ref) && !strings.HasPrefix(ref, "name:") {
			ref = "name:" + ref
		}
		id, err := c.machineID(cmd.Context(), ref)
		if err != nil {
			return err
		}
		if !safeMachineID.MatchString(id) || !safeSSHUser.MatchString(user) {
			return errors.New("invalid machine ID or SSH user")
		}
		if err := c.setup(); err != nil {
			return err
		}
		binary, err := os.Executable()
		if err != nil {
			return err
		}
		base := openSSHCommandWord(binary) + " --url " + openSSHCommandWord(c.origin)
		if c.caFile != "" {
			base += " --ca-file " + openSSHCommandWord(c.caFile)
		}
		proxy := base + " ssh-proxy --machine " + id
		managed := identity == "" && !legacy && user == "root"
		var managedKey string
		if managed {
			ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
			managed, err = c.machineManagedSSH(ctx, id)
			cancel()
			if err != nil {
				return err
			}
		}
		if managed {
			dir, err := c.persistentSSHKeyDirectory()
			if err != nil {
				return err
			}
			if dir == "" {
				dir, err = os.MkdirTemp("", "sandboxctl-managed-ssh-")
				if err != nil {
					return err
				}
				defer os.RemoveAll(dir)
			}
			managedKey, err = ensureManagedSSHIdentity(cmd.Context(), dir)
			if err != nil {
				return err
			}
			proxy += " --managed --managed-key-file " + openSSHCommandWord(managedKey)
		}
		options := []string{
			"-F", os.DevNull,
			"-o", "StrictHostKeyChecking=yes",
			"-o", "CheckHostIP=no",
			"-o", "UpdateHostKeys=no",
			"-o", "GlobalKnownHostsFile=" + os.DevNull,
			"-o", "UserKnownHostsFile=" + os.DevNull,
			"-o", "HostKeyAlias=sandbox-" + id,
			"-o", "ProxyCommand=" + proxy,
			// OpenSSH first queries key ordering and queries the host again
			// during verification. The proxy may wake the Guest between them.
			"-o", "KnownHostsCommand=" + base + " ssh-host-key --if-ready " + id,
			"-l", user,
		}
		if managed {
			options = append(options, "-i", strings.ReplaceAll(managedKey, "%", "%%"),
				"-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none",
				"-o", "ConnectTimeout=90", "-o", "PreferredAuthentications=publickey")
		}
		if identity != "" {
			options = append(options, "-i", identity, "-o", "IdentitiesOnly=yes")
		}
		if batch || managed {
			options = append(options, "-o", "BatchMode=yes")
		}
		options = append(options, "--", id)
		options = append(options, args[1:]...)
		process := exec.CommandContext(cmd.Context(), "ssh", options...)
		process.Env = append(os.Environ(), "SANDBOX_TOKEN="+c.token)
		process.Stdin, process.Stdout, process.Stderr = cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()
		if err := process.Run(); err != nil {
			var status *exec.ExitError
			if errors.As(err, &status) && status.ExitCode() > 0 {
				return exitStatus(status.ExitCode())
			}
			return err
		}
		return nil
	})
	command.Flags().StringVar(&user, "user", "root", "Guest SSH user")
	command.Flags().StringVar(&identity, "identity", "", "Use an enrolled OpenSSH private key on the legacy tunnel")
	command.Flags().BoolVar(&legacy, "legacy", false, "Use enrolled keys or an SSH agent on the published SSH service")
	command.Flags().BoolVar(&batch, "batch-mode", false, "Disable OpenSSH password and confirmation prompts")
	return command
}

const managedSSHCapabilityName = "access.managed_ssh"

type managedSSHCapability struct {
	Status      string `json:"status"`
	Constraints struct {
		ManageAllowed *bool `json:"manage_allowed"`
	} `json:"constraints"`
}

func (c *client) machineManagedSSH(ctx context.Context, id string) (bool, error) {
	var current struct {
		ID           string                          `json:"machine_id"`
		Capabilities map[string]managedSSHCapability `json:"capabilities"`
	}
	if err := c.get(ctx, res("machines", id), &current); err != nil {
		return false, err
	}
	if current.ID != id {
		return false, errors.New("platform returned a different machine ID")
	}
	capability, present := current.Capabilities[managedSSHCapabilityName]
	if !present {
		// Older servers omit this capability from the authenticated machine view.
		return false, nil
	}
	if capability.Constraints.ManageAllowed == nil {
		return false, errors.New("platform did not confirm manage permission for SSH")
	}
	if !*capability.Constraints.ManageAllowed {
		return false, errors.New("managed SSH requires manage permission for this machine")
	}
	switch capability.Status {
	case "supported":
		return true, nil
	case "unsupported":
		return false, nil
	default:
		return false, fmt.Errorf("managed SSH support is unverified for %s; use --legacy to select a published SSH service", id)
	}
}

func (c *client) persistentSSHKeyDirectory() (string, error) {
	profile, err := c.readProfile()
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if profile.Origin != c.origin {
		return "", nil
	}
	dir, err := c.profileDirectory()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "ssh"), nil
}

func openSSHCommandWord(value string) string {
	return shellWord(strings.ReplaceAll(value, "%", "%%"))
}
