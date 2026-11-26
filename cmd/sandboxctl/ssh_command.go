package main

import (
	"errors"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

func (c *client) ssh() *cobra.Command {
	var user, identity string
	var batch bool
	command := sub("ssh MACHINE [-- COMMAND [ARG...]]", "Open SSH through the platform using the verified Guest host key", cobra.MinimumNArgs(1), func(cmd *cobra.Command, args []string) error {
		if len(args) > 1 && cmd.ArgsLenAtDash() != 1 {
			return errors.New("put the remote SSH command after --")
		}
		id, err := c.machineID(cmd.Context(), args[0])
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
		options := []string{
			"-F", os.DevNull,
			"-o", "StrictHostKeyChecking=yes",
			"-o", "CheckHostIP=no",
			"-o", "UpdateHostKeys=no",
			"-o", "GlobalKnownHostsFile=" + os.DevNull,
			"-o", "UserKnownHostsFile=" + os.DevNull,
			"-o", "HostKeyAlias=sandbox-" + id,
			"-o", "ProxyCommand=" + base + " ssh-proxy --machine " + id,
			// OpenSSH first queries key ordering and queries the host again
			// during verification. The proxy may wake the Guest between them.
			"-o", "KnownHostsCommand=" + base + " ssh-host-key --if-ready " + id,
			"-l", user,
		}
		if identity != "" {
			options = append(options, "-i", identity, "-o", "IdentitiesOnly=yes")
		}
		if batch {
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
	command.Flags().StringVar(&identity, "identity", "", "OpenSSH private key file")
	command.Flags().BoolVar(&batch, "batch-mode", false, "Disable OpenSSH password and confirmation prompts")
	return command
}

func openSSHCommandWord(value string) string {
	return shellWord(strings.ReplaceAll(value, "%", "%%"))
}
