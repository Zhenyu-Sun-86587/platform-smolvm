package main

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

func (c *client) machineProductCommands() []*cobra.Command {
	show := sub("show MACHINE", "Show workspace identity, settings and observed state", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		return c.showMachine(cmd, args[0], "")
	})
	rename := sub("rename MACHINE NAME", "Change the workspace display name", cobra.ExactArgs(2), func(cmd *cobra.Command, args []string) error {
		if strings.TrimSpace(args[1]) == "" {
			return errors.New("a nonblank display name is required")
		}
		m, err := c.machine(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		return c.mutation(cmd, http.MethodPatch, res("machines", m.ID), map[string]any{
			"name": args[1], "expected_generation": m.Generation,
		}, m.Revision)
	})
	config := &cobra.Command{Use: "config", Short: "Read and update workspace configuration"}
	var body string
	set := sub("set MACHINE", "Patch workspace settings using the current machine guards", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		payload, err := jsonBody(strings.TrimSpace(body))
		if err != nil {
			return err
		}
		var patch map[string]json.RawMessage
		if err := json.Unmarshal(payload, &patch); err != nil {
			return err
		}
		m, err := c.machine(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		generation := json.RawMessage(strconv.FormatInt(m.Generation, 10))
		if existing, ok := patch["expected_generation"]; ok {
			var supplied int64
			if err := json.Unmarshal(existing, &supplied); err != nil || supplied != m.Generation {
				return errors.New("body expected_generation must match the selected generation")
			}
		}
		patch["expected_generation"] = generation
		return c.mutation(cmd, http.MethodPatch, res("machines", m.ID), patch, m.Revision)
	})
	set.Flags().StringVar(&body, "body", "", "JSON object containing the supported machine settings to change")
	_ = set.MarkFlagRequired("body")
	config.AddCommand(sub("get MACHINE", "Read workspace configuration and lifecycle status", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		return c.showMachine(cmd, args[0], "")
	}), set)
	return []*cobra.Command{show, rename, config}
}

func (c *client) serviceProductCommands() []*cobra.Command {
	status := sub("status SERVICE", "Read configured service and latest scoped observation", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		return c.show(cmd, res("services", args[0]))
	})
	endpoint := sub("endpoint SERVICE", "Read the configured service endpoint without opening it", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		var service struct {
			ID          string          `json:"service_id"`
			Protocol    string          `json:"protocol"`
			PublicHost  string          `json:"public_host,omitempty"`
			PublicPort  int             `json:"public_port,omitempty"`
			Enabled     bool            `json:"enabled"`
			Observation json.RawMessage `json:"observation,omitempty"`
			URL         string          `json:"url,omitempty"`
		}
		if err := c.get(cmd.Context(), res("services", args[0]), &service); err != nil {
			return err
		}
		if service.PublicHost != "" && (service.Protocol == "http" || service.Protocol == "https") {
			host := service.PublicHost
			if service.PublicPort > 0 {
				host = net.JoinHostPort(host, strconv.Itoa(service.PublicPort))
			}
			service.URL = (&url.URL{Scheme: "https", Host: host}).String()
		}
		return c.print(cmd, service)
	})
	return []*cobra.Command{status, endpoint}
}

func (c *client) lifecycleDefaults() *cobra.Command {
	group := &cobra.Command{Use: "defaults", Short: "Read and edit the default Worker lifecycle policy"}
	group.AddCommand(sub("get", "Read the saved default lifecycle policy and revision", cobra.NoArgs, func(cmd *cobra.Command, _ []string) error {
		var response struct {
			Revision       int64           `json:"revision"`
			PolicyRevision int64           `json:"default_lifecycle_revision"`
			Policy         json.RawMessage `json:"default_lifecycle_policy"`
		}
		if err := c.get(cmd.Context(), "system/settings", &response); err != nil {
			return err
		}
		return c.print(cmd, response)
	}))
	var raw string
	set := sub("set", "Patch the default lifecycle policy for new workspaces", cobra.NoArgs, func(cmd *cobra.Command, _ []string) error {
		policy, err := jsonBody(strings.TrimSpace(raw))
		if err != nil {
			return err
		}
		revision, err := c.settingsRevision(cmd)
		if err != nil {
			return err
		}
		return c.mutation(cmd, http.MethodPatch, "system/settings", map[string]json.RawMessage{"default_lifecycle_policy": policy}, revision)
	})
	set.Flags().StringVar(&raw, "policy", "", "JSON object containing the default policy fields to change")
	_ = set.MarkFlagRequired("policy")
	group.AddCommand(set)
	return group
}

func (c *client) networks() *cobra.Command {
	group := c.list("networks", "networks", false)
	group.Short = "Inspect registered Worker private networks and defaults"
	group.AddCommand(c.list("list", "networks", false))
	group.AddCommand(sub("attachment MACHINE", "Read the current workspace network attachment", cobra.ExactArgs(1), func(cmd *cobra.Command, args []string) error {
		return c.showMachine(cmd, args[0], "/network")
	}))
	var mode, network string
	defaults := sub("set-defaults", "Set the default network for new System workspaces", cobra.NoArgs, func(cmd *cobra.Command, _ []string) error {
		patch := map[string]string{}
		if cmd.Flags().Changed("mode") {
			if mode != "native" && mode != "fixed_internal" {
				return errors.New("--mode must be native or fixed_internal")
			}
			patch["default_network_mode"] = mode
		}
		if cmd.Flags().Changed("network") {
			patch["default_network_id"] = network
		}
		if len(patch) == 0 {
			return errors.New("specify --mode or --network")
		}
		revision, err := c.settingsRevision(cmd)
		if err != nil {
			return err
		}
		return c.mutation(cmd, http.MethodPatch, "system/settings", patch, revision)
	})
	defaults.Flags().StringVar(&mode, "mode", "", "native or fixed_internal")
	defaults.Flags().StringVar(&network, "network", "", "Registered default network ID")
	group.AddCommand(defaults)
	return group
}

func (c *client) settingsRevision(cmd *cobra.Command) (int64, error) {
	if c.revision != 0 {
		return c.revision, nil
	}
	var settings struct {
		Revision int64 `json:"revision"`
	}
	err := c.get(cmd.Context(), "system/settings", &settings)
	return settings.Revision, err
}
