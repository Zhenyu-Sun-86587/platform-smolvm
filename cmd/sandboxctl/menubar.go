package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

const menuBarBundleID = "org.smolvm.sandbox-menu"

func (c *client) menuBarCommand() *cobra.Command {
	return sub("menubar", "Open the Sandbox Platform menu bar app on macOS", cobra.NoArgs, func(cmd *cobra.Command, _ []string) error {
		if runtime.GOOS != "darwin" {
			return errors.New("the Sandbox Platform menu bar app requires macOS")
		}
		if !profileNamePattern.MatchString(c.profileName) {
			return errors.New("invalid profile name")
		}
		dir, err := c.localDirectory()
		if err != nil {
			return err
		}
		args := []string{"-b", menuBarBundleID, "--args", "--profile", c.profileName, "--config-dir", dir}
		if brand := os.Getenv("SANDBOXCTL_BRAND_DIR"); brand != "" {
			path, err := filepath.Abs(brand)
			if err != nil {
				return err
			}
			args = append(args, "--brand-dir", path)
		}
		command := exec.CommandContext(cmd.Context(), "/usr/bin/open", args...)
		if output, err := command.CombinedOutput(); err != nil {
			return fmt.Errorf("open Sandbox Menu.app: %w: %s", err, strings.TrimSpace(string(output)))
		}
		return nil
	})
}

func (c *client) profileStatus() *cobra.Command {
	profiles := &cobra.Command{Use: "profile", Short: "Inspect the local Platform profile"}
	profiles.AddCommand(sub("status", "Show whether the selected profile is saved", cobra.NoArgs, func(cmd *cobra.Command, _ []string) error {
		profile, err := c.readProfile()
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return c.print(cmd, struct {
			Name   string `json:"profile"`
			Saved  bool   `json:"saved"`
			Origin string `json:"origin,omitempty"`
		}{c.profileName, err == nil, profile.Origin})
	}))
	return profiles
}
