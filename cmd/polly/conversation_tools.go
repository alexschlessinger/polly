package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/worktree"
)

// installToolRuntime runs before the session is exposed, or on an idle
// frontend after an explicit transition out of draft mode. Build the
// replacement before installing it: a failed loader must leave the session
// usable for inspecting the plan and its saved settings.
func (o *conversationOpener) installToolRuntime(ctx context.Context, state *conversationState, client *llm.MultiPass, md *sessions.Metadata, draft bool) (retErr error) {
	config := o.config
	skillResult := &skillCatalogResult{}
	var err error
	if !draft {
		skillResult, err = loadSkillCatalog(config, state.settings.SkillDirs, md.SkillSources, state.storage.privatePaths...)
		if err != nil {
			return err
		}
		if len(skillResult.sources) > 0 {
			md.SkillSources = skillResult.sources
		}
	}
	registryOpts, probe, profile, err := sandboxRegistryOptionsWithWarnings(config, state.sandboxWarnings, skillResult, md.ExtraReadDirs, state.storage.privatePaths...)
	if err != nil {
		return err
	}
	var registry *tools.ToolRegistry
	var tracker *worktree.ChangeTracker
	defer func() {
		if retErr == nil {
			return
		}
		if tracker != nil {
			retErr = closeAfterError(tracker, "change tracker", retErr)
		}
		if registry != nil {
			retErr = closeAfterError(registry, "tool registry", retErr)
		}
		retErr = errors.Join(retErr, profile.Close())
	}()

	selected := md.ActiveTools
	switch {
	case draft:
		// Do not load a shell tool's schema, connect MCP, resolve explicit
		// skill sources, or start swarm/worktree recovery while drafting.
		// Keep their configuration in metadata for the execution runtime.
		selected = draftReaderSelections(config.Tools, selected)
		registry, err = tools.LoadRegistry(selected, registryOpts...)
	case len(config.Tools) != 0:
		registry = tools.NewToolRegistry(nil, registryOpts...)
		selected, err = loadSelectedTools(registry, config.Tools)
		md.ActiveTools = selected
	default:
		registry, err = tools.LoadRegistry(selected, registryOpts...)
	}
	if err != nil {
		// A tool that spawns during startup may report the same failure
		// that the probe diagnoses. Prefer the sandbox diagnostic then.
		if probeErr := probe.wait(ctx); probeErr != nil {
			return probeErr
		}
		return err
	}
	var skillRuntime *tools.SkillRuntime
	if !draft {
		if state.outputCapabilities.surface == outputSurfaceManagedTUI {
			tracker = installChangeTracker(registry, state.storage.privatePaths)
		}
		skillRuntime, err = newSkillRuntime(skillResult.catalog, registry)
		if err != nil {
			return err
		}
		if err := restoreActiveSkills(md, skillRuntime); err != nil {
			return err
		}
		if err := autoActivateSkills(skillResult.autoActivate, skillRuntime); err != nil {
			return err
		}
	}
	if err := updateContextInfo(ctx, state.session, md, &state.settings); err != nil {
		return err
	}

	agentConfig := selectedAgentConfig(state.settings, selected)
	runtime := state.swarm // nil before initial installation and during a resumed draft
	if !draft {
		runtime, err = buildConversationSwarm(state, config, client, md.Parent, registry, agentConfig)
		if err != nil {
			return err
		}
	}
	agentConfig.ArtifactStore = state.artifactStore
	if coord, ok := state.session.(sessions.CoordinationSession); ok {
		agentConfig.OpenArtifact = coord.OpenPublishedArtifact
	}
	agent := llm.NewAgent(client, registry, agentConfig)
	oldAgent, oldRegistry, oldProfile := state.agent, state.toolRegistry, state.sandboxProfile
	state.agent, state.agentBuiltins = agent, agentConfig.Builtins
	state.toolRegistry, state.swarm = registry, runtime
	state.skillCatalog, state.skillRuntime, state.skillSources = skillResult.catalog, skillRuntime, skillResult.sources
	state.sandboxProfile, state.sandboxProbe = profile, probe
	if !draft {
		registerSessionTitleTool(state)
		registerThemeTool(state)
		registerAskUserTool(state)
		state.startWorkspaceChanges(state.sessionContext(), tracker)
	}
	// A successful replacement owns a fresh set of resources; the old ones
	// belonged to the inactive draft frontend, not to the session lease.
	var cleanup error
	if oldAgent != nil {
		cleanup = closeAfterError(oldAgent, "draft agent", cleanup)
	}
	if oldRegistry != nil {
		cleanup = closeAfterError(oldRegistry, "draft tool registry", cleanup)
	}
	if err := errors.Join(cleanup, oldProfile.Close()); err != nil {
		state.sandboxWarnings.Note(err.Error())
	}
	return nil
}

func draftReaderSelections(flags []string, selected []tools.ToolLoaderInfo) []tools.ToolLoaderInfo {
	var readers []tools.ToolLoaderInfo
	isReader := func(name string) bool { return name == "read_file" || name == "list_dir" }
	if len(flags) != 0 {
		for _, name := range flags {
			if isReader(name) {
				readers = append(readers, tools.ToolLoaderInfo{Name: name, Type: "native", Source: name})
			}
		}
		return readers
	}
	for _, loader := range selected {
		if loader.Type == "native" && isReader(loader.Name) {
			readers = append(readers, loader)
		}
	}
	return readers
}

// restoreExecutionTools is used only by the idle frontend, after committing
// an exit or acceptance. Neither merely opening a draft nor reviewing it can
// invoke the deferred loader.
func (s *conversationState) restoreExecutionTools(ctx context.Context) error {
	if s.resumeExecution == nil {
		return nil
	}
	if err := s.resumeExecution(ctx); err != nil {
		return fmt.Errorf("execution tools are still deferred: %w", err)
	}
	s.resumeExecution = nil
	return nil
}
