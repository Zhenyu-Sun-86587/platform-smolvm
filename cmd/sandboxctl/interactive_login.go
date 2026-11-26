package main

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func (c *client) promptLogin(cmd *cobra.Command, editor *lineEditor, output io.Writer) error {
	if !c.explicitOrigin {
		if profile, err := c.readProfile(); err == nil {
			c.origin = profile.Origin
		}
		value, err := editor.read("Platform URL [" + c.origin + "]: ")
		if err != nil {
			return err
		}
		if value = strings.TrimSpace(value); value != "" {
			c.origin = value
			c.explicitOrigin = true
		}
	}
	origin, err := platformOrigin(c.origin)
	if err != nil {
		return err
	}
	keys := *origin
	keys.Path, keys.Fragment = "/", "/api-keys"
	fmt.Fprintln(output, "Create or copy a Platform API token at", keys.String())
	answer, err := editor.read("Open API keys in your browser? [Y/n] ")
	if err != nil {
		return err
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "", "y", "yes":
		openLoginBrowser(cmd.Context(), keys.String())
	}
	c.token, err = editor.password("API token: ")
	c.token = strings.TrimSpace(c.token)
	return err
}

func openLoginBrowser(ctx context.Context, target string) {
	launcher := "xdg-open"
	if runtime.GOOS == "darwin" {
		launcher = "/usr/bin/open"
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	// The URL is also printed for terminals without a default browser.
	_ = exec.CommandContext(ctx, launcher, target).Run()
}
