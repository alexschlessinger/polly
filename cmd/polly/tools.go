package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/pkdindustries/pollytool/messages"
	"github.com/pkdindustries/pollytool/sessions"
	"github.com/pkdindustries/pollytool/tools"
)

// loadTools loads all configured tools (shell tools and MCP servers)
func loadTools(config *Config) *tools.ToolRegistry {
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
				log.Printf("Warning: failed to connect to MCP server %s: %v", server, err)
				continue
			}
			mcpTools, err := mcpClient.ListTools()
			if err != nil {
				log.Printf("Warning: failed to list tools from MCP server %s: %v", server, err)
				continue
			}
			allTools = append(allTools, mcpTools...)
		}
	}

	return tools.NewToolRegistry(allTools)
}

// executeToolCall executes a single tool call and returns the result
func executeToolCall(
	ctx context.Context,
	toolCall messages.ChatMessageToolCall,
	registry *tools.ToolRegistry,
	session sessions.Session,
	config *Config,
	statusLine *Status,
) {
	// Parse arguments from JSON string
	var args map[string]any
	if err := json.Unmarshal([]byte(toolCall.Arguments), &args); err != nil {
		log.Printf("Failed to parse tool call arguments: %v", err)
		session.AddMessage(messages.ChatMessage{
			Role:       messages.MessageRoleTool,
			Content:    fmt.Sprintf("Error parsing arguments: %v", err),
			ToolCallID: toolCall.ID,
		})
		return
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
		return
	}

	log.Printf("%s %s", toolCall.Name, toolCall.Arguments)
	showToolExecutionInfo(toolCall.Name, args, config, statusLine)

	result, err := tool.Execute(ctx, args)
	if err != nil {
		result = fmt.Sprintf("Error: %v", err)
	}

	// Add tool result to session
	session.AddMessage(messages.ChatMessage{
		Role:       messages.MessageRoleTool,
		Content:    result,
		ToolCallID: toolCall.ID,
	})
}

// showToolExecutionInfo displays tool execution information to the user
func showToolExecutionInfo(toolName string, args map[string]any, config *Config, statusLine *Status) {
	if statusLine != nil {
		statusLine.ShowToolExecution(toolName, args)
	}
	// No fallback output - only terminal title updates
}

// processToolCalls processes all tool calls in the response
func processToolCalls(
	ctx context.Context,
	toolCalls []messages.ChatMessageToolCall,
	registry *tools.ToolRegistry,
	session sessions.Session,
	config *Config,
	statusLine *Status,
) {
	for _, toolCall := range toolCalls {
		executeToolCall(ctx, toolCall, registry, session, config, statusLine)
	}
}
