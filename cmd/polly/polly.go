package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alexschlessinger/pollytool/internal/log"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/skills"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
	"github.com/urfave/cli/v3"
)

func main() {
	command := getCommand()
	if err := command.Run(context.Background(), os.Args); err != nil {
		var ee *exitError
		if errors.As(err, &ee) {
			if ee.err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", ee.err)
			}
			cleanupAndExit(ee.code)
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		cleanupAndExit(1)
	}
}

type commandRunner struct {
	ctx          context.Context
	cmd          *cli.Command
	config       *Config
	sessionStore sessions.SessionStore
	contextID    string
	// autoContext marks a generated REPL context name (no -c given): its
	// creation is silent and it is discarded on exit if no turn ever ran.
	autoContext bool
}

var newSandbox = sandbox.New

type conversationMode int

const (
	conversationModeOneShot conversationMode = iota
	conversationModeREPL
)

type conversationInput struct {
	mode   conversationMode
	prompt string
}

type conversationState struct {
	session         sessions.Session
	agent           *llm.Agent
	toolRegistry    *tools.ToolRegistry
	skillCatalog    *skills.Catalog
	skillRuntime    *tools.SkillRuntime
	skillSources    []string
	sandboxWarnings *broadWritablePathWarner
	// autoNamedContext marks a session under a generated name, so the REPL
	// can mention the name and how to keep or resume it.
	autoNamedContext bool
}

func (s *conversationState) Close() {
	if s.toolRegistry != nil {
		_ = s.toolRegistry.Close()
	}
	if s.session != nil {
		s.session.Close()
	}
}

func (s *conversationState) drainSandboxWarnings() []string {
	if s == nil || s.sandboxWarnings == nil {
		return nil
	}
	return s.sandboxWarnings.Drain()
}

func (s *conversationState) sandboxWarningNotify() <-chan struct{} {
	if s == nil || s.sandboxWarnings == nil {
		return nil
	}
	return s.sandboxWarnings.Notify()
}

func newCommandRunner(ctx context.Context, cmd *cli.Command) (*commandRunner, error) {
	config := parseConfig(cmd)

	log.InitLogger(config.Debug)

	contextID := config.ContextID
	if config.UseLastContext {
		contextID = ""
	}

	// An interactive REPL with no context gets a generated, file-backed one so
	// the conversation survives exit (resume with -L or -c <name>). Contexts
	// that never see a turn are discarded on exit.
	autoContext := contextID == "" && wantsAutoREPLContext(config)

	sessionStore, err := setupSessionStore(config, contextID, autoContext)
	if err != nil {
		return nil, fmt.Errorf("failed to create context store: %w", err)
	}

	if config.UseLastContext {
		contextID = sessionStore.GetLast()
		if contextID == "" {
			return nil, fmt.Errorf("no last context found")
		}
	}
	if autoContext {
		contextID = generateContextName(sessionStore.Exists)
	}

	return &commandRunner{
		ctx:          ctx,
		cmd:          cmd,
		config:       config,
		sessionStore: sessionStore,
		contextID:    contextID,
		autoContext:  autoContext,
	}, nil
}

// wantsAutoREPLContext reports whether this invocation will land in the
// interactive REPL with no context of its own: no prompt or piped stdin, no
// context-management flag, and a REPL-compatible flag set. Only those runs
// get an auto-generated persistent context.
func wantsAutoREPLContext(config *Config) bool {
	return !config.PromptSet &&
		!hasStdinData() &&
		!needsFileStore(config, "") &&
		validateREPLConfig(config) == nil
}

func (r *commandRunner) Run() error {
	handled, err := r.handleManagementFlags()
	if err != nil {
		return err
	}
	if handled {
		return nil
	}
	if err := validateSandboxFlagCombination(r.cmd, r.config); err != nil {
		return err
	}

	// Auto-generated contexts are created silently inside initializeSession;
	// the "Created new context" stderr notice would garble the TUI splash.
	if r.contextID != "" && !r.autoContext {
		contextID := checkAndPromptForMissingContext(r.sessionStore, r.contextID)
		if contextID == "" {
			return nil
		}
		r.contextID = contextID
	}

	return runConversation(r.ctx, r.config, r.sessionStore, r.contextID, r.autoContext, r.cmd)
}

