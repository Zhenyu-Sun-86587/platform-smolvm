package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

const terminalProtocol = "sandbox.terminal.v1"

type terminalFrame struct {
	Type     string `json:"type"`
	StdinSeq uint64 `json:"stdin_seq,omitempty"`
	From     uint64 `json:"from,omitempty"`
	To       uint64 `json:"to,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
	Message  string `json:"message,omitempty"`
}

type terminalInput struct {
	Type string `json:"type"`
	Seq  uint64 `json:"seq,omitempty"`
	Data []byte `json:"data_base64,omitempty"`
	EOF  bool   `json:"eof,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
}

func terminalSize(input io.Reader) (uint16, uint16) {
	if file, ok := input.(*os.File); ok {
		w, h, err := term.GetSize(int(file.Fd()))
		if err == nil && w > 0 && w <= 65535 && h > 0 && h <= 65535 {
			return uint16(w), uint16(h)
		}
	}
	return 80, 24
}

func (c *client) attachTerminal(cmd *cobra.Command, output execOutput) error {
	id := output.ID
	path := res("execs", id) + "/attach?cursor=" + strconv.FormatUint(output.Next, 10)
	endpoint, err := c.endpoint(path)
	if err != nil {
		return err
	}
	if endpoint.Scheme == "https" {
		endpoint.Scheme = "wss"
	} else {
		endpoint.Scheme = "ws"
	}
	connect, cancelConnect := context.WithTimeout(cmd.Context(), 15*time.Second)
	ws, reply, err := websocket.Dial(connect, endpoint.String(), &websocket.DialOptions{
		HTTPClient: c.http, HTTPHeader: http.Header{"Authorization": {"Bearer " + c.token}},
		Subprotocols: []string{terminalProtocol}, CompressionMode: websocket.CompressionDisabled,
	})
	cancelConnect()
	if err != nil {
		if reply != nil && reply.Body != nil {
			_ = reply.Body.Close()
		}
		return fmt.Errorf("attach terminal %s: %w", id, err)
	}
	defer ws.CloseNow()
	ws.SetReadLimit(1 << 20)
	if ws.Subprotocol() != terminalProtocol {
		return fmt.Errorf("wrong terminal protocol for native exec %s", id)
	}
	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()
	ready, cancelReady := context.WithTimeout(ctx, 15*time.Second)
	var initial terminalFrame
	err = wsjson.Read(ready, ws, &initial)
	cancelReady()
	if err != nil || initial.Type != "ready" {
		return fmt.Errorf("native exec %s did not establish a terminal", id)
	}
	if err := c.writeExecOutput(cmd, output); err != nil {
		return err
	}
	input := cmd.InOrStdin()
	if file, ok := input.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		fd := int(file.Fd())
		state, err := term.MakeRaw(fd)
		if err != nil {
			return err
		}
		defer term.Restore(fd, state)
		resize := make(chan os.Signal, 1)
		signal.Notify(resize, syscall.SIGWINCH)
		defer signal.Stop(resize)
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-resize:
					w, h := terminalSize(input)
					if err := wsjson.Write(ctx, ws, terminalInput{Type: "resize", Cols: w, Rows: h}); err != nil {
						return
					}
				}
			}
		}()
	}
	inputResult := make(chan error, 1)
	go func() {
		err := sendTerminalInput(ctx, ws, input, initial.StdinSeq)
		inputResult <- err
		if err != nil {
			cancel()
		}
	}()
	for {
		kind, data, err := ws.Read(ctx)
		if err != nil {
			select {
			case inputErr := <-inputResult:
				if inputErr != nil && !errors.Is(inputErr, context.Canceled) {
					err = inputErr
				}
			default:
			}
			return fmt.Errorf("terminal %s ended without a Guest exit receipt: %w", id, err)
		}
		if kind == websocket.MessageBinary {
			if _, err := cmd.OutOrStdout().Write(data); err != nil {
				return err
			}
			continue
		}
		var frame terminalFrame
		if err := json.Unmarshal(data, &frame); err != nil {
			return fmt.Errorf("invalid terminal frame for native exec %s: %w", id, err)
		}
		switch frame.Type {
		case "exit":
			if frame.ExitCode == nil {
				return fmt.Errorf("terminal %s ended without a Guest exit status", id)
			}
			return execExit(execOutput{Exit: frame.ExitCode})
		case "gap":
			if !c.jsonOutput {
				fmt.Fprintf(cmd.ErrOrStderr(), "Native output gap from byte %d to %d\n", frame.From, frame.To)
			}
		case "error":
			return fmt.Errorf("terminal %s: %s", id, frame.Message)
		default:
			return fmt.Errorf("unexpected terminal frame for native exec %s", id)
		}
	}
}

func sendTerminalInput(ctx context.Context, ws *websocket.Conn, input io.Reader, seq uint64) error {
	buffer := make([]byte, 32<<10)
	for ctx.Err() == nil {
		n, readErr := input.Read(buffer)
		if n > 0 || readErr == io.EOF {
			seq++
			if err := wsjson.Write(ctx, ws, terminalInput{Type: "stdin", Seq: seq, Data: buffer[:n], EOF: readErr == io.EOF}); err != nil {
				// The read loop owns the exit receipt, even when the last
				// input write races with Guest exit.
				return nil
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
	return ctx.Err()
}
