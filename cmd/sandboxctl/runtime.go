package main

import (
	"github.com/spf13/cobra"
	"sandbox-platform/internal/runtime/customsmolvm"
)

func runtimeCapabilitiesCommand() *cobra.Command {
	var socket, bundle, reader string
	cmd := &cobra.Command{Use: "runtime-capabilities", Short: "Check a deep Serve socket against an installed bundle and state reader", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			h, err := customsmolvm.Probe(cmd.Context(), socket, bundle, reader)
			if err != nil {
				return err
			}
			return writeLocalResult(cmd, h)
		}}
	cmd.Flags().StringVar(&socket, "socket", "", "Local deep Serve socket")
	cmd.Flags().StringVar(&bundle, "bundle", "", "Expected bundle SHA-256 digest")
	cmd.Flags().StringVar(&reader, "reader", "", "Expected state reader ID")
	for _, flag := range []string{"socket", "bundle", "reader"} {
		_ = cmd.MarkFlagRequired(flag)
	}
	return cmd
}
