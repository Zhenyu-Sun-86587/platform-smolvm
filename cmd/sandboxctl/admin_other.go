//go:build !linux

package main

import "github.com/spf13/cobra"

// Server administration commands require the installed Linux runtime.
func registerLocalCommands(_ *cobra.Command) {}
