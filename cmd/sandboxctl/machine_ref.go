package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

func validResourceID(value string) bool {
	return value != "" && value != "." && value != ".." && !strings.ContainsAny(value, "/\\\x00\r\n")
}

func (c *client) machineID(ctx context.Context, ref string) (string, error) {
	name, named := strings.CutPrefix(ref, "name:")
	if !named {
		if !validResourceID(ref) {
			return "", errors.New("invalid machine ID")
		}
		return ref, nil
	}
	if c.key != "" || c.epoch != "" || c.revision != 0 || c.generation != 0 {
		return "", errors.New("use the original machine ID when supplying mutation or revision guards")
	}
	if strings.TrimSpace(name) == "" || strings.ContainsRune(name, 0) {
		return "", errors.New("missing or invalid machine display name")
	}
	var match string
	cursors := map[string]bool{}
	for cursor := ""; ; {
		query := url.Values{"cursor": {cursor}, "limit": {"100"}}
		var page struct {
			Items []struct {
				ID   string `json:"machine_id"`
				Name string `json:"name"`
			} `json:"items"`
			Next string `json:"next_cursor"`
		}
		if err := c.get(ctx, "machines?"+query.Encode(), &page); err != nil {
			return "", err
		}
		for _, item := range page.Items {
			if item.Name != name {
				continue
			}
			if !validResourceID(item.ID) {
				return "", errors.New("platform returned an invalid machine ID")
			}
			if match != "" && match != item.ID {
				return "", fmt.Errorf("machine name %q is ambiguous; use a machine ID", name)
			}
			match = item.ID
		}
		if page.Next == "" {
			break
		}
		if cursors[page.Next] {
			return "", errors.New("platform repeated a machine inventory cursor")
		}
		cursors[page.Next] = true
		cursor = page.Next
	}
	if match == "" {
		return "", fmt.Errorf("machine name %q was not found", name)
	}
	return match, nil
}

func (c *client) machinePath(ctx context.Context, ref, suffix string) (string, error) {
	id, err := c.machineID(ctx, ref)
	if err != nil {
		return "", err
	}
	return res("machines", id) + suffix, nil
}

func (c *client) showMachine(cmd *cobra.Command, ref, suffix string) error {
	path, err := c.machinePath(cmd.Context(), ref, suffix)
	if err != nil {
		return err
	}
	return c.show(cmd, path)
}
