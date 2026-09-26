package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/skills"
	"github.com/alexschlessinger/pollytool/swarm"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
	"github.com/urfave/cli/v3"
)

type conversationState struct {
	workspaceChanges *workspaceChangeState
	swarm            *swarm.Runtime
	sessionStore     sessions.SessionStore
	session          sessions.Session
	// settings are this session's own: resolved from its stored metadata
	// when it was opened, changed by /set, and read by every turn on it.
	settings        Settings
	metadataBaseURL string
	agent           *llm.Agent
	artifactStore   artifacts.Store
	toolRegistry    *tools.ToolRegistry
	skillCatalog    *skills.Catalog
	skillRuntime    *tools.SkillRuntime
	skillSources    []string
	sandboxWarnings *broadWritablePathWarner
	// sandboxProbe is the deferred check that the sandbox backend can start a
	// command. It runs concurrently with the rest of the open so a session
	// appears without waiting on the spawn; every turn waits on it before its
	// first request (see executeTurnWithUserMessage), and an open consults it
	// when a tool that spawns while loading fails, so the sandbox diagnosis
	// wins over the raw load error.
	sandboxProbe *sandboxProbe
	// sandboxProfile is the workspace's sandbox profile as this session
	// applies it; nil under --nosandbox.
	sandboxProfile *sandboxProfileState
	// sandboxInit is the session's /sandbox-init once the user first runs it:
	// the sandbox setup tools it added and whether its run is live. Only the
	// command sets it.
	sandboxInit *sandboxInit
	// instructionWarnings is the last set of repository-instruction warnings
	// shown, so a persistent problem is reported once rather than every turn.
	instructionWarnings []string
	// displayContract is composed into the request's system message each turn;
	// it is capability-specific and never persisted (see display_contract.go).
	displayContract string
	// outputCapabilities is resolved once per process run so the model-facing
	// contract and the concrete line renderer cannot disagree.
	outputCapabilities outputCapabilities
	// memberUI, when set, takes the approvals of swarm members that run
	// outside a turn (launched by a command, or woken by peer mail): the
	// managed REPL binds the screen of the tab holding this session. turnUI
	// is the running turn's UI, the fallback for hosts without a screen.
	uiMu     sync.Mutex
	memberUI TurnUI
	turnUI   TurnUI
	// spend totals this session's cost since this process opened it,
	// including its swarm members' model calls.
	spend sessionSpend
}

// sessionContext also supports display-only states that do not own a session.
func (s *conversationState) sessionContext() context.Context {
	if s == nil || s.session == nil {
		return context.Background()
	}
	return s.session.Context()
}

func (s *conversationState) setMemberUI(ui TurnUI) {
	s.uiMu.Lock()
	defer s.uiMu.Unlock()
	s.memberUI = ui
}

func (s *conversationState) setTurnUI(ui TurnUI) {
	s.uiMu.Lock()
	defer s.uiMu.Unlock()
	s.turnUI = ui
}

// hostTurnUI is the UI a swarm member without a parent turn reports to, or
// nil when nothing on this host can take an approval right now.
func (s *conversationState) hostTurnUI() TurnUI {
	s.uiMu.Lock()
	defer s.uiMu.Unlock()
	if s.memberUI != nil {
		return s.memberUI
	}
	return s.turnUI
}

// sessionOpener lets the managed REPL open sessions while it runs. prepare
// resolves the target session's stored settings against the launch settings
// and may reset that session's history when --system differs, reporting that
// through notify; it runs on the UI goroutine. open builds the runtime from
// those settings, as a generated session when auto is set, and may run off
// the UI goroutine. newName picks an unused generated session name.
type sessionOpener struct {
	prepare func(ctx context.Context, name string, notify func(string)) (string, Settings, error)
	open    func(ctx context.Context, name string, settings Settings, auto bool) (*conversationState, error)
	newName func(ctx context.Context) (string, error)
}

// effectiveTools includes the agent's private built-ins for display and
// request projection. Persistence and tool loading use toolRegistry instead.
func (s *conversationState) effectiveTools() *tools.ToolRegistry {
	if s.agent != nil {
		return s.agent.ToolRegistry()
	}
	return s.toolRegistry
}

