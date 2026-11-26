package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

func commandTerminal(cmd *cobra.Command) (input, output *os.File, interactive bool) {
	input, _ = cmd.InOrStdin().(*os.File)
	output, _ = cmd.OutOrStdout().(*os.File)
	return input, output, input != nil && output != nil &&
		term.IsTerminal(int(input.Fd())) && term.IsTerminal(int(output.Fd()))
}

type lineEditor struct {
	input    *os.File
	output   *os.File
	terminal *term.Terminal
}

// The editor reads through the Enter key. Leaving later bytes in the TTY
// keeps pasted/type-ahead input available to a nested login or Guest shell.
type terminalInputReader struct {
	ctx  context.Context
	file *os.File
}

func (r terminalInputReader) Read(buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	poll := []unix.PollFd{{Fd: int32(r.file.Fd()), Events: unix.POLLIN}}
	for r.ctx.Err() == nil {
		n, err := unix.Poll(poll, 150)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if n > 0 {
			return r.file.Read(buf[:1])
		}
	}
	return 0, r.ctx.Err()
}

func newLineEditor(ctx context.Context, input, output *os.File) *lineEditor {
	stream := struct {
		io.Reader
		io.Writer
	}{terminalInputReader{ctx, input}, output}
	return &lineEditor{input, output, term.NewTerminal(stream, "")}
}

func (e *lineEditor) read(prompt string) (string, error) {
	e.terminal.SetPrompt(prompt)
	return e.readRaw(e.terminal.ReadLine)
}

func (e *lineEditor) password(prompt string) (string, error) {
	return e.readRaw(func() (string, error) { return e.terminal.ReadPassword(prompt) })
}

func (e *lineEditor) readRaw(read func() (string, error)) (string, error) {
	fd := int(e.input.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return "", err
	}
	defer term.Restore(fd, old)
	if width, height, err := term.GetSize(int(e.output.Fd())); err == nil && width > 0 && height > 0 {
		_ = e.terminal.SetSize(width, height)
	}
	line, err := read()
	if errors.Is(err, term.ErrPasteIndicator) {
		err = nil
	}
	return line, err
}

func (c *client) addInteractiveShell(root *cobra.Command) {
	var skip bool
	root.Flags().BoolVar(&skip, "no-animation", false, "Show the interactive welcome without animation")
	root.RunE = func(cmd *cobra.Command, _ []string) error {
		input, output, interactive := commandTerminal(cmd)
		if !interactive || c.jsonOutput {
			return cmd.Help()
		}
		return c.runInteractiveShell(cmd, input, output, skip)
	}
	var shellSkip bool
	shell := sub("shell", "Open the interactive workspace shell", cobra.NoArgs, func(cmd *cobra.Command, _ []string) error {
		input, output, interactive := commandTerminal(cmd)
		if !interactive || c.jsonOutput {
			return errors.New("sandboxctl shell requires a terminal on stdin and stdout")
		}
		return c.runInteractiveShell(cmd, input, output, shellSkip)
	})
	shell.Flags().BoolVar(&shellSkip, "no-animation", false, "Show the welcome without animation")
	root.AddCommand(shell)
}

// Each command starts with a new client, so connection and mutation flags
// supplied on one line never carry over into the next command.
func shellConnectionArgs(cmd *cobra.Command) []string {
	var args []string
	for _, name := range []string{"url", "profile", "config-dir", "ca-file", "token"} {
		if flag := cmd.Flags().Lookup(name); flag != nil && flag.Changed {
			args = append(args, "--"+name+"="+flag.Value.String())
		}
	}
	return args
}

func shellCommand(cmd *cobra.Command, connection, args []string) (*cobra.Command, *client) {
	command, api := rootCommand()
	command.RunE = func(child *cobra.Command, _ []string) error { return child.Help() }
	if shell, _, err := command.Find([]string{"shell"}); err == nil && shell != command {
		shell.RunE = func(_ *cobra.Command, _ []string) error {
			return errors.New("you are already in the workspace shell")
		}
	}
	command.SetArgs(append(append([]string{}, connection...), args...))
	command.SetIn(cmd.InOrStdin())
	command.SetOut(cmd.OutOrStdout())
	command.SetErr(cmd.ErrOrStderr())
	return command, api
}

