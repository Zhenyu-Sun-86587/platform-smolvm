package main

import (
	"errors"
	"net/http"

	"github.com/spf13/cobra"
)

func (c *client) vmSnapshotCommands() []*cobra.Command {
	var label string
	capture := sub("capture-vm MACHINE", "Capture a verified full VM state point", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		m, err := c.machine(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		return c.mutation(cmd, http.MethodPost, res("machines", m.ID)+"/snapshots", map[string]any{
			"expected_generation": m.Generation, "capture_mode": "vm", "purpose": "recovery", "label": label,
		}, m.Revision)
	})
	capture.Flags().StringVar(&label, "label", "", "Snapshot label")

	var restorePolicy, rollbackPolicy string
	restore := sub("restore-vm SNAPSHOT NAME", "Create a machine from a verified full VM state point", cobra.ExactArgs(2), func(cmd *cobra.Command, args []string) error {
		if err := validateVMRestore(args[0], restorePolicy); err != nil {
			return err
		}
		return c.mutation(cmd, http.MethodPost, "restores", map[string]string{
			"snapshot_id": args[0], "name": args[1], "require_capture_mode": "vm", "volume_restore_policy": restorePolicy,
		}, 0)
	})
	restore.Flags().StringVar(&restorePolicy, "volume-restore-policy", "clone_conflicts", "VM volume policy (exact or clone_conflicts)")
	rollback := sub("rollback-vm MACHINE SNAPSHOT", "Replace a machine execution from a verified full VM state point", cobra.ExactArgs(2), func(cmd *cobra.Command, args []string) error {
		if err := validateVMRestore(args[1], rollbackPolicy); err != nil {
			return err
		}
		m, err := c.machine(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		return c.mutation(cmd, http.MethodPost, res("machines", m.ID)+"/rollback", map[string]any{
			"snapshot_id": args[1], "expected_generation": m.Generation, "require_capture_mode": "vm", "volume_restore_policy": rollbackPolicy,
		}, m.Revision)
	})
	rollback.Flags().StringVar(&rollbackPolicy, "volume-restore-policy", "clone_conflicts", "VM volume policy (exact or clone_conflicts)")
	return []*cobra.Command{capture, restore, rollback}
}

func validateVMRestore(snapshot, policy string) error {
	if !validResourceID(snapshot) {
		return errors.New("invalid snapshot ID")
	}
	if policy != "exact" && policy != "clone_conflicts" {
		return errors.New("--volume-restore-policy must be exact or clone_conflicts")
	}
	return nil
}