func (s *conversationState) Close() error {
	var errs []error
	if s.workspaceChanges != nil {
		s.workspaceChanges.stopStartup()
	}
	if s.swarm != nil {
		if err := s.swarm.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if s.agent != nil {
		if err := s.agent.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if s.toolRegistry != nil {
		if err := s.toolRegistry.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if s.workspaceChanges != nil {
		errs = append(errs, s.workspaceChanges.close())
	}
	if err := s.sandboxProfile.Close(); err != nil {
		errs = append(errs, err)
	}
	if s.session != nil {
		if err := s.session.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// updateMetadata runs one read-modify-write cycle on the session's metadata.
// GetMetadata never returns a nil value without an error, so mutate always
// receives a usable object.
func updateMetadata(ctx context.Context, session sessions.Session, mutate func(*sessions.Metadata)) error {
	md, err := session.GetMetadata(ctx)
	if err != nil {
		return err
	}
	mutate(md)
	return session.SetMetadata(ctx, md)
}

// closeAfterError releases a resource acquired before cause occurred, joining
// a failed close (labelled what) onto cause so neither failure is lost.
func closeAfterError(c io.Closer, what string, cause error) error {
	if c == nil {
		return cause
	}
	if err := c.Close(); err != nil {
		return errors.Join(cause, fmt.Errorf("close %s: %w", what, err))
	}
	return cause
}

// changedInstructionWarnings returns the repository-instruction warnings to
// show this turn. The files are read again every turn, so a warning repeats
// only once the set changes.
func (s *conversationState) changedInstructionWarnings(warnings []string) []string {
	if slices.Equal(warnings, s.instructionWarnings) {
		return nil
	}
	s.instructionWarnings = warnings
	return warnings
}

// drainSandboxWarnings and sandboxWarningNotify are safe on a nil state and
// on a state without a warner (Drain and Notify take a nil receiver).
func (s *conversationState) drainSandboxWarnings() []string {
	if s == nil {
		return nil
	}
	return s.sandboxWarnings.Drain()
}

func (s *conversationState) sandboxWarningNotify() <-chan struct{} {
	if s == nil {
		return nil
	}
	return s.sandboxWarnings.Notify()
}

// conversationOpener builds conversation runtimes for one process from the
// launch config, provider client, session store, and parsed command that
// every open reads. A nil llmClient constructs one from the environment.
type conversationOpener struct {
	config       *Config
	llmClient    *llm.MultiPass
	sessionStore sessions.SessionStore
	cmd          *cli.Command
	// displayContract and outputCapabilities describe the run's fixed output
	// surface (see display_contract.go); every opened state carries them.
	displayContract    string
	outputCapabilities outputCapabilities
}

// openNew sets up everything a conversation needs: the session, its tool
// registry and skill runtime, and the agent, with preparation notices going
// to stderr. Session metadata is staged on one object and written once, so
// a failure part-way through persists nothing. On error every acquired
// resource is released and nil is returned.
func (o *conversationOpener) openNew(ctx context.Context, contextID string, autoContext bool) (*conversationState, error) {
	contextID, settings, err := o.prepare(ctx, contextID, notifyStderr)
	if err != nil {
		return nil, err
	}
	return o.open(ctx, contextID, settings, autoContext)
}

func notifyStderr(line string) {
	fmt.Fprintln(os.Stderr, line)
}

// open is the second half of openNew: it acquires contextID and builds its
// runtime from the settings prepare resolved for it. It only reads config,
// so the managed REPL may run it off the UI goroutine while the visible
// session keeps serving input.
func (o *conversationOpener) open(ctx context.Context, contextID string, settings Settings, autoContext bool) (state *conversationState, retErr error) {
	config, llmClient, sessionStore := o.config, o.llmClient, o.sessionStore
	if settings.ModelHost != "" && !strings.HasPrefix(settings.Model, "openrouter/") {
		return nil, fmt.Errorf("modelhost is supported only for OpenRouter")
	}
	if llmClient == nil {
		llmClient = newLLMRouter()
	}

	if cache, ok := sessionStore.(llm.ModelMetadataCache); ok {
		llmClient.SetModelMetadataCache(cache)
	}
	// Warm the model's metadata while the rest of the open runs, after the
	// cache is attached so a cached entry is read rather than refetched. The
	// first turn's context-window lookup joins this fetch, which the metadata
	// service bounds, instead of starting its own.
	prefetch := modelMetadataTarget(settings.Model, settings.ModelHost, config.BaseURL)
	go func() { _, _ = llmClient.LookupModel(context.WithoutCancel(ctx), prefetch, false) }()

	// Get or create the session early so persisted skill sources can be read.
	session, err := getOrCreateSession(ctx, sessionStore, contextID, needsFileStore(config, contextID), autoContext)
	if err != nil {
		return nil, err
	}
	var toolRegistry *tools.ToolRegistry
	var changeTracker io.Closer
	defer func() {
		if retErr == nil {
			return
		}
		if changeTracker != nil {
			retErr = closeAfterError(changeTracker, "change tracker", retErr)
		}
		if toolRegistry != nil {
			retErr = closeAfterError(toolRegistry, "tool registry", retErr)
		}
		retErr = closeAfterError(session, "session", retErr)
	}()
	metadata, err := session.GetMetadata(ctx)
	if err != nil {
		return nil, fmt.Errorf("read context metadata: %w", err)
	}
	if metadata.SwarmID != "" {
		return nil, fmt.Errorf("this swarm member is inspected through /sessions and resumed by its parent %q; independent execution would lose its worktree binding", metadata.Parent)
	}

	// Extra read-only directories merge rather than replace: the --add-dir
	// entries are validated strictly now (a bad one fails the open before the
	// session runs), the persisted ones leniently so a directory deleted
	// between sessions stays on the record, and the merged list is both
	// persisted by the updateContextInfo write below and granted to the
	// sandbox. There is deliberately no settingSpecs row: flagged rows
	// replace the stored value, while this feature must merge.
	extraReadDirs, err := resolveSessionExtraReadDirs(config, metadata)
	if err != nil {
		return nil, err
	}
	metadata.ExtraReadDirs = extraReadDirs

	// Discover skills before building the runtime tool registry, passing the
	// persisted sources so --skill is restored on resume; new sources are
	// staged for the single write below.
	skillResult, err := loadSkillCatalog(config, settings.SkillDirs, metadata.SkillSources)
	if err != nil {
		return nil, err
	}
	if len(skillResult.sources) > 0 {
		metadata.SkillSources = skillResult.sources
	}

	privatePaths, err := sessionPrivatePaths(sessionStore)
	if err != nil {
		return nil, err
	}
	sandboxWarnings := newBroadWritablePathWarner()
	registryOpts, probe, sandboxProfile, err := sandboxRegistryOptionsWithWarnings(config, sandboxWarnings, skillCatalogRoots(skillResult), extraReadDirs, privatePaths...)
	if err != nil {
		return nil, err
	}
	defer func() {
		if state == nil {
			_ = sandboxProfile.Close()
		}
	}()
	// A tool that spawns while loading (a shell tool's --schema, a stdio MCP
	// server) runs under the backend the probe is checking and fails first
	// when that backend cannot start. The probe's diagnosis names the escape
	// hatch, so it wins over the raw load error; a load that succeeds never
	// waits.
	loadErr := func(err error) error {
		if probeErr := probe.wait(ctx); probeErr != nil {
			return probeErr
		}
		return err
	}
	if len(config.Tools) > 0 {
		// Command-line tools replace the session's persisted tools.
		toolRegistry = tools.NewToolRegistry(nil, registryOpts...)
		for _, source := range config.Tools {
			if _, err := toolRegistry.LoadToolAuto(source); err != nil {
				return nil, loadErr(fmt.Errorf("failed to load tool %s: %w", source, err))
			}
		}
		metadata.ActiveTools = toolRegistry.GetActiveToolLoaders()
	} else {
		toolRegistry, err = tools.LoadRegistry(metadata.ActiveTools, registryOpts...)
		if err != nil {
			return nil, loadErr(err)
		}
	}
	tracker := installChangeTracker(toolRegistry, privatePaths)
	if tracker != nil {
		changeTracker = tracker
	}
	skillRuntime, err := newSkillRuntime(skillResult.catalog, toolRegistry)
	if err != nil {
		return nil, err
	}
	if err := restoreActiveSkills(metadata, skillRuntime); err != nil {
		return nil, err
	}
	if err := autoActivateSkills(skillResult.autoActivate, skillRuntime); err != nil {
		return nil, err
	}
	if err := updateContextInfo(ctx, session, metadata, &settings); err != nil {
		return nil, err
	}

	artifactStore := session.ArtifactStore()
	agentConfig := settings.agentConfig()
	agentConfig.ArtifactStore = artifactStore
	if coord, ok := session.(sessions.CoordinationSession); ok {
		agentConfig.OpenArtifact = coord.OpenPublishedArtifact
	}
	agent := llm.NewAgent(llmClient, toolRegistry, agentConfig)
	state = &conversationState{
		sessionStore:       sessionStore,
		session:            session,
		settings:           settings,
		metadataBaseURL:    config.BaseURL,
		agent:              agent,
		artifactStore:      artifactStore,
		toolRegistry:       toolRegistry,
		skillCatalog:       skillResult.catalog,
		skillRuntime:       skillRuntime,
		skillSources:       skillResult.sources,
		sandboxWarnings:    sandboxWarnings,
		sandboxProbe:       probe,
		sandboxProfile:     sandboxProfile,
		displayContract:    o.displayContract,
		outputCapabilities: o.outputCapabilities,
	}
	registerSessionTitleTool(state)
	registerThemeTool(state)
	if err := registerSwarm(state, config, llmClient); err != nil {
		return nil, err
	}
	if o.outputCapabilities.surface == outputSurfaceManagedTUI {
		state.startWorkspaceChanges(state.sessionContext(), tracker)
	} else {
		state.initializeWorkspaceChanges(ctx, tracker)
	}
	return state, nil
}

// resolveSessionExtraReadDirs merges the --add-dir entries into the session's
// persisted extra read-only directories. The flagged entries are validated
// strictly against the working directory; the persisted entries are
// re-canonicalized leniently, so a directory deleted between sessions stays
// on the record (the sandbox's own construction freeze keeps it unreadable
// until it is recreated and the session resumed again) while an entry that no
// longer canonicalizes at all — it now sits under the home directory, or
// masks a credential path — is dropped: it could never be granted again, and
// keeping it would make the session unopenable.
func resolveSessionExtraReadDirs(config *Config, metadata *sessions.Metadata) ([]string, error) {
	flagged, err := resolveConfigAddDirs(config)
	if err != nil {
		return nil, err
	}
	existing := make([]string, 0, len(metadata.ExtraReadDirs))
	for _, path := range metadata.ExtraReadDirs {
		canonical, err := sandbox.CanonicalizeExtraReadDir(path)
		if err != nil {
			continue
		}
		existing = append(existing, canonical)
	}
	return sandbox.MergeExtraReadDirs(existing, flagged), nil
}

// prepare resolves the settings contextID will run with: the launch
// settings, with every stored value that no flag overrode restored from the
// session's metadata. When an explicit --system differs from the stored
// prompt the session's history is reset, reported through notify so the
// managed REPL can show it without writing to a terminal it owns. The
// returned name is the session's stored name when it exists.
func (o *conversationOpener) prepare(ctx context.Context, contextID string, notify func(string)) (string, Settings, error) {
	config, sessionStore, cmd := o.config, o.sessionStore, o.cmd
	settings := config.Launch.clone()
	if contextID == "" {
		return contextID, settings, nil
	}

	contextInfo, err := sessionStore.GetMetadata(ctx, contextID)
	if errors.Is(err, sessions.ErrSessionNotFound) {
		return contextID, settings, nil
	}
	if err != nil {
		return "", Settings{}, fmt.Errorf("read context metadata: %w", err)
	}

	// Persisted settings are authoritative for an existing session. Zero
	// and empty values are intentional settings too, so copy every field
	// that was not explicitly overridden on this command line; environment
	// and configuration-file values are defaults for new sessions only.
	for _, spec := range settingSpecs {
		if spec.flagged() && !flagGiven(cmd, spec.key) {
			spec.fromMeta(&settings, contextInfo)
		}
	}

	if flagGiven(cmd, "model") && !flagGiven(cmd, "modelhost") {
		settings.ModelHost = ""
	}
	if settings.ModelHost != "" && !strings.HasPrefix(settings.Model, "openrouter/") {
		return "", Settings{}, fmt.Errorf("modelhost is supported only for OpenRouter")
	}
	if flagGiven(cmd, "system") && cmd.String("system") != contextInfo.SystemPrompt {
		notify("System prompt changed, resetting conversation...")
		// Store the explicitly changed prompt before Clear: Clear rebuilds
		// the system message from session metadata, including the meaningful
		// empty prompt case.
		if err := resetContextWithSystemPrompt(ctx, sessionStore, contextID, settings.SystemPrompt); err != nil {
			return "", Settings{}, fmt.Errorf("failed to reset context: %w", err)
		}
	}
	return contextID, settings, nil
}

// applyFlagSettings copies only explicitly-set CLI flags onto md, so a plain
// --reset keeps stored settings instead of replacing them with defaults.
func applyFlagSettings(md *sessions.Metadata, settings *Settings, cmd *cli.Command) {
	if flagGiven(cmd, "model") && !flagGiven(cmd, "modelhost") {
		md.ModelHost = ""
	}
	for _, spec := range settingSpecs {
		if spec.flagSet(cmd) {
			spec.toMeta(settings, md)
		}
	}
}

// updateContextInfo writes the resolved settings onto md, the metadata staged
// by conversationOpener.open, and persists it: every flagged row, since the
// resolved settings hold the stored value unless a flag overrode it (see
// conversationOpener.prepare), so the copy is a no-op for an untouched row and an
// override for a set flag. Name and LastUsed are storage-owned: SetMetadata
// overwrites both, so they are not written here.
func updateContextInfo(ctx context.Context, session sessions.Session, md *sessions.Metadata, settings *Settings) error {
	for _, spec := range settingSpecs {
		if spec.flagged() {
			spec.toMeta(settings, md)
		}
	}
	return session.SetMetadata(ctx, md)
}

// createCompletionRequest builds an LLM completion request from the process
// config and the session's settings.
func createCompletionRequest(config *Config, settings *Settings, history []messages.ChatMessage, registry *tools.ToolRegistry, skillCatalog *skills.Catalog, schema *llm.Schema) *llm.CompletionRequest {
	// Parse thinking effort - already validated at config parsing time
	thinkingEffort, _ := llm.ParseThinkingEffort(settings.ThinkingEffort)

	return &llm.CompletionRequest{
		BaseURL:          config.BaseURL,
		Timeout:          config.Timeout,
		Deadline:         config.Deadline,
		Temperature:      llm.Float32Ptr(float32(settings.Temperature)),
		Model:            settings.Model,
		ModelHost:        settings.ModelHost,
		MaxTokens:        settings.MaxTokens,
		MaxContextTokens: settings.MaxHistoryTokens,
		Messages:         history,
		Skills:           skillCatalog,
		Tools:            registry.All(),
		ResponseSchema:   schema,
		ThinkingEffort:   thinkingEffort,
		Fast:             settings.Fast,
	}
}

// generateSessionName picks a generated session name no session in store
// uses.
func generateSessionName(ctx context.Context, store sessions.SessionStore) (string, error) {
	name, err := generateContextName(func(name string) (bool, error) {
		return store.Exists(ctx, name)
	})
	if err != nil {
		return "", fmt.Errorf("failed to generate context name: %w", err)
	}
	return name, nil
}