func (r *commandRunner) handleManagementFlags() (bool, error) {
	cfg := r.config
	store := r.sessionStore

	if cfg.ResetContext != "" {
		return true, handleResetContext(store, cfg, r.cmd, cfg.ResetContext)
	}
	if cfg.ListContexts {
		return true, handleListContexts(store)
	}
	if cfg.ListSkills {
		return true, handleListSkills(cfg)
	}
	if cfg.DeleteContext != "" {
		return true, handleDeleteContext(store, cfg.DeleteContext)
	}
	if cfg.AddToContext {
		return true, handleAddToContext(store, cfg, r.contextID)
	}
	if cfg.PurgeAll {
		return true, handlePurgeAll(store)
	}
	if cfg.CreateContext != "" {
		return true, handleCreateContext(store, cfg, cfg.CreateContext)
	}
	if cfg.ShowContext != "" {
		return true, handleShowContext(store, cfg.ShowContext)
	}

	return false, nil
}

func runCommand(ctx context.Context, cmd *cli.Command) error {
	runner, err := newCommandRunner(ctx, cmd)
	if err != nil {
		return err
	}

	return runner.Run()
}

// initializeSession sets up everything needed for a conversation session
func initializeSession(config *Config, sessionStore sessions.SessionStore, contextID string, cmd *cli.Command, sandboxWarnings *broadWritablePathWarner) (string, sessions.Session, *llm.Agent, *tools.ToolRegistry, *skills.Catalog, *tools.SkillRuntime, *skillCatalogResult, error) {
	// Initialize conversation using helper function
	var err error
	contextID, _, err = initializeConversation(config, sessionStore, contextID, cmd)
	if err != nil {
		return "", nil, nil, nil, nil, nil, nil, err
	}

	// Load API keys
	apiKeys := loadAPIKeys()

	// Create LLM provider
	llmClient := llm.NewMultiPass(apiKeys)

	// Get or create session early so we can read persisted skill sources.
	needFileStore := needsFileStore(config, contextID)
	session := getOrCreateSession(sessionStore, contextID, needFileStore)
	metadata := session.GetMetadata()

	// Discover skills before building the runtime tool registry.
	// Pass persisted SkillSources so --skill is restored on session resume.
	skillResult, err := loadSkillCatalog(config, metadata.SkillSources)
	if err != nil {
		session.Close()
		return "", nil, nil, nil, nil, nil, nil, err
	}
	skillCatalog := skillResult.catalog

	// Persist skill sources for future session restores.
	if len(skillResult.sources) > 0 {
		metadata.SkillSources = skillResult.sources
		if err := session.SetMetadata(metadata); err != nil {
			session.Close()
			return "", nil, nil, nil, nil, nil, nil, fmt.Errorf("failed to persist skill sources: %w", err)
		}
	}

	registryOpts, err := sandboxRegistryOptionsWithWarnings(config, sandboxWarnings)
	if err != nil {
		session.Close()
		return "", nil, nil, nil, nil, nil, nil, err
	}

	// Handle command-line tools if provided - they replace session tools
	var toolRegistry *tools.ToolRegistry

	if len(config.Tools) > 0 {
		// Load command-line tools directly into the registry we'll use
		toolRegistry = tools.NewToolRegistry(nil, registryOpts...)
		for _, source := range config.Tools {
			_, err := toolRegistry.LoadToolAuto(source)
			if err != nil {
				session.Close()
				return "", nil, nil, nil, nil, nil, nil, fmt.Errorf("failed to load tool %s: %w", source, err)
			}
		}
		// Store the metadata for persistence
		metadata.ActiveTools = toolRegistry.GetActiveToolLoaders()
		if err := session.SetMetadata(metadata); err != nil {
			session.Close()
			return "", nil, nil, nil, nil, nil, nil, fmt.Errorf("failed to persist tool metadata: %w", err)
		}
	} else {
		// Load tools from session metadata
		toolRegistry, err = loadTools(metadata.ActiveTools, registryOpts...)
		if err != nil {
			session.Close()
			return "", nil, nil, nil, nil, nil, nil, err
		}
	}
	skillRuntime, err := newSkillRuntime(skillCatalog, toolRegistry)
	if err != nil {
		if toolRegistry != nil {
			_ = toolRegistry.Close()
		}
		session.Close()
		return "", nil, nil, nil, nil, nil, nil, err
	}
	if err := restoreActiveSkills(metadata, skillRuntime); err != nil {
		if toolRegistry != nil {
			_ = toolRegistry.Close()
		}
		session.Close()
		return "", nil, nil, nil, nil, nil, nil, err
	}
	if err := autoActivateSkills(skillResult.autoActivate, skillRuntime); err != nil {
		if toolRegistry != nil {
			_ = toolRegistry.Close()
		}
		session.Close()
		return "", nil, nil, nil, nil, nil, nil, err
	}

	// Update context info with current settings using helper function
	if err := updateContextInfo(session, config, cmd); err != nil {
		if toolRegistry != nil {
			_ = toolRegistry.Close()
		}
		session.Close()
		return "", nil, nil, nil, nil, nil, nil, err
	}

	// Create the agent with the tool registry
	agent := llm.NewAgent(llmClient, toolRegistry, llm.AgentConfig{
		MaxIterations: config.MaxIterations,
		ToolTimeout:   config.ToolTimeout,
	})

	return contextID, session, agent, toolRegistry, skillCatalog, skillRuntime, skillResult, nil
}

