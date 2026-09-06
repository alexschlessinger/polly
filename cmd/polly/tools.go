package main

import (
	"errors"
	"fmt"

	"github.com/alexschlessinger/pollytool/tools"
)

// loadTools loads tools based on ToolLoaderInfo list
func loadTools(loaderInfos []tools.ToolLoaderInfo, opts ...tools.RegistryOption) (*tools.ToolRegistry, error) {
	registry := tools.NewToolRegistry(nil, opts...)

	if len(loaderInfos) == 0 {
		return registry, nil
	}

	// Group tools by source for efficient loading
	shellTools := make(map[string]bool)
	mcpServers := make(map[string][]string) // server -> list of tool names
	nativeTools := make(map[string]bool)

	for _, info := range loaderInfos {
		switch info.Type {
		case "shell":
			shellTools[info.Source] = true
		case "mcp":
			if mcpServers[info.Source] == nil {
				mcpServers[info.Source] = []string{}
			}
			mcpServers[info.Source] = append(mcpServers[info.Source], info.Name)
		case "native":
			nativeTools[info.Name] = true
		}
	}

	// Load shell tools
	for path := range shellTools {
		if _, err := registry.LoadShellTool(path); err != nil {
			return nil, fmt.Errorf("failed to load shell tool %s: %w", path, err)
		}
	}

	// Load MCP servers with filtering - only load the specific tools that were persisted
	for server, toolNames := range mcpServers {
		if err := registry.LoadMCPServerWithFilter(server, toolNames); err != nil {
			return nil, fmt.Errorf("failed to load MCP server %s: %w", server, err)
		}
	}

	// Load native tools
	for name := range nativeTools {
		if _, err := registry.LoadToolAuto(name); err != nil {
			// A saved session may have been created on a machine with zg.
			// Keep its selection persisted, but omit the unavailable tool now.
			if errors.Is(err, tools.ErrSearchFilesUnavailable) {
				continue
			}
			return nil, fmt.Errorf("failed to load native tool %s: %w", name, err)
		}
	}

	return registry, nil
}
