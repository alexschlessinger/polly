package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpFixtureConfig runs this test binary as a real stdio server, without an
// external package, network download, or clock-dependent response.
func mcpFixtureConfig(t *testing.T) string {
	t.Helper()
	command, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	config := MCPServersConfig{MCPServers: map[string]MCPConfig{
		"time": {Command: command, Args: []string{"-test.run=^TestMCPStdioFixture$", "--", "mcp-stdio-fixture"}},
	}}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMCPStdioFixture(t *testing.T) {
	if os.Args[len(os.Args)-1] != "mcp-stdio-fixture" {
		return
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "time-fixture", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{
		Name: "get_current_time", Description: "Get the fixture time in a timezone",
		InputSchema: map[string]any{
			"type": "object", "title": "get_current_timeArguments",
			"properties": map[string]any{"timezone": map[string]any{"type": "string"}},
			"required":   []string{"timezone"}, "additionalProperties": false,
		},
	}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args struct {
			Timezone string `json:"timezone"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
		if args.Timezone == "" {
			return nil, fmt.Errorf("timezone is required")
		}
		return &mcp.CallToolResult{Content: []mcp.Content{
			&mcp.TextContent{Text: "time in " + args.Timezone + ": 03:04:05"},
		}}, nil
	})
	server.AddTool(&mcp.Tool{
		Name: "clock_status", Description: "Check the fixture clock",
		InputSchema: map[string]any{"type": "object", "title": "ClockStatusArguments", "properties": map[string]any{}, "additionalProperties": false},
	}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if string(req.Params.Arguments) != "{}" {
			return nil, fmt.Errorf("arguments = %s, want an empty object", req.Params.Arguments)
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ready"}}}, nil
	})
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// Keep the test runner's PASS line out of the protocol stream.
	os.Exit(0)
}