func sandboxRegistryOptions(config *Config) ([]tools.RegistryOption, error) {
	return sandboxRegistryOptionsWithWarnings(config, newBroadWritablePathWarner())
}

func sandboxRegistryOptionsWithWarnings(config *Config, warnings *broadWritablePathWarner) ([]tools.RegistryOption, error) {
	if config.NoSandbox {
		return []tools.RegistryOption{tools.WithUnsafeNoSandbox()}, nil
	}
	if warnings == nil {
		warnings = newBroadWritablePathWarner()
	}

	baseCfg, err := sandbox.ParsePreset(config.SandboxPreset)
	if err != nil {
		return nil, err
	}
	baseCfg = baseCfg.Merge(sandbox.Config{
		WritablePaths: config.WritePaths,
		DenyPaths:     config.DenyPaths,
		AllowNetwork:  config.AllowNet,
	})
	baseCfg, err = sandbox.PrepareConfig(baseCfg)
	if err != nil {
		return nil, fmt.Errorf("prepare sandbox config: %w", err)
	}

	// The same warning-aware factory handles the startup probe and every final
	// per-tool config produced later by the registry. One shared state suppresses
	// repeats when the base grant appears in several effective configs.
	factory := newSandbox
	warningFactory := func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		sb, err := factory(cfg)
		if err == nil && sb != nil {
			warnings.Warn(cfg)
		}
		return sb, err
	}

	// Validate that the backend constructs (e.g. the binary exists)...
	sb, err := warningFactory(baseCfg)
	if err != nil {
		return nil, fmt.Errorf("sandbox requested but unavailable: %w", err)
	}
	// ...and that it can actually start a command. Construction alone misses
	// environments where the backend is present but fails at runtime; without
	// this probe every bash call would silently return a refusal while the run
	// still exits 0/ok.
	if err := sandbox.Probe(sb); err != nil {
		return nil, fmt.Errorf("sandbox requested but failed to start: %w\n"+
			"Set POLLYTOOL_NOSANDBOX=1 (or pass --nosandbox) to run without the sandbox", err)
	}

	return []tools.RegistryOption{tools.WithSandboxFactory(warningFactory, baseCfg)}, nil
}

type broadWritablePathWarner struct {
	mu      sync.Mutex
	seen    map[string]bool
	pending []string
	notify  chan struct{}
	home    string
}

func newBroadWritablePathWarner() *broadWritablePathWarner {
	home, _ := os.UserHomeDir()
	home = canonicalWarningPath(home)
	return &broadWritablePathWarner{
		seen:   make(map[string]bool),
		notify: make(chan struct{}, 1),
		home:   home,
	}
}

func (w *broadWritablePathWarner) Notify() <-chan struct{} {
	if w == nil {
		return nil
	}
	return w.notify
}

// Drain atomically takes all pending warning bodies. Consuming the coalesced
// notification under the same lock as the queue prevents a concurrent enqueue
// from losing its wakeup.
func (w *broadWritablePathWarner) Drain() []string {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	select {
	case <-w.notify:
	default:
	}
	pending := append([]string(nil), w.pending...)
	w.pending = nil
	w.mu.Unlock()
	return pending
}

// Warn reports explicit writable grants for the whole home directory or a
// filesystem root. The workspace preset rejects those roots before discovery,
// but --writepath and per-tool overlays can still add them. The credential deny
// list still applies; this is a user-visible heads-up, not a refusal.
func (w *broadWritablePathWarner) Warn(cfg sandbox.Config) {
	if w == nil || cfg.DenyWrite {
		return
	}
	for _, path := range cfg.WritablePaths {
		path = filepath.Clean(path)
		if broadWritablePathDenied(path, cfg.DenyWritePaths) {
			continue
		}
		scope := ""
		switch {
		case path != "" && filepath.IsAbs(path) && filepath.Dir(path) == path:
			scope = "a filesystem root"
		case w.home != "" && path == w.home:
			scope = "the whole home directory"
		default:
			continue
		}

		body := fmt.Sprintf("sandbox writable path %q grants write access to %s; remove or narrow the originating --writepath/POLLYTOOL_WRITEPATHS or tool writablePaths setting unless this broad access is intentional", path, scope)
		w.emit(path, body)
	}
}

