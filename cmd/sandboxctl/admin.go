package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"sandbox-platform/internal/config"
	"sandbox-platform/internal/diagnostics"
	"sandbox-platform/internal/maintenance"
)

const localConfigPath = "/etc/sandbox-platform/config.yaml"
const installReleaseScript = "/opt/sandbox-platform/libexec/install-release"

func registerLocalCommands(root *cobra.Command) {
	root.AddCommand(adminCommand(), doctorCommand(), diagnosticsCommand())
}

func configFlag() string {
	if path := os.Getenv("SANDBOX_CONFIG"); path != "" {
		return path
	}
	return localConfigPath
}

func writeLocalResult(cmd *cobra.Command, value any) error {
	e := json.NewEncoder(cmd.OutOrStdout())
	if compact, _ := cmd.Flags().GetBool("json"); !compact {
		e.SetIndent("", "  ")
	}
	return e.Encode(value)
}

func adminCommand() *cobra.Command {
	var path string
	root := &cobra.Command{Use: "admin", Short: "Maintain the local platform installation"}
	root.PersistentFlags().StringVar(&path, "config", configFlag(), "Local operator configuration")
	cfg := func() (config.Config, error) { return config.Load(path) }
	backup := &cobra.Command{Use: "backup", Short: "Create, verify, or restore local backups"}
	var output, verifyArchive, restoreArchive, rollback string
	create := &cobra.Command{Use: "create", Short: "Copy a consistent registry and its ready artifacts", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := cfg()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Minute)
			defer cancel()
			b, err := maintenance.CreateBackup(ctx, c, path, output)
			if err != nil {
				return err
			}
			return writeLocalResult(cmd, b)
		}}
	create.Flags().StringVar(&output, "output", "", "New output directory")
	_ = create.MarkFlagRequired("output")
	verify := &cobra.Command{Use: "verify", Short: "Verify backup checksums, registry metadata, and artifact signatures", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := cfg()
			if err != nil {
				return err
			}
			b, err := maintenance.VerifyBackup(cmd.Context(), c, verifyArchive)
			if err != nil {
				return err
			}
			return writeLocalResult(cmd, b)
		}}
	verify.Flags().StringVar(&verifyArchive, "archive", "", "Backup directory")
	_ = verify.MarkFlagRequired("archive")
	restore := &cobra.Command{Use: "restore", Short: "Restore a same-installation backup while the daemon is stopped", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := cfg()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Minute)
			defer cancel()
			b, err := maintenance.RestoreBackup(ctx, c, path, restoreArchive, rollback)
			if err != nil {
				return err
			}
			return writeLocalResult(cmd, b)
		}}
	restore.Flags().StringVar(&restoreArchive, "archive", "", "Backup directory")
	restore.Flags().StringVar(&rollback, "rollback-output", "", "New directory for the pre-restore backup")
	_ = restore.MarkFlagRequired("archive")
	_ = restore.MarkFlagRequired("rollback-output")
	backup.AddCommand(create, verify, restore)

	maintenanceCommand := &cobra.Command{Use: "maintenance", Short: "Set the local maintenance gate with the daemon stopped"}
	var reason string
	change := func(enabled bool) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, _ []string) error {
			c, err := cfg()
			if err != nil {
				return err
			}
			control, err := maintenance.SetMaintenance(cmd.Context(), c, enabled, reason)
			if err != nil {
				return err
			}
			return writeLocalResult(cmd, control)
		}
	}
	enter := &cobra.Command{Use: "enter", Short: "Enable the maintenance gate", Args: cobra.NoArgs, RunE: change(true)}
	enter.Flags().StringVar(&reason, "reason", "", "Operator reason")
	_ = enter.MarkFlagRequired("reason")
	maintenanceCommand.AddCommand(enter, &cobra.Command{Use: "exit", Short: "Disable the maintenance gate after operator review", Args: cobra.NoArgs, RunE: change(false)})
	root.AddCommand(backup, maintenanceCommand, upgradeCommand(&path))
	return root
}

