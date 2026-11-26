package main

import (
	"fmt"
	"net/http"

	"github.com/spf13/cobra"
)

func (c *client) guardedPower() []*cobra.Command {
	var from string
	wake := sub("wake-full MACHINE", "Wake the specified preserved native execution", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		switch from {
		case "standby":
			return c.guardedPowerChange(cmd, args[0], "wake", from)
		case "hibernated":
			return c.guardedPowerChange(cmd, args[0], "start", from)
		default:
			return fmt.Errorf("--from must be standby or hibernated")
		}
	})
	wake.Flags().StringVar(&from, "from", "", "Confirmed source power state (standby or hibernated)")
	_ = wake.MarkFlagRequired("from")
	start := sub("cold-start MACHINE", "Boot a stopped machine from persistent disks", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		return c.guardedPowerChange(cmd, args[0], "start", "stopped")
	})
	return []*cobra.Command{wake, start}
}

func (c *client) guardedPowerChange(cmd *cobra.Command, ref, action, state string) error {
	m, err := c.machine(cmd.Context(), ref)
	if err != nil {
		return err
	}
	return c.mutation(cmd, http.MethodPost, res("machines", m.ID)+"/"+action, map[string]any{
		"expected_generation": m.Generation, "expected_power_state": state,
	}, m.Revision)
}