func (w *broadWritablePathWarner) emit(path, body string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.seen[path] {
		return
	}
	w.seen[path] = true
	w.pending = append(w.pending, body)
	select {
	case w.notify <- struct{}{}:
	default:
	}
}

func canonicalWarningPath(path string) string {
	if path == "" {
		return ""
	}
	path = filepath.Clean(path)
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(real)
	}
	return path
}

func broadWritablePathDenied(path string, denyWritePaths []string) bool {
	if path == "" {
		return false
	}
	for _, denied := range denyWritePaths {
		denied = filepath.Clean(denied)
		if denied == "" {
			continue
		}
		rel, err := filepath.Rel(denied, path)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func runConversation(ctx context.Context, config *Config, sessionStore sessions.SessionStore, contextID string, autoContext bool, cmd *cli.Command) error {
	input, err := resolveConversationInput(config)
	if err != nil {
		return err
	}

	// Applies before initializeSession so a context's stored prompt still
	// overrides the effective default.
	if input.mode == conversationModeREPL {
		config.Settings.SystemPrompt = effectiveDefaultSystemPrompt(
			config.Settings.SystemPrompt, cmd.IsSet("system"), supportsManagedREPL())
	}

	// Initialize session state once so one-shot and REPL share the same runtime.
	sandboxWarnings := newBroadWritablePathWarner()
	_, session, agent, toolRegistry, skillCatalog, skillRuntime, skillResult, err := initializeSession(config, sessionStore, contextID, cmd, sandboxWarnings)
	if err != nil {
		return err
	}
	state := &conversationState{
		session:          session,
		agent:            agent,
		toolRegistry:     toolRegistry,
		skillCatalog:     skillCatalog,
		skillRuntime:     skillRuntime,
		skillSources:     skillResult.sources,
		sandboxWarnings:  sandboxWarnings,
		autoNamedContext: autoContext,
	}
	defer state.Close()

	// Set up signal handling
	ctx, cancel := setupSignalHandling(ctx)
	defer cancel()

	switch input.mode {
	case conversationModeOneShot:
		drainSandboxWarningsToWriter(os.Stderr, state)
		defer drainSandboxWarningsToWriter(os.Stderr, state)
		var schema *llm.Schema
		if config.SchemaPath != "" {
			schema, err = loadSchemaFile(config.SchemaPath)
			if err != nil {
				return fmt.Errorf("failed to load schema: %w", err)
			}
		}
		code, err := executeTurn(ctx, config, state, input.prompt, schema, bufio.NewReader(os.Stdin), nil)
		if err != nil {
			return &exitError{code: code, err: err}
		}
		if code != 0 {
			// Output completed; the code alone signals an incomplete outcome
			// (e.g. truncation -> 2) with no error message to print.
			return &exitError{code: code}
		}
		return nil
	case conversationModeREPL:
		replErr := runREPL(ctx, config, state)
		if autoContext {
			discardUnusedAutoContext(state, sessionStore, contextID)
		}
		return replErr
	default:
		return fmt.Errorf("unknown conversation mode")
	}
}

// discardUnusedAutoContext deletes a generated context that never saw a turn,
// so launch-and-quit REPL runs leave no file behind. The session's own lock
// must be released first — Delete skips locked sessions. A context renamed via
// /rename is untouched: its file no longer lives under the generated name.
func discardUnusedAutoContext(state *conversationState, store sessions.SessionStore, contextID string) {
	if state.session == nil {
		return
	}
	for _, msg := range state.session.GetHistory() {
		if msg.Role != messages.MessageRoleSystem {
			return
		}
	}
	state.session.Close()
	store.Delete(contextID)
}

func selectConversationMode(config *Config, stdinAvailable bool) (conversationMode, error) {
	if config.PromptSet || stdinAvailable {
		return conversationModeOneShot, nil
	}

	if err := validateREPLConfig(config); err != nil {
		return conversationModeOneShot, err
	}

	return conversationModeREPL, nil
}

func resolveConversationInput(config *Config) (conversationInput, error) {
	stdinAvailable := hasStdinData()
	mode, err := selectConversationMode(config, stdinAvailable)
	if err != nil {
		return conversationInput{}, err
	}

	switch mode {
	case conversationModeOneShot:
		if config.PromptSet {
			return conversationInput{mode: conversationModeOneShot, prompt: config.Prompt}, nil
		}
		prompt, err := readFromStdin()
		if err != nil {
			return conversationInput{}, err
		}
		return conversationInput{mode: conversationModeOneShot, prompt: prompt}, nil
	case conversationModeREPL:
		return conversationInput{mode: conversationModeREPL}, nil
	default:
		return conversationInput{}, fmt.Errorf("unknown conversation mode")
	}
}

func validateREPLConfig(config *Config) error {
	var rejected []string
	if len(config.Files) > 0 {
		rejected = append(rejected, "--file")
	}
	if config.SchemaPath != "" {
		rejected = append(rejected, "--schema")
	}
	// The trailer writes raw lines to stderr, which would garble the managed
	// REPL's tcell screen and interleave with the fallback REPL's prompt.
	if config.Meta {
		rejected = append(rejected, "--meta")
	}
	if len(rejected) == 0 {
		return nil
	}

	verb := "require"
	if len(rejected) == 1 {
		verb = "requires"
	}

	return fmt.Errorf("%s %s -p or stdin; bare polly starts a text-only REPL", strings.Join(rejected, " and "), verb)
}

// effectiveDefaultSystemPrompt swaps the pipe-oriented default system prompt
// (which forbids markdown) for the markdown-welcoming one when the managed
// REPL — which renders markdown — is about to run. Only the untouched default
// is swapped: an explicit -s/POLLYTOOL_SYSTEM value passes through, and the
// fallback line REPL keeps the plain-output default.
func effectiveDefaultSystemPrompt(current string, isSet, managedREPL bool) string {
	if managedREPL && !isSet && current == defaultSystemPrompt {
		return defaultREPLSystemPrompt
	}
	return current
}

func runREPL(ctx context.Context, config *Config, state *conversationState) error {
	if supportsManagedREPL() {
		return runManagedREPL(ctx, config, state)
	}
	return runFallbackREPL(ctx, config, state)
}

// executeTurn runs one turn and returns the process exit code the turn's
// outcome maps to (0 end_turn, 2 max_tokens, 3 max_iterations, 1 hard error)
// alongside any error. Only the one-shot path acts on the code; the REPLs
// ignore it and consume just the error.
func executeTurn(ctx context.Context, config *Config, state *conversationState, prompt string, schema *llm.Schema, inputReader *bufio.Reader, turnUI TurnUI) (int, error) {
	return executeTurnWithExistingUser(ctx, config, state, prompt, schema, inputReader, turnUI, false)
}

// executeTurnWithExistingUser runs a turn and, when reuseUser is true, avoids
// persisting the same user message twice. This is used when retrying a canceled
// turn whose user message was already durably stored. Reuse is deliberately
// conservative: only an equivalent user message at the very end of history is
// reused, so a missing, changed, or non-terminal message is persisted normally.
func executeTurnWithExistingUser(ctx context.Context, config *Config, state *conversationState, prompt string, schema *llm.Schema, inputReader *bufio.Reader, turnUI TurnUI, reuseUser bool) (int, error) {
	userMsg, err := buildMessageWithFiles(prompt, config.Files)
	if err != nil {
		return 1, fmt.Errorf("error processing files: %w", err)
	}

	// Persist the user message before spending API tokens. If the session store
	// is broken (e.g. disk full), fail fast rather than make a call whose result
	// can't be saved either. In-memory sessions never error here.
	if err := persistUserMessageForTurn(state.session, userMsg, reuseUser); err != nil {
		return 1, fmt.Errorf("failed to persist user message: %w", err)
	}

	if turnUI == nil {
		turnUI = newLineTurnUI(config, inputReader)
	}
	turnUI.Start()
	defer turnUI.Stop()

	req := createCompletionRequest(config, state.session, state.toolRegistry, state.skillCatalog, schema)

	// trimLeadingNL strips leading newlines from the next content burst.
	// Armed only after a reasoning event fires — models with thinking enabled
	// commonly emit a leading "\n\n" to visually separate the (hidden)
	// reasoning from the reply. We strip only \n/\r so leading spaces/tabs
	// (e.g. code-block indentation) are preserved.
	trimLeadingNL := false

	stats := &turnToolStats{}
	turnStart := time.Now()

	resp, err := state.agent.Run(ctx, req, &llm.AgentCallbacks{
		OnReasoning: func(content string) {
			trimLeadingNL = true
			turnUI.ShowThinking(content)
		},
		OnContent: func(content string) {
			if config.SchemaPath != "" {
				return
			}
			if trimLeadingNL {
				content = trimLeadingResponseNewlines(content)
				if content == "" {
					return
				}
				trimLeadingNL = false
			}
			turnUI.AppendAssistantText(content)
		},
		OnToolStart: func(calls []messages.ChatMessageToolCall) {
			turnUI.AppendToolStart(calls)
		},
		ApproveToolCalls: turnUI.ApproveToolCalls,
		OnToolEnd: func(tc messages.ChatMessageToolCall, result string, duration time.Duration, err error) {
			stats.record(tc.Name, err)
			turnUI.AppendToolEnd(tc, result, duration, err)
		},
		OnError: func(err error) {},
	})
	if ctx.Err() != nil {
		return 1, ctx.Err()
	}
	var in, out int
	if resp != nil {
		// Multi-iteration turns produce multiple assistant messages (one per
		// LLM call between tool roundtrips). Providers report input tokens
		// cumulatively per-call (each call resends history), so take max for
		// input. Output tokens are per-iteration, so sum them.
		for _, m := range resp.AllMessages {
			if m.Role != messages.MessageRoleAssistant {
				continue
			}
			if t := m.GetInputTokens(); t > in {
				in = t
			}
			out += m.GetOutputTokens()
		}
		turnUI.RecordTurnTokens(in, out)
	}

	// finishTurn covers everything downstream of a completed agent run:
	// persistence, the truncation warning, and final output. Folding its error
	// into runErr means the trailer and exit code below always describe the
	// turn's final state, whichever stage failed.
	// A failed/canceled turn keeps any streamed text visibly labeled as
	// not saved. Keep persistence aligned with that contract: generated messages
	// and skill metadata are committed only after the agent has completed.
	runErr := err
	if runErr == nil {
		runErr = func() error {
			if resp == nil {
				return fmt.Errorf("agent returned no response")
			}
			if err := persistActiveSkills(state.session, state.skillRuntime, state.skillSources); err != nil {
				return fmt.Errorf("failed to persist active skills: %w", err)
			}

			// Persist the whole turn (assistant message per iteration + every tool
			// result) with a single write instead of one rewrite per message.
			if perr := state.session.AddMessages(durableTurnMessages(resp.AllMessages)); perr != nil {
				return fmt.Errorf("failed to persist turn: %w", perr)
			}

			if resp.Message != nil && resp.Message.StopReason == messages.StopReasonMaxTokens {
				turnUI.AppendWarning(fmt.Sprintf("response truncated (hit %d token limit, use --maxtokens to increase)", config.MaxTokens))
			}

			if config.SchemaPath != "" {
				var content string
				if resp.Message != nil {
					content = resp.Message.Content
				}
				return outputStructured(content, schema)
			}
			turnUI.FinishTextTurn()
			return nil
		}()
	}

	stopReason, code := classifyOutcome(resp, runErr)
	if config.Meta {
		writeMetaTrailer(os.Stderr, buildMeta(stopReason, resp, runErr, config.Model, stats, in, out, time.Since(turnStart).Milliseconds()))
	}
	return code, runErr
}

// durableTurnMessages removes provider-protocol denial exchanges while still
// recording that an all-denied turn completed. The internal marker is never
// sent to a model; hydration folds it into one compact denied row instead of
// resurrecting the completed prompt as an incomplete /retry candidate.
func durableTurnMessages(generated []messages.ChatMessage) []messages.ChatMessage {
	stripped := llm.StripDeniedExchanges(generated)
	if terminalToolBatchAllDenied(generated) {
		stripped = append(stripped, messages.ChatMessage{
			Role: messages.MessageRoleInternal,
			Metadata: map[string]any{
				messages.MetadataKeyTurnStatus: messages.TurnStatusToolDenied,
			},
		})
	}
	return stripped
}

func terminalToolBatchAllDenied(generated []messages.ChatMessage) bool {
	proposal := -1
	for i, msg := range generated {
		if msg.Role == messages.MessageRoleAssistant && len(msg.ToolCalls) > 0 {
			proposal = i
		}
	}
	if proposal < 0 {
		return false
	}
	seen := false
	for _, msg := range generated[proposal+1:] {
		if msg.Role != messages.MessageRoleTool {
			continue
		}
		seen = true
		if msg.Content != llm.ToolDeniedContent {
			return false
		}
	}
	return seen
}

func persistUserMessageForTurn(session sessions.Session, userMsg messages.ChatMessage, reuseUser bool) error {
	if reuseUser && sessionEndsWithEquivalentUserMessage(session, userMsg) {
		return nil
	}
	return session.AddMessage(userMsg)
}

func sessionEndsWithEquivalentUserMessage(session sessions.Session, userMsg messages.ChatMessage) bool {
	history := session.GetHistory()
	if len(history) == 0 {
		return false
	}
	last := history[len(history)-1]
	return last.Role == messages.MessageRoleUser &&
		userMsg.Role == messages.MessageRoleUser &&
		last.Content == userMsg.Content &&
		slices.Equal(last.Parts, userMsg.Parts)
}

func modelVisibleHistory(history []messages.ChatMessage) []messages.ChatMessage {
	visible := make([]messages.ChatMessage, 0, len(history))
	for _, msg := range history {
		if msg.Role != messages.MessageRoleInternal {
			visible = append(visible, msg)
		}
	}
	return visible
}

// createCompletionRequest builds an LLM completion request from config
func createCompletionRequest(config *Config, session sessions.Session, registry *tools.ToolRegistry, skillCatalog *skills.Catalog, schema *llm.Schema) *llm.CompletionRequest {
	// Parse thinking effort - already validated at config parsing time
	thinkingEffort, _ := llm.ParseThinkingEffort(config.ThinkingEffort)

	return &llm.CompletionRequest{
		BaseURL:        config.BaseURL,
		Timeout:        config.Timeout,
		Temperature:    llm.Float32Ptr(float32(config.Temperature)),
		Model:          config.Model,
		MaxTokens:      config.MaxTokens,
		Messages:       modelVisibleHistory(session.GetHistory()),
		Skills:         skillCatalog,
		Tools:          registry.All(),
		ResponseSchema: schema,
		ThinkingEffort: thinkingEffort,
	}
}

// initializeConversation handles all the setup needed before starting a conversation
func initializeConversation(config *Config, sessionStore sessions.SessionStore, contextID string, cmd *cli.Command) (string, *sessions.Metadata, error) {
	var needReset bool
	var originalContextInfo *sessions.Metadata

	// Load context settings if available
	if contextID != "" {
		if contextInfo := sessionStore.GetAllMetadata()[contextID]; contextInfo != nil {
			originalContextInfo = contextInfo

			// Check if system prompt is being changed (only if context has existing conversation)
			if cmd.IsSet("system") && cmd.String("system") != contextInfo.SystemPrompt {
				// Check if there's an existing conversation to reset
				if sessionStore.Exists(contextInfo.Name) {
					needReset = true
					fmt.Fprintf(os.Stderr, "System prompt changed, resetting conversation...\n")
				}
			}

			// Use stored settings if not overridden by command line
			if !cmd.IsSet("model") && contextInfo.Model != "" {
				config.Settings.Model = contextInfo.Model
			}
			if !cmd.IsSet("temp") && contextInfo.Temperature != 0 {
				config.Settings.Temperature = contextInfo.Temperature
			}
			if !cmd.IsSet("maxtokens") && contextInfo.MaxTokens != 0 {
				config.Settings.MaxTokens = contextInfo.MaxTokens
			}
			// Stored zero means unlimited and must sync too, or /get would
			// report the flag default while trimming is actually off.
			if !cmd.IsSet("maxcontext") {
				config.Settings.MaxHistoryTokens = contextInfo.MaxHistoryTokens
			}
			// Only use stored system prompt if flag wasn't explicitly set
			if !cmd.IsSet("system") && contextInfo.SystemPrompt != "" {
				config.Settings.SystemPrompt = contextInfo.SystemPrompt
			}
			// Tools are now handled directly with session metadata in initializeSession
			// Apply stored thinking effort if not provided via command line
			if !cmd.IsSet("thinking") && contextInfo.ThinkingEffort != "off" && contextInfo.ThinkingEffort != "" {
				config.Settings.ThinkingEffort = contextInfo.ThinkingEffort
			}
			// Apply stored tool timeout if not provided via command line
			if !cmd.IsSet("tooltimeout") && contextInfo.ToolTimeout > 0 {
				config.Settings.ToolTimeout = contextInfo.ToolTimeout
			}
			if !cmd.IsSet("maxiterations") && contextInfo.MaxIterations > 0 {
				config.MaxIterations = contextInfo.MaxIterations
			}
			if !cmd.IsSet("skilldir") && len(contextInfo.SkillDirs) > 0 {
				config.Settings.SkillDirs = contextInfo.SkillDirs
			}
		}
	}

	// Perform reset if system prompt changed
	if needReset && originalContextInfo != nil {
		// Get the context name
		contextName := contextID
		if originalContextInfo.Name != "" {
			contextName = originalContextInfo.Name
		}

		// Reset the context
		if err := resetContext(sessionStore, contextName); err != nil {
			return "", nil, fmt.Errorf("failed to reset context: %w", err)
		}
		// Context name remains the same after reset
		contextID = contextName
	}

	return contextID, originalContextInfo, nil
}

// applyFlagSettings copies only explicitly-set CLI flags onto md. Explicit
// zero values (e.g. --maxcontext 0 = unlimited) are preserved, which the
// mergo-based UpdateMetadata merge cannot do.
func applyFlagSettings(md *sessions.Metadata, config *Config, cmd *cli.Command) {
	if cmd.IsSet("model") {
		md.Model = config.Settings.Model
	}
	if cmd.IsSet("temp") {
		md.Temperature = config.Settings.Temperature
	}
	if cmd.IsSet("maxtokens") {
		md.MaxTokens = config.Settings.MaxTokens
	}
	if cmd.IsSet("maxcontext") {
		md.MaxHistoryTokens = config.Settings.MaxHistoryTokens
	}
	if cmd.IsSet("maxiterations") {
		md.MaxIterations = config.MaxIterations
	}
	if cmd.IsSet("tooltimeout") {
		md.ToolTimeout = config.Settings.ToolTimeout
	}
	if cmd.IsSet("system") {
		md.SystemPrompt = config.Settings.SystemPrompt
	}
	if cmd.IsSet("thinking") {
		md.ThinkingEffort = config.Settings.ThinkingEffort
	}
	if cmd.IsSet("skilldir") {
		md.SkillDirs = config.Settings.SkillDirs
	}
}

// updateContextInfo persists current settings onto the session metadata via
// read-modify-write, so explicit zero values survive (UpdateMetadata's merge
// drops them) and persistence errors surface.
func updateContextInfo(session sessions.Session, config *Config, cmd *cli.Command) error {
	md := session.GetMetadata()
	if md == nil {
		md = &sessions.Metadata{}
	}
	md.Name = session.GetName()
	md.LastUsed = time.Now()
	// Config holds resolved values — stored settings unless flags override
	// (see initializeConversation) — so these are safe to write back.
	md.Model = config.Settings.Model
	md.Temperature = config.Settings.Temperature
	md.MaxTokens = config.Settings.MaxTokens
	md.MaxIterations = config.MaxIterations
	md.ToolTimeout = config.Settings.ToolTimeout
	md.SkillDirs = config.Settings.SkillDirs
	// Tools are already handled in initializeSession; settings that have no
	// resolved-config equivalent apply only when explicitly set.
	applyFlagSettings(md, config, cmd)
	return session.SetMetadata(md)
}

// beforeExit is invoked synchronously by cleanupAndExit before os.Exit.
// The managed REPL registers gotui's ui.Close here so a signal-triggered
// exit (SIGTERM, or a SIGINT that tcell's raw mode didn't capture as a key
// event) still restores the terminal — os.Exit skips the deferred ui.Close
// in managedREPL.Run. The mutex serializes registration from the REPL
// goroutine with the read from the signal goroutine in setupSignalHandling.
var (
	beforeExitMu sync.Mutex
	beforeExit   func()
)

func setBeforeExit(fn func()) {
	beforeExitMu.Lock()
	beforeExit = fn
	beforeExitMu.Unlock()
}

// cleanupAndExit performs cleanup and exits with the given code
func cleanupAndExit(code int) {
	beforeExitMu.Lock()
	fn := beforeExit
	beforeExitMu.Unlock()
	if fn != nil {
		fn()
	}
	os.Exit(code)
}

// readFromStdin reads all lines from stdin and joins them with newlines
func readFromStdin() (string, error) {
	scanner := bufio.NewScanner(os.Stdin)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("error reading stdin: %w", err)
	}
	return strings.Join(lines, "\n"), nil
}

// hasStdinData checks if stdin has data available
func hasStdinData() bool {
	stat, _ := os.Stdin.Stat()
	return (stat.Mode() & os.ModeCharDevice) == 0
}

// setupSignalHandling sets up signal handling for graceful shutdown
func setupSignalHandling(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		// Cleanup before canceling context
		cleanupAndExit(130) // 128 + SIGINT(2) = 130
	}()
	return ctx, cancel
}

