//go:build linux

package main

import (
	"sandbox-platform/internal/runtime/customsmolvm"

	"github.com/spf13/cobra"
)

type runtimeEndpoint struct{ socket, bundle, reader string }

func (e *runtimeEndpoint) flags(cmd *cobra.Command) {
	cmd.Flags().StringVar(&e.socket, "socket", "", "Local deep Serve socket")
	cmd.Flags().StringVar(&e.bundle, "bundle", "", "Expected bundle SHA-256 digest")
	cmd.Flags().StringVar(&e.reader, "reader", "", "Expected state reader ID")
	for _, flag := range []string{"socket", "bundle", "reader"} {
		_ = cmd.MarkFlagRequired(flag)
	}
}

func runtimeCapabilitiesCommand() *cobra.Command {
	var endpoint runtimeEndpoint
	cmd := &cobra.Command{Use: "runtime-capabilities", Short: "Check a deep Serve socket against an installed bundle and state reader", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			h, err := customsmolvm.Probe(cmd.Context(), endpoint.socket, endpoint.bundle, endpoint.reader)
			if err != nil {
				return err
			}
			return writeLocalResult(cmd, h)
		}}
	endpoint.flags(cmd)
	return cmd
}

func runtimeOperationCommand() *cobra.Command {
	var endpoint runtimeEndpoint
	cmd := &cobra.Command{Use: "runtime-operation ID", Short: "Read a durable operation receipt from the selected deep Serve", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			receipt, err := customsmolvm.QueryOperation(cmd.Context(), endpoint.socket, endpoint.bundle, endpoint.reader, args[0])
			if err != nil {
				return err
			}
			return writeLocalResult(cmd, receipt)
		}}
	endpoint.flags(cmd)
	return cmd
}
