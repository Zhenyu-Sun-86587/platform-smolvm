package main

import "github.com/spf13/cobra"

func (c *client) backups() *cobra.Command {
	group := &cobra.Command{Use: "backups", Short: "Inspect scheduled backups and start a configured policy"}
	group.AddCommand(c.list("list", "backups", false), c.showItem("backups"), c.list("policies", "backups/policies", false))
	group.AddCommand(sub("run POLICY", "Start a configured backup policy", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		return c.mutation(cmd, "POST", res("backups/policies", args[0])+"/runs", struct{}{}, 0)
	}))
	return group
}
