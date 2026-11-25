package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"sandbox-platform/internal/buildinfo"
)

type mcpWriteIntent struct {
	ControlEpoch   string `json:"control_epoch" jsonschema:"Current epoch from get_control"`
	IdempotencyKey string `json:"idempotency_key" jsonschema:"Keep this key unchanged when retrying the same request"`
}

type mcpMachineIntent struct {
	mcpWriteIntent
	MachineID  string `json:"machine_id"`
	Revision   int64  `json:"revision" jsonschema:"Current machine revision"`
	Generation int64  `json:"binding_generation" jsonschema:"Expected machine binding generation"`
}

type mcpResult = map[string]any

func (c *client) mcpCommand() *cobra.Command {
	return sub("mcp", "Serve Sandbox Platform tools over Model Context Protocol stdio", cobra.NoArgs, func(cmd *cobra.Command, _ []string) error {
		if err := c.setup(); err != nil {
			return err
		}
		server := mcp.NewServer(&mcp.Implementation{Name: "sandbox-platform", Version: buildinfo.Version}, nil)
		c.registerMCPReadTools(server)
		c.registerMCPWriteTools(server)
		return server.Run(cmd.Context(), &mcp.StdioTransport{})
	})
}

func (c *client) registerMCPReadTools(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{Name: "get_control", Description: "Read the current control epoch and runtime capabilities."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, mcpResult, error) {
			return c.mcpRequest(ctx, http.MethodGet, "control", nil, mcpWriteIntent{}, 0)
		})
	mcp.AddTool(server, &mcp.Tool{Name: "list_machines", Description: "Read a page of workspaces visible to the current credential."},
		func(ctx context.Context, _ *mcp.CallToolRequest, input struct {
			Cursor string `json:"cursor,omitempty"`
		}) (*mcp.CallToolResult, mcpResult, error) {
			path := "machines"
			if input.Cursor != "" {
				path += "?cursor=" + url.QueryEscape(input.Cursor)
			}
			return c.mcpRequest(ctx, http.MethodGet, path, nil, mcpWriteIntent{}, 0)
		})
	mcp.AddTool(server, &mcp.Tool{Name: "get_machine", Description: "Read machine state, capabilities and binding generation."},
		func(ctx context.Context, _ *mcp.CallToolRequest, input struct {
			MachineID string `json:"machine_id"`
		}) (*mcp.CallToolResult, mcpResult, error) {
			return c.mcpRequest(ctx, http.MethodGet, res("machines", input.MachineID), nil, mcpWriteIntent{}, 0)
		})
	mcp.AddTool(server, &mcp.Tool{Name: "get_operation", Description: "Read a durable operation including its ready or failed children."},
		func(ctx context.Context, _ *mcp.CallToolRequest, input struct {
			OperationID string `json:"operation_id"`
		}) (*mcp.CallToolResult, mcpResult, error) {
			return c.mcpRequest(ctx, http.MethodGet, res("operations", input.OperationID), nil, mcpWriteIntent{}, 0)
		})
}

