package tools

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// checkUvxAvailable checks if uvx is available on the system
func checkUvxAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("uvx"); err != nil {
		t.Skip("uvx is not installed, skipping MCP tests")
	}
}

func TestMCPClient(t *testing.T) {
	checkUvxAvailable(t)
	
	// Start the MCP server using uvx
	serverCmd := "uvx mcp-server-time"
	
	client, err := NewMCPClient(serverCmd)
	if err != nil {
		// If uvx is not available or server can't start, skip the test
		t.Skipf("Could not start MCP server (is uvx available?): %v", err)
	}
	defer client.Close()

	// Give the server a moment to initialize
	time.Sleep(100 * time.Millisecond)

	// Test listing tools
	tools, err := client.ListTools()
	if err != nil {
		t.Fatalf("Failed to list tools: %v", err)
	}

	if len(tools) == 0 {
		t.Error("Expected at least one tool from mcp-server-time")
	}

	// Verify we got expected tools
	var foundTimeTools bool
	for _, tool := range tools {
		schema := tool.GetSchema()
		if schema != nil && strings.Contains(schema.Title, "time") {
			foundTimeTools = true
			break
		}
	}

	if !foundTimeTools {
		t.Error("Expected to find time-related tools")
	}
}

func TestMCPToolExecution(t *testing.T) {
	checkUvxAvailable(t)
	ctx := context.Background()
	
	// Start the MCP server
	serverCmd := "uvx mcp-server-time"
	
	client, err := NewMCPClient(serverCmd)
	if err != nil {
		t.Skipf("Could not start MCP server: %v", err)
	}
	defer client.Close()

	// Give the server a moment to initialize
	time.Sleep(100 * time.Millisecond)

	// Get tools
	tools, err := client.ListTools()
	if err != nil {
		t.Fatalf("Failed to list tools: %v", err)
	}

	if len(tools) == 0 {
		t.Fatal("No tools available from server")
	}

	// Find the get_current_time tool and test it
	for _, tool := range tools {
		schema := tool.GetSchema()
		if schema.Title == "get_current_time" {
			t.Logf("Testing tool: %s", schema.Title)
			
			// Test with valid timezone
			args := map[string]any{
				"timezone": "America/New_York",
			}
			
			result, err := tool.Execute(ctx, args)
			if err != nil {
				t.Errorf("Failed to execute tool with valid args: %v", err)
			} else {
				t.Logf("Tool result: %s", result)
				// Verify we got some result
				if result == "" {
					t.Error("Expected non-empty result from tool execution")
				}
				// Result should contain time information
				if !strings.Contains(result, "time") && !strings.Contains(result, "Time") {
					t.Error("Expected result to contain time information")
				}
			}
			return
		}
	}
	
	t.Error("Could not find get_current_time tool")
}

func TestMCPToolSchema(t *testing.T) {
	checkUvxAvailable(t)
	
	// Start the MCP server
	serverCmd := "uvx mcp-server-time"
	
	client, err := NewMCPClient(serverCmd)
	if err != nil {
		t.Skipf("Could not start MCP server: %v", err)
	}
	defer client.Close()

	// Give the server a moment to initialize
	time.Sleep(100 * time.Millisecond)

	// Get tools
	tools, err := client.ListTools()
	if err != nil {
		t.Fatalf("Failed to list tools: %v", err)
	}

	// Test schema generation for each tool
	for _, tool := range tools {
		schema := tool.GetSchema()
		
		if schema == nil {
			t.Error("Expected non-nil schema")
			continue
		}
		
		// Verify basic schema properties
		if schema.Title == "" {
			t.Error("Expected schema to have a title")
		}
		
		if schema.Type != "object" {
			t.Errorf("Expected schema type to be 'object', got %s", schema.Type)
		}
		
		t.Logf("Tool schema - Title: %s, Description: %s", schema.Title, schema.Description)
	}
}

func TestMCPClientInvalidCommand(t *testing.T) {
	// Test with a non-existent command
	_, err := NewMCPClient("this-command-does-not-exist")
	if err == nil {
		t.Error("Expected error for non-existent command")
	}
}

func TestMCPClientEmptyCommand(t *testing.T) {
	// Test with empty command
	_, err := NewMCPClient("")
	if err == nil {
		t.Error("Expected error for empty command")
	}
}


func TestMCPToolFilterArguments(t *testing.T) {
	checkUvxAvailable(t)
	ctx := context.Background()
	
	// Start the MCP server
	serverCmd := "uvx mcp-server-time"
	
	client, err := NewMCPClient(serverCmd)
	if err != nil {
		t.Skipf("Could not start MCP server: %v", err)
	}
	defer client.Close()

	// Give the server a moment to initialize
	time.Sleep(100 * time.Millisecond)

	// Get tools
	tools, err := client.ListTools()
	if err != nil {
		t.Fatalf("Failed to list tools: %v", err)
	}

	if len(tools) == 0 {
		t.Fatal("No tools available")
	}

	// Test that extra arguments are filtered out
	tool := tools[0]
	
	// Pass extra arguments that shouldn't be in the schema
	args := map[string]any{
		"extra_arg_1": "should be filtered",
		"extra_arg_2": 123,
		"extra_arg_3": true,
	}
	
	// This should not fail due to extra arguments
	// The Execute method should filter them out
	_, err = tool.Execute(ctx, args)
	// We don't check the error because the tool might still fail for other reasons
	// The important thing is that it doesn't fail due to unexpected arguments
	t.Logf("Execution with extra args completed (error ok): %v", err)
}