func (c *client) runInteractiveShell(cmd *cobra.Command, input, output *os.File, skip bool) error {
	if err := c.showWelcome(cmd.Context(), input, output, skip); err != nil {
		return err
	}
	editor := newLineEditor(cmd.Context(), input, output)
	connection := shellConnectionArgs(cmd)
	editor.terminal.AutoCompleteCallback = shellCompletion

	_, profileErr := c.readProfile()
	if c.token == "" && errors.Is(profileErr, os.ErrNotExist) {
		fmt.Fprintln(output, "Sign in to list your workspaces and create your first one.")
		answer, err := editor.read("Set up this profile now? [Y/n] ")
		if errors.Is(err, io.EOF) {
			fmt.Fprintln(output)
			return nil
		}
		if err != nil {
			return err
		}
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "", "y", "yes":
			login, api := shellCommand(cmd, connection, []string{"login", "--no-ssh-config"})
			if err := login.ExecuteContext(cmd.Context()); err != nil {
				reportError(cmd.ErrOrStderr(), api.jsonOutput, err)
			}
		}
	}
	c.showShellInventory(cmd)
	fmt.Fprintln(output, "Type help for commands, machines create to get started, or exit to leave.")

	for cmd.Context().Err() == nil {
		line, err := editor.read("sandboxctl> ")
		if errors.Is(err, io.EOF) {
			fmt.Fprintln(output)
			return nil
		}
		if err != nil {
			return err
		}
		args, err := splitShellLine(line)
		if err != nil {
			reportError(cmd.ErrOrStderr(), false, err)
			continue
		}
		if len(args) == 0 {
			continue
		}
		if len(args) == 1 && (args[0] == "exit" || args[0] == "quit") {
			return nil
		}
		if needsWorkspacePicker(args) {
			args, err = c.chooseWorkspace(cmd, editor, args)
			if err != nil && !errors.Is(err, io.EOF) {
				reportError(cmd.ErrOrStderr(), false, err)
			}
			if err != nil || len(args) == 0 {
				continue
			}
		}
		command, api := shellCommand(cmd, connection, args)
		if err := command.ExecuteContext(cmd.Context()); err != nil {
			if status := reportError(cmd.ErrOrStderr(), api.jsonOutput, err); status != 1 && !api.jsonOutput {
				fmt.Fprintf(cmd.ErrOrStderr(), "Command exited with status %d.\n", status)
			}
		}
	}
	return cmd.Context().Err()
}

type shellWorkspace struct {
	ID    string `json:"machine_id"`
	Name  string `json:"name"`
	Power string `json:"power_state"`
}

type shellSnapshot struct {
	ID    string `json:"snapshot_id"`
	Label string `json:"label"`
	State string `json:"state"`
}

type shellTemplate struct {
	ID   string `json:"template_id"`
	Name string `json:"name"`
	Spec struct {
		BootMode string `json:"boot_mode"`
	} `json:"spec"`
}

type resourcePage[T any] struct {
	Items []T    `json:"items"`
	Next  string `json:"next_cursor"`
}

// Keep the shell's control requests independent of the caller's client so a
// login replaced from inside the shell is visible on the next request.
func (c *client) shellClient() *client {
	return &client{
		origin: c.origin, token: c.token, caFile: c.caFile, configDir: c.configDir,
		profileName: c.profileName, useProfiles: true,
		explicitProfile: c.explicitProfile, explicitOrigin: c.explicitOrigin,
	}
}

func (c *client) showShellInventory(cmd *cobra.Command) {
	if c.token == "" {
		if _, err := c.readProfile(); errors.Is(err, os.ErrNotExist) {
			return
		}
	}
	api := c.shellClient()
	ctx, cancel := context.WithTimeout(cmd.Context(), 4*time.Second)
	defer cancel()
	writer := cmd.OutOrStdout()
	workspaces := resourcePage[shellWorkspace]{}
	if err := api.get(ctx, "machines?limit=8", &workspaces); err != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), "Could not list workspaces:", terminalText(err.Error()))
		return
	}
	fmt.Fprintln(writer, "Workspaces")
	if len(workspaces.Items) == 0 {
		fmt.Fprintln(writer, "  No workspaces yet.")
	}
	for _, item := range workspaces.Items {
		fmt.Fprintf(writer, "  %s  %s  %s\n", terminalText(item.Name), terminalText(item.Power), terminalText(item.ID))
	}
	if workspaces.Next != "" {
		fmt.Fprintln(writer, "  Run machines list for more workspaces.")
	}
	snapshots := resourcePage[shellSnapshot]{}
	if err := api.get(ctx, "snapshots?limit=5", &snapshots); err != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), "Could not list snapshots:", terminalText(err.Error()))
		return
	}
	fmt.Fprintln(writer, "Snapshots")
	if len(snapshots.Items) == 0 {
		fmt.Fprintln(writer, "  No snapshots yet.")
	}
	for _, item := range snapshots.Items {
		fmt.Fprintf(writer, "  %s  %s  %s\n", terminalText(item.Label), terminalText(item.State), terminalText(item.ID))
	}
	if snapshots.Next != "" {
		fmt.Fprintln(writer, "  Run snapshots list MACHINE for more snapshots.")
	}
}

