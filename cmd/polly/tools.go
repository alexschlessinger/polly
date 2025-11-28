package main

import (
	"fmt"

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

