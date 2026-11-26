package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/spf13/cobra"
)

func (c *client) exec() *cobra.Command {
	var cwd, user string
	var timeout, wait int
	var follow, tty bool
	command := sub("exec MACHINE -- COMMAND [ARG...]", "Run a native Guest command", cobra.MinimumNArgs(2), func(cmd *cobra.Command, args []string) error {
		if wait < 0 || wait > 30 || timeout < 0 || timeout > 86400 {
			return errors.New("invalid wait-seconds or timeout-seconds")
		}
		if follow && tty || c.noWait && (follow || tty) {
			return errors.New("--follow, --tty and --no-wait select different exec modes")
		}

		m, err := c.machine(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		directory := cwd
		if (follow || tty) && !cmd.Flags().Changed("cwd") {
			directory = ""
		}
		body := commandArgs(args[1:], directory, user)
		body["timeout_seconds"], body["wait_seconds"] = timeout, wait
		if tty || c.noWait {
			body["wait_seconds"] = 0
		}
		if tty {
			width, height := terminalSize(cmd.InOrStdin())
			body["tty"], body["interactive"], body["cols"], body["rows"] = true, true, width, height
		}
		path := res("machines", m.ID) + "/exec"
		raw, err := c.startExec(cmd, path, body, m.Revision)
		if err != nil {
			return err
		}
		var output execOutput
		if err := json.Unmarshal(raw, &output); err != nil {
			return err
		}
		if !validResourceID(output.ID) || output.Next > output.End {
			return errors.New("platform returned an invalid native exec receipt")
		}
		switch {
		case c.noWait:
			return c.print(cmd, raw)
		case tty:
			return c.attachTerminal(cmd, output)
		case follow:
			return c.observeExec(cmd, raw, output, true)
		case c.jsonOutput:
			if err := c.print(cmd, raw); err != nil {
				return err
			}
			return execExit(output)
		default:
			return c.observeExec(cmd, raw, output, false)
		}
	})
	command.Flags().StringVar(&cwd, "cwd", "/workspace", "Guest working directory (follow and tty use the runtime default)")
	command.Flags().StringVar(&user, "user", "root", "Guest user")
	command.Flags().IntVar(&timeout, "timeout-seconds", 300, "Native command timeout")
	command.Flags().IntVar(&wait, "wait-seconds", 10, "Initial wait for a noninteractive command")
	command.Flags().BoolVar(&follow, "follow", false, "Follow Guest stdout and stderr until exit")
	command.Flags().BoolVarP(&tty, "tty", "t", false, "Attach an interactive native Guest terminal")
	return command
}

func (c *client) startExec(cmd *cobra.Command, path string, body map[string]any, revision int64) (json.RawMessage, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	headers, err := c.intent(cmd.Context(), revision)
	if err != nil {
		return nil, err
	}
	headers.Set("Content-Type", "application/json")
	resp, err := c.request(cmd.Context(), http.MethodPost, path, bytes.NewReader(raw), headers)
	if err != nil {
		return nil, c.mutationError(cmd, http.MethodPost, path, headers, 0, err)
	}
	defer resp.Body.Close()
	var result json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, c.mutationError(cmd, http.MethodPost, path, headers, 0, err)
	}
	return result, nil
}

func (c *client) observeExec(cmd *cobra.Command, raw json.RawMessage, output execOutput, follow bool) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), c.waitTimeout)
	defer cancel()
	id := output.ID
	for {
		if c.jsonOutput {
			if err := c.print(cmd, raw); err != nil {
				return err
			}
		} else if err := c.writeExecOutput(cmd, output); err != nil {
			return err
		}
		if output.Done && output.Next >= output.End {
			return execExit(output)
		}
		if !follow && !output.Done {
			fmt.Fprintf(cmd.ErrOrStderr(), "Native exec %s at byte %d\n", id, output.Next)
			return nil
		}
		if output.Next >= output.End {
			timer := time.NewTimer(200 * time.Millisecond)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return execObserveError(id, ctx.Err())
			}
		}
		cursor := output.Next
		path := res("execs", id) + "/output?cursor=" + strconv.FormatUint(cursor, 10)
		if err := c.get(ctx, path, &raw); err != nil {
			if ctx.Err() != nil {
				return execObserveError(id, ctx.Err())
			}
			return fmt.Errorf("observe native exec %s at byte %d: %w", id, cursor, err)
		}
		output = execOutput{}
		if err := json.Unmarshal(raw, &output); err != nil {
			return err
		}
		if output.ID != id || output.Next < cursor || output.End < output.Next || output.Next == cursor && output.End > cursor {
			return fmt.Errorf("invalid output cursor for native exec %s", id)
		}
	}
}

func execObserveError(id string, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &apiError{ExitCode: 124, Detail: apiErrorDetail{
			Code: "EXEC_WAIT_TIMEOUT", Message: "wait deadline exceeded; read the original session with execs output " + id,
			ExecID: id, Effect: "unknown", Retry: "client_decision",
		}}
	}
	return fmt.Errorf("observe native exec %s: %w", id, err)
}
