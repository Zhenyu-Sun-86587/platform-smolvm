package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
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
		var credentials sshCredentialPaths
		if managed {
			dir, err := os.MkdirTemp("", "sandboxctl-managed-ssh-")
			if err != nil {
				return err
			}
			defer os.RemoveAll(dir)
			ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
			credentials, err = c.prepareSSHCredential(ctx, dir, id)
			if err != nil {
				managed = false
				if !c.canUseLegacySSH(ctx, id, err) {
					cancel()
					return err
				}
			}
			cancel()
			if managed {
				proxy += " --managed"
			}
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
			options = append(options, "-i", credentials.identity,
				"-o", "CertificateFile="+sshConfigArg(strings.ReplaceAll(credentials.certificate, "%", "%%")), "-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none",
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

func (c *client) canUseLegacySSH(ctx context.Context, id string, err error) bool {
	var upstream *apiError
	if !errors.As(err, &upstream) {
		return false
	}
	if upstream.Status == http.StatusConflict && upstream.Detail.Code == "NOT_SUPPORTED" {
		return upstream.Detail.Effect == "none"
	}
	if (upstream.Status != http.StatusNotFound && upstream.Status != http.StatusMethodNotAllowed) || upstream.Detail.Code != "" {
		return false
	}
	// Old Go muxes return plain 404/405 for the missing certificate method.
	// Independently verify that the same authenticated machine is visible.
	var current machine
	return c.get(ctx, res("machines", id), &current) == nil && current.ID == id
}

func openSSHCommandWord(value string) string {
	return shellWord(strings.ReplaceAll(value, "%", "%%"))
}