// outputStructured formats and outputs structured response
func outputStructured(content string, schema *llm.Schema) error {
	// Empty content means no structured output was produced — e.g. the model
	// emitted a tool call that was denied and the turn short-circuited. Report
	// it instead of printing a silent blank line that looks like success.
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("no structured output produced")
	}
	// If content is already JSON, pretty-print it
	var data any
	if err := json.Unmarshal([]byte(content), &data); err == nil {
		// Validate against schema if provided
		if schema != nil {
			if err := validateJSONAgainstSchema(data, schema); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: Output doesn't match schema: %v\n", err)
			}
		}

		jsonBytes, _ := json.MarshalIndent(data, "", "  ")
		fmt.Println(string(jsonBytes))
	} else {
		// Fallback to raw output if not valid JSON
		fmt.Println(content)
	}
	return nil
}

// stripProviderPrefix returns the bare model name, dropping "provider/" if present.
func stripProviderPrefix(m string) string {
	if i := strings.IndexByte(m, '/'); i >= 0 {
		return m[i+1:]
	}
	return m
}

func toolCount(r *tools.ToolRegistry) int {
	if r == nil {
		return 0
	}
	return len(r.All())
}

func skillCount(c *skills.Catalog) int {
	if c == nil {
		return 0
	}
	return len(c.List())
}
