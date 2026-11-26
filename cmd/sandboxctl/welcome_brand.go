package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

const defaultWordmark = "Sandbox Platform"

const (
	welcomeFrames = 15
	welcomeTick   = 40 * time.Millisecond
)

func (c *client) brandDirectory() string {
	if dir := os.Getenv("SANDBOXCTL_BRAND_DIR"); dir != "" {
		return dir
	}
	dir, err := c.localDirectory()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "brand")
}

func wordmark(directory string, width int) []string {
	fallback := []string{defaultWordmark}
	if directory == "" {
		return fallback
	}
	path := filepath.Join(directory, "terminal.txt")
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4096 {
		return fallback
	}
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) > 4096 {
		return fallback
	}
	rows := strings.Split(strings.Trim(string(raw), "\r\n"), "\n")
	if len(rows) > 8 || !strings.Contains(string(raw), "#") {
		return fallback
	}
	for i, row := range rows {
		row = strings.TrimRight(row, "\r ")
		if len(row) >= width || len(row) > 80 || strings.ContainsFunc(row, func(r rune) bool { return r != '#' && r != ' ' }) {
			return fallback
		}
		rows[i] = strings.ReplaceAll(row, "#", "█")
	}
	return rows
}

func reducedMotion(ctx context.Context) bool {
	switch strings.ToLower(os.Getenv("SANDBOXCTL_REDUCE_MOTION")) {
	case "1", "true", "yes":
		return true
	}
	if runtime.GOOS != "darwin" {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	value, err := exec.CommandContext(ctx, "/usr/bin/defaults", "read", "com.apple.universalaccess", "reduceMotion").Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(value)) == "1"
}

func (c *client) showWelcome(ctx context.Context, input, output *os.File, skip bool) error {
	width, height, _ := term.GetSize(int(output.Fd()))
	rows := wordmark(c.brandDirectory(), width)
	fmt.Fprintln(output)
	const hint = "Press any key to skip"
	if skip || os.Getenv("TERM") == "dumb" || width <= len(hint) || height <= len(rows)+1 || reducedMotion(ctx) {
		_, err := fmt.Fprintln(output, strings.Join(rows, "\n"))
		return err
	}
	fd := int(input.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		_, err = fmt.Fprintln(output, strings.Join(rows, "\n"))
		return err
	}
	defer term.Restore(fd, old)
	fmt.Fprint(output, "\x1b[?25l")
	defer fmt.Fprint(output, "\x1b[?25h")

	poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	for frame := 0; frame < welcomeFrames; frame++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if frame > 0 {
			fmt.Fprintf(output, "\x1b[%dA", len(rows)+1)
		}
		for _, row := range rows {
			fmt.Fprint(output, "\r", welcomeFrame(row, frame), "\x1b[K\r\n")
		}
		fmt.Fprint(output, "\r", hint, "\x1b[K\r\n")

		n, err := unix.Poll(poll, int(welcomeTick/time.Millisecond))
		if err != nil && !errors.Is(err, unix.EINTR) {
			break
		}
		if n > 0 {
			if poll[0].Revents&unix.POLLIN != 0 {
				var key [1]byte
				if _, err := input.Read(key[:]); err == nil && key[0] == 3 {
					return exitStatus(130)
				}
			}
			break
		}
	}

	fmt.Fprintf(output, "\x1b[%dA", len(rows)+1)
	for _, row := range rows {
		fmt.Fprint(output, "\r", row, "\x1b[K\r\n")
	}
	_, err = io.WriteString(output, "\r\x1b[K")
	return err
}

func welcomeFrame(row string, frame int) string {
	var rendered strings.Builder
	cells := []rune(row)
	width := len(cells) - 1
	for x, cell := range cells {
		// Resolve the center first, then move outward toward both edges.
		distance := max(width-2*x, 2*x-width)
		start := 10 * distance / max(1, width)
		switch {
		case cell == ' ' || frame >= start+3:
			rendered.WriteRune(cell)
		case frame >= start:
			rendered.WriteRune('·')
		default:
			rendered.WriteByte(' ')
		}
	}
	return rendered.String()
}
