package main

import "github.com/spf13/cobra"

func (c *client) desktops() *cobra.Command {
	group := &cobra.Command{Use: "desktop", Short: "Manage authorized VNC desktop sessions"}
	group.AddCommand(c.list("list MACHINE", "desktop-sessions", true), c.showItem("desktop-sessions"))
	var ttl int64
	create := sub("create MACHINE SERVICE", "Create a desktop session and its activity lease", cobra.ExactArgs(2), func(cmd *cobra.Command, args []string) error {
		machine, err := c.machine(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		return c.mutation(cmd, "POST", res("machines", machine.ID)+"/desktop-sessions", map[string]any{
			"service_id": args[1], "expected_generation": machine.Generation, "ttl_seconds": ttl,
		}, machine.Revision)
	})
	create.Flags().Int64Var(&ttl, "ttl-seconds", 900, "Desktop session lifetime in seconds")
	group.AddCommand(create)
	group.AddCommand(sub("close SESSION", "Close a desktop session and release its lease", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		return c.mutation(cmd, "DELETE", res("desktop-sessions", args[0]), struct{}{}, 0)
	}))
	group.AddCommand(sub("proxy SESSION", "Forward the session's VNC stream over standard input and output", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		return c.tunnel(cmd.Context(), res("desktop-sessions", args[0])+"/tunnel", cmd.InOrStdin(), cmd.OutOrStdout(), func() error { return nil })
	}))
	return group
}
