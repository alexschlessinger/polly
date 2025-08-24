package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
)

// loadTools loads all configured tools (shell tools and MCP servers)
func loadTools(config *Config) (*tools.ToolRegistry, error) {
	var allTools []tools.Tool

	// Load shell tools
	if len(config.ToolPaths) > 0 {
		shellTools, err := tools.LoadShellTools(config.ToolPaths)
		if err != nil {
			log.Printf("Warning: failed to load some shell tools: %v", err)
		}
		allTools = append(allTools, shellTools...)
	}

	// Load MCP servers
	if len(config.MCPServers) > 0 {
		for _, server := range config.MCPServers {
			mcpClient, err := tools.NewMCPClient(server)
			if err != nil {
				return nil, err
			}
			mcpTools, err := mcpClient.ListTools()
			if err != nil {
				return nil, err
			}
			allTools = append(allTools, mcpTools...)
		}
	}

	return tools.NewToolRegistry(allTools), nil
}

// executeToolCall executes a single tool call and returns the result
// Returns true if the tool executed successfully, false if there was an error
func executeToolCall(
	ctx context.Context,
	toolCall messages.ChatMessageToolCall,
	registry *tools.ToolRegistry,
	session sessions.Session,
	statusLine StatusHandler,
) bool {
	// Parse arguments from JSON string
	var args map[string]any
	if err := json.Unmarshal([]byte(toolCall.Arguments), &args); err != nil {
		log.Printf("Failed to parse tool call arguments: %v", err)
		session.AddMessage(messages.ChatMessage{
			Role:       messages.MessageRoleTool,
			Content:    fmt.Sprintf("Error parsing arguments: %v", err),
			ToolCallID: toolCall.ID,
		})
		return false
	}

	// Execute tool
	tool, exists := registry.Get(toolCall.Name)
	if !exists {
		log.Printf("Tool not found: %s", toolCall.Name)
		session.AddMessage(messages.ChatMessage{
			Role:       messages.MessageRoleTool,
			Content:    fmt.Sprintf("Error: tool not found: %s", toolCall.Name),
			ToolCallID: toolCall.ID,
		})
		return false
	}

	log.Printf("%s %s", toolCall.Name, toolCall.Arguments)

	// Show tool execution status
	if statusLine != nil {
		statusLine.ShowToolCall(toolCall.Name)
	}

	// Apply timeout from session metadata (always has a value due to defaults)
	metadata := session.GetMetadata()
	timeout := metadata.ToolTimeout

	executeCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		executeCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	result, err := tool.Execute(executeCtx, args)
	success := err == nil
	if err != nil {
		// Check if it was a timeout
		if executeCtx.Err() == context.DeadlineExceeded {
			result = fmt.Sprintf("Error: tool execution timed out after %v", timeout)
		} else {
			result = fmt.Sprintf("Error: %v", err)
		}
	}

	// Add tool result to session
	session.AddMessage(messages.ChatMessage{
		Role:       messages.MessageRoleTool,
		Content:    result,
		ToolCallID: toolCall.ID,
	})

	return success
}
