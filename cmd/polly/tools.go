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

// loadTools loads tools based on ToolLoaderInfo list
func loadTools(loaderInfos []tools.ToolLoaderInfo) (*tools.ToolRegistry, error) {
	registry := tools.NewToolRegistry(nil)
	
	if len(loaderInfos) == 0 {
		return registry, nil
	}
	
	// Group tools by source for efficient loading
	shellTools := make(map[string]bool)
	mcpServers := make(map[string][]string) // server -> list of tool names
	
	for _, info := range loaderInfos {
		switch info.Type {
		case "shell":
			shellTools[info.Source] = true
		case "mcp":
			if mcpServers[info.Source] == nil {
				mcpServers[info.Source] = []string{}
			}
			mcpServers[info.Source] = append(mcpServers[info.Source], info.Name)
		}
		// Native tools are registered automatically
	}
	
	// Load shell tools
	for path := range shellTools {
		if err := registry.LoadShellTool(path); err != nil {
			return nil, fmt.Errorf("failed to load shell tool %s: %w", path, err)
		}
	}
	
	// Load MCP servers with filtering - only load the specific tools that were persisted
	for server, toolNames := range mcpServers {
		if err := registry.LoadMCPServerWithFilter(server, toolNames); err != nil {
			return nil, fmt.Errorf("failed to load MCP server %s: %w", server, err)
		}
	}
	
	return registry, nil
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
