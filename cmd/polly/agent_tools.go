package main

import (
	"fmt"
	"slices"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/tools"
)

// Agent tools are persisted as selections, never loaded as native constructors.
// Keeping these markers separate from the registry preserves --tool restrictions
// when a session is reopened, including a session selecting only agent tools.
var restoreToolLoader = tools.ToolLoaderInfo{Name: llm.BuiltinRestoreArtifact, Type: "agent", Source: "builtin"}

func loadSelectedTools(registry *tools.ToolRegistry, sources []string) ([]tools.ToolLoaderInfo, error) {
	restore := false
	for _, source := range sources {
		if source == llm.BuiltinRestoreArtifact {
			restore = true
			continue
		}
		if _, err := registry.LoadToolAuto(source); err != nil {
			return nil, fmt.Errorf("failed to load tool %s: %w", source, err)
		}
	}
	loaders := registry.GetActiveToolLoaders()
	if restore {
		loaders = append(loaders, restoreToolLoader)
	}
	return loaders, nil
}

func selectedAgentConfig(settings Settings, loaders []tools.ToolLoaderInfo) llm.AgentConfig {
	config := settings.agentConfig()
	// Recall remains available independently of explicit mutation selections.
	config.Builtins = []string{llm.BuiltinListArtifacts, llm.BuiltinReadArtifact, llm.BuiltinReadTranscript}
	if slices.Contains(loaders, restoreToolLoader) {
		config.Builtins = append(config.Builtins, llm.BuiltinRestoreArtifact)
	}
	return config
}

// A reconfigured turn must carry the session's private tool selection into
// swarm defaults rather than re-enabling an omitted tool from host defaults.
func (s *conversationState) agentConfig(settings Settings) llm.AgentConfig {
	config := settings.agentConfig()
	config.Builtins = slices.Clone(s.agentBuiltins)
	return config
}