func needsWorkspacePicker(args []string) bool {
	return (len(args) == 2 || len(args) == 3 && !strings.HasPrefix(args[2], "-")) &&
		(args[0] == "machines" || args[0] == "machine") &&
		(args[1] == "create" || args[1] == "new")
}

func (c *client) chooseWorkspace(cmd *cobra.Command, editor *lineEditor, args []string) ([]string, error) {
	ctx, cancel := context.WithTimeout(cmd.Context(), 8*time.Second)
	defer cancel()
	templates := resourcePage[shellTemplate]{}
	if err := c.shellClient().get(ctx, "templates?limit=100", &templates); err != nil {
		return nil, err
	}
	if len(templates.Items) == 0 {
		return nil, errors.New("there are no registered templates")
	}
	var name string
	if len(args) == 3 {
		name = args[2]
	} else {
		value, err := editor.read("Workspace name (empty to cancel): ")
		if err != nil || strings.TrimSpace(value) == "" {
			return nil, err
		}
		name = strings.TrimSpace(value)
	}
	for i, item := range templates.Items {
		fmt.Fprintf(editor.output, "  %d. %s (%s)\n", i+1, terminalText(item.Name), terminalText(item.ID))
	}
	if templates.Next != "" {
		fmt.Fprintln(editor.output, "Use templates list for later pages and supply --template to select one.")
	}
	for {
		value, err := editor.read("Template number (empty to cancel): ")
		if err != nil || strings.TrimSpace(value) == "" {
			return nil, err
		}
		pick, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || pick < 1 || pick > len(templates.Items) {
			fmt.Fprintln(editor.output, "Choose a number from the list.")
			continue
		}
		selected := templates.Items[pick-1]
		create := []string{"machines", "create", "--template=" + selected.ID}
		if selected.Spec.BootMode == "system" || selected.Spec.BootMode == "oci" {
			create = append(create, "--boot-mode="+selected.Spec.BootMode)
		}
		return append(create, "--", name), nil
	}
}

func terminalText(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
}

func shellCompletion(line string, pos int, key rune) (string, int, bool) {
	if key != '\t' {
		return "", 0, false
	}
	prefix := line[:pos]
	if strings.ContainsAny(prefix, "'\"\\") {
		return line, pos, true
	}
	args := strings.Fields(prefix)
	last, _ := utf8.DecodeLastRuneInString(prefix)
	if len(args) == 0 || unicode.IsSpace(last) {
		args = append(args, "")
	}
	part := args[len(args)-1]
	if len(args) == 1 && strings.HasPrefix("exit", part) && part != "" {
		args[0] = "exit"
	} else {
		complete, _ := rootCommand()
		var results bytes.Buffer
		complete.SetOut(&results)
		complete.SetErr(io.Discard)
		complete.SetArgs(append([]string{cobra.ShellCompNoDescRequestCmd}, args...))
		if err := complete.Execute(); err != nil {
			return line, pos, true
		}
		matches := strings.Split(strings.TrimSpace(results.String()), "\n")
		matches = matches[:len(matches)-1] // Cobra's trailing completion directive.
		if len(matches) != 1 || !strings.HasPrefix(matches[0], part) {
			return line, pos, true
		}
		args[len(args)-1] = matches[0]
	}
	replacement := args[len(args)-1]
	if pos == len(line) {
		replacement += " "
	}
	start := pos - len(part)
	return line[:start] + replacement + line[pos:], start + len(replacement), true
}

func splitShellLine(line string) ([]string, error) {
	var args []string
	var word strings.Builder
	var quote rune
	started, escaped := false, false
	for _, r := range line {
		switch {
		case escaped:
			if quote == '"' && !strings.ContainsRune("\"\\$`", r) {
				word.WriteByte('\\')
			}
			word.WriteRune(r)
			escaped = false
		case r == quote && quote != 0:
			quote = 0
		case r == '\\' && quote != '\'':
			started, escaped = true, true
		case quote != 0:
			word.WriteRune(r)
		case r == '\'' || r == '"':
			started, quote = true, r
		case unicode.IsSpace(r):
			if started {
				args = append(args, word.String())
				word.Reset()
				started = false
			}
		default:
			started = true
			word.WriteRune(r)
		}
	}
	if quote != 0 || escaped {
		return nil, errors.New("unfinished quote or escape")
	}
	if started {
		args = append(args, word.String())
	}
	return args, nil
}
