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
	config *Config,
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

	result, err := tool.Execute(ctx, args)
	success := err == nil
	if err != nil {
		result = fmt.Sprintf("Error: %v", err)
	}

	// Add tool result to session
	session.AddMessage(messages.ChatMessage{
		Role:       messages.MessageRoleTool,
		Content:    result,
		ToolCallID: toolCall.ID,
	})
	
	return success
}


// processToolCalls processes all tool calls in the response
func processToolCalls(
	ctx context.Context,
	toolCalls []messages.ChatMessageToolCall,
	registry *tools.ToolRegistry,
	session sessions.Session,
	config *Config,
	statusLine StatusHandler,
) {
	for _, toolCall := range toolCalls {
		// Ignore return value in regular mode - status is shown in title bar
		_ = executeToolCall(ctx, toolCall, registry, session, config, statusLine)
	}
}
