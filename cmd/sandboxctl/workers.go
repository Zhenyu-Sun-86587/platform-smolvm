package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func (c *client) workers() *cobra.Command {
	group := &cobra.Command{Use: "workers", Short: "Inspect Worker ownership and maintenance"}
	group.AddCommand(c.list("list", "workers", false), c.showItem("workers"))
	var revision int64
	var reason string
	mode := sub("set-mode WORKER MODE", "Change the desired Worker maintenance mode", cobra.ExactArgs(2), func(cmd *cobra.Command, args []string) error {
		if revision < 1 || reason == "" {
			return fmt.Errorf("a positive --revision and --reason are required")
		}
		return c.mutation(cmd, "PATCH", res("workers", args[0])+"/maintenance", map[string]string{"mode": args[1], "reason": reason}, revision)
	})
	mode.Flags().Int64Var(&revision, "revision", 0, "Worker revision from workers show")
	mode.Flags().StringVar(&reason, "reason", "", "Reason for the maintenance change")
	group.AddCommand(mode)
	return group
}