func upgradeCommand(path *string) *cobra.Command {
	root := &cobra.Command{Use: "upgrade", Short: "Check and apply a pinned platform release"}
	var planManifest, applyManifest, archive string
	plan := &cobra.Command{Use: "plan", Short: "Check release assets and host prerequisites", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runReleaseScript(cmd, "--plan", "--manifest", planManifest, "--config", *path)
		}}
	plan.Flags().StringVar(&planManifest, "manifest", "", "Release manifest path")
	_ = plan.MarkFlagRequired("manifest")
	validate := func(ctx context.Context, archive string) error {
		if filepath.Clean(*path) != localConfigPath {
			return errors.New("release changes require the installed service configuration")
		}
		cfg, err := config.Load(*path)
		if err != nil {
			return err
		}
		control, err := maintenance.CurrentControl(ctx, cfg)
		if err != nil {
			return err
		}
		if !control.Maintenance {
			return errors.New("enter local maintenance before changing a release")
		}
		backup, err := maintenance.VerifyBackup(ctx, cfg, archive)
		if err != nil {
			return err
		}
		if backup.InstallationID != control.InstallationID || backup.ControlEpoch != control.ControlEpoch ||
			backup.SchemaVersion != control.SchemaVersion {
			return errors.New("backup does not match the current installation, control epoch, and schema")
		}
		return nil
	}
	apply := &cobra.Command{Use: "apply", Short: "Install a verified release in maintenance mode", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validate(cmd.Context(), archive); err != nil {
				return err
			}
			return runReleaseScript(cmd, "--upgrade", "--manifest", applyManifest, "--backup", archive)
		}}
	apply.Flags().StringVar(&applyManifest, "manifest", "", "Release manifest path")
	apply.Flags().StringVar(&archive, "backup", "", "Verified backup from this installation and epoch")
	_ = apply.MarkFlagRequired("manifest")
	_ = apply.MarkFlagRequired("backup")
	var rollbackVersion, rollbackArchive string
	rollback := &cobra.Command{Use: "rollback", Short: "Select a compatible installed release in maintenance mode", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validate(cmd.Context(), rollbackArchive); err != nil {
				return err
			}
			return runReleaseScript(cmd, "--rollback", rollbackVersion)
		}}
	rollback.Flags().StringVar(&rollbackVersion, "to", "", "Installed release version")
	rollback.Flags().StringVar(&rollbackArchive, "backup", "", "Verified backup from this installation and epoch")
	_ = rollback.MarkFlagRequired("to")
	_ = rollback.MarkFlagRequired("backup")
	root.AddCommand(plan, apply, rollback)
	return root
}

func runReleaseScript(cmd *cobra.Command, args ...string) error {
	command := exec.CommandContext(cmd.Context(), installReleaseScript, args...)
	command.Stdout = cmd.OutOrStdout()
	command.Stderr = cmd.ErrOrStderr()
	command.Stdin = cmd.InOrStdin()
	return command.Run()
}

func doctorCommand() *cobra.Command {
	var path string
	cmd := &cobra.Command{Use: "doctor", Short: "Inspect local runtime, configuration, and host readiness", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(path)
			if err != nil {
				return err
			}
			return writeLocalResult(cmd, diagnostics.Collect(cmd.Context(), cfg))
		}}
	cmd.Flags().StringVar(&path, "config", configFlag(), "Local operator configuration")
	return cmd
}

func diagnosticsCommand() *cobra.Command {
	root := &cobra.Command{Use: "diagnostics", Short: "Collect a local diagnostics report"}
	var path, output string
	export := &cobra.Command{Use: "export", Short: "Save allowlisted diagnostics to a ZIP file", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(path)
			if err != nil {
				return err
			}
			report := diagnostics.Collect(cmd.Context(), cfg)
			if err := diagnostics.WriteZip(output, report); err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), output)
			return err
		}}
	export.Flags().StringVar(&path, "config", configFlag(), "Local operator configuration")
	export.Flags().StringVar(&output, "output", "", "New ZIP file path")
	_ = export.MarkFlagRequired("output")
	root.AddCommand(export)
	return root
}