func (c *client) registerMCPWriteTools(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{Name: "create_machine", Description: "Submit a workspace create request and return its operation ID."},
		func(ctx context.Context, _ *mcp.CallToolRequest, input struct {
			mcpWriteIntent
			Spec map[string]any `json:"spec" jsonschema:"CreateMachine request as defined in the platform API"`
		}) (*mcp.CallToolResult, mcpResult, error) {
			return c.mcpRequest(ctx, http.MethodPost, "machines", input.Spec, input.mcpWriteIntent, 0)
		})
	mcp.AddTool(server, &mcp.Tool{Name: "fork_batch", Description: "Submit one verified independent fork for an ordered child set and return the operation ID."},
		func(ctx context.Context, _ *mcp.CallToolRequest, input struct {
			mcpMachineIntent
			Children []struct {
				Name string `json:"name"`
			} `json:"children"`
		}) (*mcp.CallToolResult, mcpResult, error) {
			return c.mcpMachineRequest(ctx, input.mcpMachineIntent, "fork-batches", map[string]any{
				"children": input.Children, "require_independent": true,
			})
		})
	mcp.AddTool(server, &mcp.Tool{Name: "change_power", Description: "Submit a start, stop, standby, wake or hibernate request and return the operation ID."},
		func(ctx context.Context, _ *mcp.CallToolRequest, input struct {
			mcpMachineIntent
			Action string `json:"action"`
		}) (*mcp.CallToolResult, mcpResult, error) {
			switch input.Action {
			case "start", "stop", "standby", "wake", "hibernate":
				return c.mcpMachineRequest(ctx, input.mcpMachineIntent, input.Action, map[string]any{})
			default:
				return nil, nil, errors.New("action must be start, stop, standby, wake or hibernate")
			}
		})
	mcp.AddTool(server, &mcp.Tool{Name: "start_task", Description: "Submit a persistent task inside an authorized workspace and return the operation ID."},
		func(ctx context.Context, _ *mcp.CallToolRequest, input struct {
			mcpMachineIntent
			Spec map[string]any `json:"spec" jsonschema:"Persistent task request as defined in the platform API"`
		}) (*mcp.CallToolResult, mcpResult, error) {
			return c.mcpMachineRequest(ctx, input.mcpMachineIntent, "tasks", input.Spec)
		})
	mcp.AddTool(server, &mcp.Tool{Name: "exec_guest", Description: "Start a foreground execution inside an authorized workspace and return its session ID."},
		func(ctx context.Context, _ *mcp.CallToolRequest, input struct {
			mcpWriteIntent
			MachineID string   `json:"machine_id"`
			Argv      []string `json:"argv"`
			Cwd       string   `json:"cwd,omitempty"`
		}) (*mcp.CallToolResult, mcpResult, error) {
			if input.MachineID == "" || len(input.Argv) == 0 {
				return nil, nil, errors.New("provide a machine ID and guest command arguments")
			}
			return c.mcpRequest(ctx, http.MethodPost, res("machines", input.MachineID)+"/exec", map[string]any{
				"argv": input.Argv, "cwd": input.Cwd,
			}, input.mcpWriteIntent, 0)
		})
}

func (c *client) mcpMachineRequest(ctx context.Context, input mcpMachineIntent, suffix string, body map[string]any) (*mcp.CallToolResult, mcpResult, error) {
	if input.MachineID == "" || input.Revision < 1 || input.Generation < 1 || body == nil {
		return nil, nil, errors.New("provide the machine ID, current revision and binding generation")
	}
	body["expected_generation"] = input.Generation
	return c.mcpRequest(ctx, http.MethodPost, res("machines", input.MachineID)+"/"+suffix, body, input.mcpWriteIntent, input.Revision)
}

func (c *client) mcpRequest(ctx context.Context, method, path string, payload any, intent mcpWriteIntent, revision int64) (*mcp.CallToolResult, mcpResult, error) {
	headers := make(http.Header)
	var request io.Reader
	if method == http.MethodPost {
		if intent.ControlEpoch == "" || intent.IdempotencyKey == "" || payload == nil {
			return nil, nil, errors.New("provide a control epoch, stable idempotency key and request body")
		}
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, nil, err
		}
		request = bytes.NewReader(data)
		headers.Set("Content-Type", "application/json")
		headers.Set("If-Control-Epoch", intent.ControlEpoch)
		headers.Set("Idempotency-Key", intent.IdempotencyKey)
		if revision > 0 {
			headers.Set("If-Match", fmt.Sprintf(`"%d"`, revision))
		}
	}
	r, err := c.request(ctx, method, path, request, headers)
	if err != nil {
		var failure *apiError
		if !errors.As(err, &failure) {
			return nil, nil, err
		}
		detail := mcpResult{"status": failure.Status, "error": failure.Detail}
		encoded, err := json.Marshal(detail)
		if err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: string(encoded)}}}, detail, nil
	}
	defer r.Body.Close()
	const maxResult = 4 << 20
	data, err := io.ReadAll(io.LimitReader(r.Body, maxResult+1))
	if err != nil {
		return nil, nil, err
	}
	if len(data) > maxResult {
		return nil, nil, errors.New("platform response exceeds the MCP result limit")
	}
	var result mcpResult
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil {
		return nil, nil, err
	}
	return nil, result, nil
}
