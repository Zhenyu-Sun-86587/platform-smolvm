package main

import (
	"context"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"
)

func (c *client) volumeRevision(ctx context.Context, id string) (int64, error) {
	var volume struct {
		Revision int64 `json:"revision"`
	}
	err := c.get(ctx, res("volumes", id), &volume)
	return volume.Revision, err
}

func (c *client) volumes() *cobra.Command {
	group := &cobra.Command{Use: "volumes", Short: "Manage independent block volumes"}
	group.AddCommand(c.list("list", "volumes", false), c.showItem("volumes"),
		c.update("create", "volumes", "POST", false), c.update("delete VOLUME", "volumes", "DELETE", true))
	group.AddCommand(sub("grow VOLUME CAPACITY_BYTES", "Increase the block capacity", cobra.ExactArgs(2), func(cmd *cobra.Command, args []string) error {
		capacity, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil || capacity < 512 || capacity%512 != 0 {
			return fmt.Errorf("capacity must be a positive whole number of 512-byte sectors")
		}
		revision := c.revision
		if revision == 0 {
			revision, err = c.volumeRevision(cmd.Context(), args[0])
			if err != nil {
				return err
			}
		}
		return c.mutation(cmd, "PATCH", res("volumes", args[0]), map[string]any{"capacity_bytes": capacity}, revision)
	}))
	group.AddCommand(c.volumeSnapshots(), c.volumeAttachments())
	return group
}

func (c *client) volumeSnapshots() *cobra.Command {
	group := &cobra.Command{Use: "snapshots", Short: "Manage immutable volume snapshots"}
	group.AddCommand(c.showItem("volume-snapshots"))
	group.AddCommand(sub("list VOLUME", "List volume snapshots", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		return c.show(cmd, res("volumes", args[0])+"/snapshots")
	}))
	group.AddCommand(sub("create VOLUME", "Capture a volume", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		revision := c.revision
		if revision == 0 {
			var err error
			revision, err = c.volumeRevision(cmd.Context(), args[0])
			if err != nil {
				return err
			}
		}
		return c.mutation(cmd, "POST", res("volumes", args[0])+"/snapshots", map[string]any{}, revision)
	}))
	group.AddCommand(sub("delete SNAPSHOT", "Release a volume snapshot", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		path := res("volume-snapshots", args[0])
		revision := c.revision
		if revision == 0 {
			var snapshot struct {
				VolumeRevision int64 `json:"volume_revision"`
			}
			if err := c.get(cmd.Context(), path, &snapshot); err != nil {
				return err
			}
			revision = snapshot.VolumeRevision
		}
		return c.mutation(cmd, "DELETE", path, map[string]any{}, revision)
	}))
	group.AddCommand(sub("clone SNAPSHOT NAME", "Create an independent volume from a snapshot", cobra.ExactArgs(2), func(cmd *cobra.Command, args []string) error {
		return c.mutation(cmd, "POST", res("volume-snapshots", args[0])+"/clones", map[string]any{"name": args[1]}, 0)
	}))
	return group
}

func (c *client) volumeAttachments() *cobra.Command {
	group := &cobra.Command{Use: "attachments", Short: "Manage offline volume attachments"}
	group.AddCommand(c.list("list MACHINE", "volume-attachments", true))
	var mode, snapshot, fork, capture string
	var attachRevision, detachRevision int64
	attach := sub("attach MACHINE VOLUME DEVICE", "Reserve a volume on a stopped machine", cobra.ExactArgs(3), func(cmd *cobra.Command, args []string) error {
		scope, revision, err := c.attachmentScope(cmd.Context(), args[0], args[1], attachRevision)
		if err != nil {
			return err
		}
		scope["volume_id"], scope["device_key"], scope["mode"] = args[1], args[2], mode
		scope["snapshot_id"], scope["fork_policy"], scope["capture_policy"] = snapshot, fork, capture
		return c.mutation(cmd, "POST", res("machines", args[0])+"/volume-attachments", scope, revision)
	})
	attach.Flags().StringVar(&mode, "mode", "rw", "Volume access mode")
	attach.Flags().StringVar(&snapshot, "snapshot", "", "Immutable snapshot ID for read-only access")
	attach.Flags().StringVar(&fork, "fork-policy", "", "Fork policy for the attachment")
	attach.Flags().StringVar(&capture, "capture-policy", "include", "Capture policy for the attachment")
	attach.Flags().Int64Var(&attachRevision, "volume-revision", 0, "Original volume revision for replay")
	detach := sub("detach MACHINE VOLUME ATTACHMENT", "Remove a volume from a stopped machine", cobra.ExactArgs(3), func(cmd *cobra.Command, args []string) error {
		scope, revision, err := c.attachmentScope(cmd.Context(), args[0], args[1], detachRevision)
		if err != nil {
			return err
		}
		return c.mutation(cmd, "DELETE", res("volume-attachments", args[2]), scope, revision)
	})
	detach.Flags().Int64Var(&detachRevision, "volume-revision", 0, "Original volume revision for replay")
	group.AddCommand(attach, detach)
	return group
}

func (c *client) attachmentScope(ctx context.Context, machineID, volumeID string, volumeRevision int64) (map[string]any, int64, error) {
	machineRevision, generation := c.revision, c.generation
	if machineRevision == 0 || generation == 0 {
		m, err := c.machine(ctx, machineID)
		if err != nil {
			return nil, 0, err
		}
		if machineRevision == 0 {
			machineRevision = m.Revision
		}
		if generation == 0 {
			generation = m.Generation
		}
	}
	if volumeRevision == 0 {
		var err error
		volumeRevision, err = c.volumeRevision(ctx, volumeID)
		if err != nil {
			return nil, 0, err
		}
	}
	return map[string]any{"expected_volume_revision": volumeRevision, "expected_generation": generation}, machineRevision, nil
}
