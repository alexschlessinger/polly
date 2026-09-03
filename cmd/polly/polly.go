package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
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
		// Signal cancellation travels through the ordinary command return path so
		// session, store, and terminal defers all run before the process exits.
		// Do not render that expected shutdown as a generic command error.
		if code, remaining, ok := splitSignalError(err); ok {
			if remaining != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", remaining)
			}
			cleanupAndExit(code)
		}
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
	llmClient    *llm.MultiPass
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
	sessionStore    sessions.SessionStore
	session         sessions.Session
	agent           *llm.Agent
	artifactStore   artifacts.Store
	toolRegistry    *tools.ToolRegistry
	skillCatalog    *skills.Catalog
	skillRuntime    *tools.SkillRuntime
	skillSources    []string
	sandboxWarnings *broadWritablePathWarner
	// displayContract is composed into the request's system message each turn;
	// it is capability-specific and never persisted (see display_contract.go).
	displayContract string
	// outputCapabilities is resolved once per process run so the model-facing
	// contract and the concrete line renderer cannot disagree.
	outputCapabilities outputCapabilities
	// contextWindows caches per-model context-window discovery for this
	// process, including failed attempts (entry present, value 0).
	contextWindows map[string]int
}

type resumeSessionRequest struct{ name string }

func (r *resumeSessionRequest) Error() string { return "resume session " + r.name }

func (s *conversationState) Close() error {
	var errs []error
	if s.toolRegistry != nil {
		if err := s.toolRegistry.Close(); err != nil {
			errs = append(errs, err)
		}
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

func closeSessionAfterError(session sessions.Session, cause error) error {
	if session == nil {
		return cause
	}
	if err := session.Close(); err != nil {
		return errors.Join(cause, fmt.Errorf("close session: %w", err))
	}
	return cause
}

func closeStoreAfterError(store sessions.SessionStore, cause error) error {
	if store == nil {
		return cause
	}
	if err := store.Close(); err != nil {
		return errors.Join(cause, fmt.Errorf("close context store: %w", err))
	}
	return cause
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

	// An interactive REPL with no context gets a generated, disk-backed one so
	// the conversation survives exit (resume with -L or -c <name>). Contexts
	// that never see a turn are discarded on exit.
	autoContext := contextID == "" && wantsAutoREPLContext(config)

	sessionStore, err := setupSessionStore(config, contextID, autoContext)
	if err != nil {
		return nil, fmt.Errorf("failed to create context store: %w", err)
	}

	if config.UseLastContext {
		contextID, err = sessionStore.GetLast(ctx)
		if err != nil {
			return nil, closeStoreAfterError(sessionStore, fmt.Errorf("failed to find last context: %w", err))
		}
		if contextID == "" {
			return nil, closeStoreAfterError(sessionStore, fmt.Errorf("no last context found"))
		}
	}
	if autoContext {
		var existsErr error
		contextID = generateContextName(func(name string) bool {
			exists, err := sessionStore.Exists(ctx, name)
			if err != nil {
				existsErr = err
				return true
			}
			return exists
		})
		if existsErr != nil {
			return nil, closeStoreAfterError(sessionStore, fmt.Errorf("failed to generate context name: %w", existsErr))
		}
	}

	return &commandRunner{
		ctx:          ctx,
		cmd:          cmd,
		config:       config,
		llmClient:    llm.NewMultiPass(loadAPIKeys()),
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

func (r *commandRunner) Run() (retErr error) {
	defer func() {
		if err := r.sessionStore.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close context store: %w", err))
		}
	}()
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
		contextID, err := checkAndPromptForMissingContext(r.ctx, r.sessionStore, r.contextID)
		if err != nil {
			return err
		}
		if contextID == "" {
			return nil
		}
		r.contextID = contextID
	}

	showStartupLogo := true
	for {
		err := r.runConversation(showStartupLogo)
		var resume *resumeSessionRequest
		if !errors.As(err, &resume) {
			return err
		}
		r.contextID = resume.name
		r.autoContext = false
		showStartupLogo = false
	}
}

func (r *commandRunner) handleManagementFlags() (bool, error) {
	cfg := r.config
	store := r.sessionStore

	if cfg.ResetContext != "" {
		return true, handleResetContext(r.ctx, store, cfg, r.cmd, cfg.ResetContext)
	}
	if cfg.ListContexts {
		return true, handleListContexts(r.ctx, store)
	}
	if cfg.ListSkills {
		return true, handleListSkills(cfg)
	}
	if cfg.DeleteContext != "" {
		return true, handleDeleteContext(r.ctx, store, cfg.DeleteContext)
	}
	if cfg.AddToContext {
		return true, handleAddToContext(r.ctx, store, cfg, r.contextID)
	}
	if cfg.PurgeAll {
		return true, handlePurgeAll(r.ctx, store)
	}
	if cfg.CreateContext != "" {
		return true, handleCreateContext(r.ctx, store, cfg, cfg.CreateContext)
	}
	if cfg.ShowContext != "" {
		return true, handleShowContext(r.ctx, store, cfg.ShowContext)
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

// newConversationState sets up everything a conversation needs: the session,
// its tool registry and skill runtime, and the agent. A nil llmClient
// constructs one from config. Session metadata is staged on one object and
// written once, so a failure part-way through persists nothing. On error
// every acquired resource is released and nil is returned.
func newConversationState(ctx context.Context, config *Config, llmClient *llm.MultiPass, sessionStore sessions.SessionStore, contextID string, autoContext bool, cmd *cli.Command, sandboxWarnings *broadWritablePathWarner) (state *conversationState, retErr error) {
	contextID, _, err := initializeConversation(ctx, config, sessionStore, contextID, cmd)
	if err != nil {
		return nil, err
	}
	if llmClient == nil {
		llmClient = llm.NewMultiPass(loadAPIKeys())
	}

	// Get or create the session early so persisted skill sources can be read.
	session, err := getOrCreateSession(ctx, sessionStore, contextID, needsFileStore(config, contextID), autoContext)
	if err != nil {
		return nil, err
	}
	var toolRegistry *tools.ToolRegistry
	defer func() {
		if retErr == nil {
			return
		}
		if toolRegistry != nil {
			_ = toolRegistry.Close()
		}
		retErr = closeSessionAfterError(session, retErr)
	}()
	metadata, err := session.GetMetadata(ctx)
	if err != nil {
		return nil, fmt.Errorf("read context metadata: %w", err)
	}

	// Discover skills before building the runtime tool registry, passing the
	// persisted sources so --skill is restored on resume; new sources are
	// staged for the single write below.
	skillResult, err := loadSkillCatalog(config, metadata.SkillSources)
	if err != nil {
		return nil, err
	}
	if len(skillResult.sources) > 0 {
		metadata.SkillSources = skillResult.sources
	}

	registryOpts, err := sandboxRegistryOptionsWithWarnings(config, sandboxWarnings)
	if err != nil {
		return nil, err
	}
	if len(config.Tools) > 0 {
		// Command-line tools replace the session's persisted tools.
		toolRegistry = tools.NewToolRegistry(nil, registryOpts...)
		for _, source := range config.Tools {
			if _, err := toolRegistry.LoadToolAuto(source); err != nil {
				return nil, fmt.Errorf("failed to load tool %s: %w", source, err)
			}
		}
		metadata.ActiveTools = toolRegistry.GetActiveToolLoaders()
	} else {
		toolRegistry, err = loadTools(metadata.ActiveTools, registryOpts...)
		if err != nil {
			return nil, err
		}
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
	if err := updateContextInfo(ctx, session, metadata, config, cmd); err != nil {
		return nil, err
	}

	artifactStore := session.ArtifactStore()
	agent := llm.NewAgent(llmClient, toolRegistry, llm.AgentConfig{
		MaxIterations: config.MaxIterations,
		ToolTimeout:   config.ToolTimeout,
		ArtifactStore: artifactStore,
	})
	return &conversationState{
		sessionStore:    sessionStore,
		session:         session,
		agent:           agent,
		artifactStore:   artifactStore,
		toolRegistry:    toolRegistry,
		skillCatalog:    skillResult.catalog,
		skillRuntime:    skillRuntime,
		skillSources:    skillResult.sources,
		sandboxWarnings: sandboxWarnings,
	}, nil
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

func (r *commandRunner) runConversation(showStartupLogo bool) (retErr error) {
	ctx, config := r.ctx, r.config
	input, err := resolveConversationInput(config)
	if err != nil {
		return err
	}

	// The frontend is fixed for the life of the run; resolve it once so the
	// display contract and the REPL flavor cannot disagree.
	managedREPL := supportsManagedREPL()
	outputCapabilities := outputCapabilitiesForRun(input.mode, managedREPL)

	// Initialize session state once so one-shot and REPL share the same runtime.
	state, err := newConversationState(ctx, config, r.llmClient, r.sessionStore, r.contextID, r.autoContext, r.cmd, newBroadWritablePathWarner())
	if err != nil {
		return err
	}
	state.displayContract = displayContractFor(outputCapabilities)
	state.outputCapabilities = outputCapabilities
	session := state.session
	defer func() {
		if err := state.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close conversation state: %w", err))
		}
	}()

	// Set up signal handling
	signalCtx, cancelSignal := setupSignalHandling(ctx)
	defer cancelSignal()
	// Make the lease context the direct parent so lease loss is observable
	// synchronously by the agent and TUI. Signal/caller cancellation is bridged
	// into the same typed-cause context for the other shutdown path.
	ctx, cancelRun := context.WithCancelCause(session.Context())
	stopSignalCancel := context.AfterFunc(signalCtx, func() {
		cancelRun(context.Cause(signalCtx))
	})
	defer func() {
		stopSignalCancel()
		cancelRun(nil)
	}()

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
		replErr := runREPL(ctx, config, state, managedREPL, showStartupLogo)
		if r.autoContext {
			if err := discardUnusedAutoContext(ctx, state); replErr == nil && err != nil {
				replErr = err
			}
		}
		return replErr
	default:
		return fmt.Errorf("unknown conversation mode")
	}
}

func cacheSessionIDForSession(ctx context.Context, session sessions.Session) (string, error) {
	id, err := session.CacheSessionID(ctx)
	if err != nil {
		return "", fmt.Errorf("read session cache identity: %w", err)
	}
	return id, nil
}

// discardUnusedAutoContext closes a generated context that never saw a turn,
// so launch-and-quit REPL runs leave no durable session behind. SQLite session
// close owns this retention transition: it atomically removes an unused auto
// session while preserving one promoted to named via /rename. In particular,
// there must be no follow-up store operation using the now-cancelled session
// context.
func discardUnusedAutoContext(ctx context.Context, state *conversationState) error {
	if state.session == nil {
		return nil
	}
	history, err := state.session.GetHistory(ctx)
	if err != nil {
		return fmt.Errorf("read generated context history: %w", err)
	}
	for _, msg := range history {
		if msg.Role != messages.MessageRoleSystem {
			return nil
		}
	}
	if err := state.session.Close(); err != nil {
		return fmt.Errorf("close generated context: %w", err)
	}
	return nil
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

func runREPL(ctx context.Context, config *Config, state *conversationState, managedREPL, showStartupLogo bool) error {
	if managedREPL {
		return runManagedREPL(ctx, config, state, showStartupLogo)
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
// persisting the same user message twice. This is used when resubmitting an
// unchanged restored draft whose user message was already durably stored. Reuse is deliberately
// conservative: only an equivalent user message at the very end of history is
// reused, so a missing, changed, or non-terminal message is persisted normally.
func executeTurnWithExistingUser(ctx context.Context, config *Config, state *conversationState, prompt string, schema *llm.Schema, inputReader *bufio.Reader, turnUI TurnUI, reuseUser bool) (int, error) {
	userMsg, err := buildMessageWithFiles(prompt, config.Files)
	if err != nil {
		return 1, fmt.Errorf("error processing files: %w", err)
	}
	return executeTurnWithUserMessage(ctx, config, state, userMsg, schema, inputReader, turnUI, reuseUser)
}

// executeTurnWithUserMessage is the shared turn body behind a caller-built
// user message. The one-shot and fallback paths build theirs from --file;
// the managed REPL builds a multimodal message from composer attachments.
func executeTurnWithUserMessage(ctx context.Context, config *Config, state *conversationState, userMsg messages.ChatMessage, schema *llm.Schema, inputReader *bufio.Reader, turnUI TurnUI, reuseUser bool) (int, error) {
	// An unchanged restored draft must reuse the representation already persisted. If a
	// prior storage failure left prepared bytes inline and the store later
	// recovers, rewriting only the restored candidate to an artifact would make it
	// look like a different user turn and persist a duplicate.
	history, err := state.session.GetHistory(ctx)
	if err != nil {
		return 1, fmt.Errorf("read session history: %w", err)
	}
	reusingPersistedUser := reuseUser && historyEndsWithEquivalentUserMessage(history, userMsg)
	if !reusingPersistedUser {
		userMsg, err = externalizeMessageImages(ctx, userMsg, state.artifactStore)
		if err != nil {
			return 1, fmt.Errorf("persist input artifacts: %w", err)
		}
	}
	requestMessages, err := prepareSessionImageRequest(ctx, state.session, userMsg, reuseUser)
	if err != nil {
		return 1, err
	}
	// Structured output is machine-facing: display guidance is irrelevant
	// there, "plain text only" could fight the schema on providers whose
	// structured output is prompt-based, and the context-mechanics guidance
	// (put findings in replies) is moot when the reply is a schema payload.
	if schema == nil {
		requestMessages = applyDisplayContract(requestMessages, sendTimeContracts(state.displayContract))
	}

	// Reject turns projection would deterministically fail before the user
	// message is durably persisted: a persisted-then-unsendable message would
	// make the same failure permanent for exact retries. This covers image
	// references that resolve to nothing (or ambiguously) and a single prompt
	// that alone exceeds the context budget.
	if err := llm.ValidateImageProjection(requestMessages); err != nil {
		return 1, err
	}
	if !reusingPersistedUser && config.MaxHistoryTokens > 0 {
		if tokens := llm.EstimateMessageTokens(userMsg); tokens > config.MaxHistoryTokens {
			return 1, fmt.Errorf("prompt needs about %d tokens, exceeding the %d-token context budget; it was not added to the conversation", tokens, config.MaxHistoryTokens)
		}
	}

	// Persist the user message before spending API tokens. If the session store
	// is broken (e.g. disk full), fail fast rather than make a call whose result
	// can't be saved either. Both SQLite modes surface write and lease failures.
	if turnUI == nil {
		turnUI = newLineTurnUIWithCapabilities(config, inputReader, state.outputCapabilities)
	}
	turnUI.UserMessagePersistenceStarted()
	persistErr := persistUserMessageForTurn(ctx, state.session, userMsg, reuseUser)
	turnUI.UserMessagePersistenceFinished(persistErr == nil)
	if persistErr != nil {
		return 1, fmt.Errorf("failed to persist user message: %w", persistErr)
	}

	turnUI.Start()
	defer turnUI.Stop()

	req := createCompletionRequest(config, requestMessages, state.toolRegistry, state.skillCatalog, schema)
	req.MaxContextTokens = resolveContextBudget(ctx, config, state)
	req.CacheSessionID, err = cacheSessionIDForSession(ctx, state.session)
	if err != nil {
		return 1, err
	}

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
		OnToolResult: func(tc messages.ChatMessageToolCall, result messages.ChatMessage) {
			if images := inspectionTranscriptImages(result, state.artifactStore); len(images) > 0 {
				turnUI.AppendToolMedia(tc, images)
			}
		},
		OnError: func(err error) {},
	})
	if ctx.Err() != nil {
		// Cancellation outranks whatever error the aborted run surfaced, but
		// the turn still flows through persistence below: tools that completed
		// changed the world whether or not the user hit cancel.
		err = context.Cause(ctx)
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
		used, estimated := in, false
		if used <= 0 {
			used = resp.Projection.RequestEstimatedTokens
			estimated = true
		}
		turnUI.RecordContextUsage(used, req.MaxContextTokens, estimated)
	}

	runErr := err

	// Persist everything the run generated, completed or not. Executed tool
	// calls already changed the world; dropping their record would leave the
	// session blind to work that actually happened and make a retry redo it.
	// Run guarantees AllMessages ends at a provider-valid boundary (an aborted
	// tool batch is completed with interrupted stubs), so a partial turn
	// replays cleanly. The detached context keeps a canceled turn's save from
	// being canceled along with it; the persistence gate stops a detached turn
	// from appending after newer turns already have.
	if resp != nil && len(resp.AllMessages) > 0 && turnUI.TurnPersistenceAllowed() {
		persistCtx := context.WithoutCancel(ctx)
		perr := func() error {
			if err := persistActiveSkills(persistCtx, state.session, state.skillRuntime, state.skillSources); err != nil {
				return fmt.Errorf("failed to persist active skills: %w", err)
			}
			// Persist the whole turn (assistant message per iteration + every
			// tool result) with a single write instead of one rewrite per
			// message. A failed turn additionally records why it ended, so
			// hydration can settle it instead of rendering an abandoned turn.
			durable := durableTurnMessages(resp.AllMessages)
			if runErr != nil {
				durable = append(durable, interruptedTurnMarker(runErr))
			}
			if perr := state.session.AddMessages(persistCtx, durable); perr != nil {
				return fmt.Errorf("failed to persist turn: %w", perr)
			}
			return nil
		}()
		switch {
		case perr != nil:
			runErr = errors.Join(runErr, perr)
		case runErr != nil:
			// Both facts matter downstream: the turn failed, and the work it
			// completed is durable. UIs label the outcome accordingly.
			runErr = &turnProgressSavedError{cause: runErr}
		}
	}

	// The success tail covers everything downstream of a completed agent run:
	// warnings and final output. Folding its error into runErr means the
	// trailer and exit code below always describe the turn's final state,
	// whichever stage failed.
	if runErr == nil {
		runErr = func() error {
			if resp == nil {
				return fmt.Errorf("agent returned no response")
			}

			if resp.Projection.OmittedExchanges > 0 {
				word := "exchanges"
				if resp.Projection.OmittedExchanges == 1 {
					word = "exchange"
				}
				turnUI.AppendWarning(fmt.Sprintf("model context omitted %d earlier %s; full transcript retained", resp.Projection.OmittedExchanges, word))
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

// externalizeMessageImages replaces prepared base64 image parts with private
// content-addressed references. Artifact storage is authoritative, so a write
// failure is returned instead of silently persisting a second inline format.
func externalizeMessageImages(ctx context.Context, msg messages.ChatMessage, store artifacts.Store) (messages.ChatMessage, error) {
	msg = cloneChatMessage(msg)
	for i, part := range msg.Parts {
		if part.Type != "image_base64" || part.ImageData == "" {
			continue
		}
		if store == nil {
			return messages.ChatMessage{}, fmt.Errorf("artifact store is unavailable")
		}
		// A nonportable part (legacy GIF/BMP bytes, mismatched MIME) must be
		// normalized before its bytes become an immutable artifact: once
		// externalized, the base64-only portability validation never sees it
		// again and hydration would replay the bad MIME to providers forever.
		if !portablePersistedImagePart(part) {
			upgraded, err := upgradeLegacyImagePart(part)
			if err != nil {
				continue
			}
			upgraded.Reference = part.Reference
			msg.Parts[i] = upgraded
			if upgraded.Type != "image_base64" {
				continue
			}
			part = upgraded
		}
		data, err := base64.StdEncoding.DecodeString(part.ImageData)
		if err != nil {
			return messages.ChatMessage{}, fmt.Errorf("decode image artifact %d: %w", i+1, err)
		}
		ref, err := store.Put(ctx, artifacts.Blob{
			Kind: artifacts.KindImage, MIMEType: part.MimeType, Name: part.FileName,
			ImageToken: part.Reference, Reference: part.Reference, Data: data,
		})
		if err != nil {
			return messages.ChatMessage{}, fmt.Errorf("store image artifact %d: %w", i+1, err)
		}
		msg.Parts[i] = messages.ContentPart{
			Type: "image_artifact", MimeType: ref.MIMEType, FileName: ref.Name, Reference: ref.ImageToken, Artifact: &ref,
		}
	}
	return msg, nil
}

// turnPersistenceAllowed asks the turn UI whether the turn may still write to
// the session. The managed REPL declines for a turn it has detached (^C
// cancellation timed out): newer turns may already be appending, so a late
// write would interleave this turn's messages out of order. UIs without an
// opinion allow persistence.
// interruptedTurnMarker records why a partially persisted turn ended. The
// internal role never reaches a provider; hydration uses it to settle the
// turn and label it interrupted instead of leaving it looking abandoned.
func interruptedTurnMarker(cause error) messages.ChatMessage {
	return messages.ChatMessage{
		Role: messages.MessageRoleInternal,
		Metadata: map[string]any{
			messages.MetadataKeyTurnStatus: messages.TurnStatusInterrupted,
			messages.MetadataKeyError:      cause.Error(),
		},
	}
}

// durableTurnMessages removes provider-protocol denial exchanges while
// retaining their safe display projection. The internal marker is never sent
// to a model; hydration uses it to restore disclosure order and to keep an
// all-denied turn from looking like an incomplete composer draft.
func durableTurnMessages(generated []messages.ChatMessage) []messages.ChatMessage {
	stripped := llm.StripDeniedExchanges(generated)
	allDenied := terminalToolBatchAllDenied(generated)
	displayToolCalls := deniedDisplayToolCalls(generated)
	if allDenied || displayToolCalls != "" {
		metadata := make(map[string]any)
		if allDenied {
			metadata[messages.MetadataKeyTurnStatus] = messages.TurnStatusToolDenied
		}
		if reasoning := deniedDisplayReasoning(generated); reasoning != "" {
			metadata[messages.MetadataKeyDisplayReasoning] = reasoning
		}
		if displayToolCalls != "" {
			metadata[messages.MetadataKeyDisplayToolCalls] = displayToolCalls
		}
		stripped = append(stripped, messages.ChatMessage{
			Role:     messages.MessageRoleInternal,
			Metadata: metadata,
		})
	}
	return stripped
}

// durableDisplayToolCall is the safe, UI-only subset needed to restore tool
// disclosure order after denied provider-protocol calls have been stripped.
// Arguments and result bodies are intentionally absent.
type durableDisplayToolCall struct {
	ID     string `json:"id,omitempty"`
	Name   string `json:"name"`
	Denied bool   `json:"denied,omitempty"`
}

func deniedDisplayToolCalls(generated []messages.ChatMessage) string {
	deniedIDs := make(map[string]struct{})
	for _, msg := range generated {
		if msg.Role == messages.MessageRoleTool && msg.Content == llm.ToolDeniedContent {
			deniedIDs[msg.ToolCallID] = struct{}{}
		}
	}
	if len(deniedIDs) == 0 {
		return ""
	}
	var calls []durableDisplayToolCall
	for _, msg := range generated {
		if msg.Role != messages.MessageRoleAssistant {
			continue
		}
		for _, call := range msg.ToolCalls {
			_, denied := deniedIDs[call.ID]
			calls = append(calls, durableDisplayToolCall{ID: call.ID, Name: toolDisplayName(call.Name), Denied: denied})
		}
	}
	if len(calls) == 0 {
		return ""
	}
	encoded, err := json.Marshal(calls)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func decodeDisplayToolCalls(value any) []durableDisplayToolCall {
	encoded, ok := value.(string)
	if !ok || encoded == "" {
		return nil
	}
	var calls []durableDisplayToolCall
	if err := json.Unmarshal([]byte(encoded), &calls); err != nil {
		return nil
	}
	return calls
}

// deniedDisplayReasoning keeps the human-visible reasoning that
// StripDeniedExchanges necessarily drops with a reasoning-only assistant tool
// proposal. Storing it on the internal completion marker avoids both an orphan
// provider message and double-counting it as durable model reasoning.
func deniedDisplayReasoning(generated []messages.ChatMessage) string {
	deniedIDs := make(map[string]struct{})
	for _, msg := range generated {
		if msg.Role == messages.MessageRoleTool && msg.Content == llm.ToolDeniedContent {
			deniedIDs[msg.ToolCallID] = struct{}{}
		}
	}
	var segments []string
	for _, msg := range generated {
		if msg.Role != messages.MessageRoleAssistant || msg.Content != "" || len(msg.ToolCalls) == 0 {
			continue
		}
		allDenied := true
		for _, call := range msg.ToolCalls {
			if _, denied := deniedIDs[call.ID]; !denied {
				allDenied = false
				break
			}
		}
		if allDenied && strings.TrimSpace(msg.Reasoning) != "" {
			segments = append(segments, msg.Reasoning)
		}
	}
	return strings.Join(segments, "\n")
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

func persistUserMessageForTurn(ctx context.Context, session sessions.Session, userMsg messages.ChatMessage, reuseUser bool) error {
	if reuseUser {
		equivalent, err := sessionEndsWithEquivalentUserMessage(ctx, session, userMsg)
		if err != nil {
			return err
		}
		if equivalent {
			return nil
		}
	}
	return session.AddMessage(ctx, userMsg)
}

func sessionEndsWithEquivalentUserMessage(ctx context.Context, session sessions.Session, userMsg messages.ChatMessage) (bool, error) {
	history, err := session.GetHistory(ctx)
	if err != nil {
		return false, err
	}
	return historyEndsWithEquivalentUserMessage(history, userMsg), nil
}

func historyEndsWithEquivalentUserMessage(history []messages.ChatMessage, userMsg messages.ChatMessage) bool {
	if len(history) == 0 {
		return false
	}
	return equivalentUserMessage(history[len(history)-1], userMsg)
}

func equivalentUserMessage(left, right messages.ChatMessage) bool {
	return left.Role == messages.MessageRoleUser &&
		right.Role == messages.MessageRoleUser &&
		left.Content == right.Content &&
		equalContentParts(left.Parts, right.Parts)
}

func equalContentParts(left, right []messages.ContentPart) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		l, r := left[i], right[i]
		lRef, rRef := l.Artifact, r.Artifact
		l.Artifact, r.Artifact = nil, nil
		if l != r {
			return false
		}
		if (lRef == nil) != (rRef == nil) {
			return false
		}
		if lRef != nil && *lRef != *rRef {
			return false
		}
	}
	return true
}

// maxEncodedImageHistoryBytes is a portable request budget, not a provider
// maximum. Counting the persisted base64 text (rather than decoded bytes)
// keeps the projected inline request safely below the tightest native-client
// request ceiling while leaving room for JSON and conversation text.
const (
	maxEncodedImageHistoryBytes = 16 << 20
	// Anthropic's direct API caps each base64-encoded image at 10 MB. Keep the
	// decimal byte limit here (not 10 MiB) so the shared request shape remains
	// portable across direct and compatible endpoints.
	maxPortableEncodedImageBytes = 10_000_000
	// Anthropic's 200k-context models have the smallest native-client request
	// limit: 100 images across the entire request, including earlier turns.
	maxPortableRequestImages = 100
)

// prepareSessionImageRequest projects the exact history that AddMessage will
// expose to llm.Agent. Image hydration and context budgeting happen inside the
// agent; this boundary only avoids duplicating an unchanged persisted draft.
func prepareSessionImageRequest(ctx context.Context, session sessions.Session, userMsg messages.ChatMessage, reuseUser bool) ([]messages.ChatMessage, error) {
	if err := validatePreparedUserMessage(userMsg); err != nil {
		return nil, err
	}
	history, err := session.GetHistory(ctx)
	if err != nil {
		return nil, fmt.Errorf("read session history: %w", err)
	}
	reusingTerminalUser := reuseUser && historyEndsWithEquivalentUserMessage(history, userMsg)
	if !reusingTerminalUser {
		history = append(history, userMsg)
	}
	// llm.Agent now owns provider-visible image selection and context
	// projection. The canonical transcript remains complete here.
	return normalizeLegacyImagesForProjection(modelVisibleHistory(history)), nil
}

func normalizeLegacyImagesForProjection(history []messages.ChatMessage) []messages.ChatMessage {
	normalized := make([]messages.ChatMessage, len(history))
	for i, msg := range history {
		normalized[i] = cloneChatMessage(msg)
		for j, part := range normalized[i].Parts {
			if part.Type != "image_base64" || portablePersistedImagePart(part) {
				continue
			}
			if upgraded, err := upgradeLegacyImagePart(part); err == nil {
				upgraded.Reference = part.Reference
				normalized[i].Parts[j] = upgraded
			}
		}
	}
	return normalized
}

func validatePreparedUserMessage(msg messages.ChatMessage) error {
	if _, err := preparePortableImageRequest([]messages.ChatMessage{msg}); err != nil {
		return err
	}
	images := 0
	for _, part := range msg.Parts {
		if part.Type == "image_base64" || part.Type == "image_url" ||
			(part.Artifact != nil && part.Artifact.Kind == artifacts.KindImage) {
			images++
		}
	}
	if images > maxPromptAttachments {
		return fmt.Errorf("model-visible message 1 has %d images; portable maximum is %d", images, maxPromptAttachments)
	}
	return nil
}

func preparePortableImageRequest(history []messages.ChatMessage) ([]messages.ChatMessage, error) {
	// Reject an oversized persisted request before decoding legacy base64 into a
	// second in-memory copy.
	if err := validateEncodedImageBudget(history); err != nil {
		return nil, err
	}
	normalized := make([]messages.ChatMessage, len(history))
	for messageIndex, msg := range history {
		normalized[messageIndex] = cloneChatMessage(msg)
		for partIndex, part := range normalized[messageIndex].Parts {
			if part.Type != "image_base64" || portablePersistedImagePart(part) {
				continue
			}
			upgraded, err := upgradeLegacyImagePart(part)
			if err != nil {
				return nil, fmt.Errorf("model-visible message %d has a legacy image that cannot be normalized: %w", messageIndex+1, err)
			}
			normalized[messageIndex].Parts[partIndex] = upgraded
		}
	}
	if err := validatePortableImageRequest(normalized); err != nil {
		return nil, err
	}
	return normalized, nil
}

func upgradeLegacyImagePart(part messages.ContentPart) (messages.ContentPart, error) {
	decoder := base64.NewDecoder(base64.StdEncoding, strings.NewReader(part.ImageData))
	data, err := io.ReadAll(io.LimitReader(decoder, int64(maxLocalImageBytes)+1))
	if err != nil || len(data) == 0 {
		return messages.ContentPart{}, fmt.Errorf("invalid or empty base64 data")
	}
	if len(data) > maxLocalImageBytes {
		return messages.ContentPart{}, fmt.Errorf("decoded image exceeds the %d MiB preparation limit", maxLocalImageBytes>>20)
	}
	if strings.EqualFold(strings.TrimSpace(part.MimeType), "image/svg+xml") || strings.EqualFold(filepath.Ext(part.FileName), ".svg") {
		label := strings.TrimSpace(part.FileName)
		if label == "" {
			label = "legacy.svg"
		}
		return messages.ContentPart{Type: "text", Text: "[legacy SVG image omitted: " + label + "]", FileName: part.FileName}, nil
	}
	upgraded, err := prepareImageBytesForUpload(data, part.FileName)
	if err != nil {
		return messages.ContentPart{}, err
	}
	return *upgraded, nil
}

// validatePortableImageRequest enforces the common request shape accepted by
// every native multimodal client. It deliberately validates the whole visible
// history, not only the candidate: a legacy image in an earlier turn is replayed
// to the provider too and can otherwise poison a restored draft or a new prompt.
func validatePortableImageRequest(history []messages.ChatMessage) error {
	if err := validateEncodedImageBudget(history); err != nil {
		return err
	}
	totalImages := 0
	for messageIndex, msg := range history {
		imageCount := 0
		for _, part := range msg.Parts {
			switch part.Type {
			case "image_base64":
				imageCount++
				totalImages++
				if imageCount > maxPromptAttachments {
					return fmt.Errorf("model-visible message %d has %d images; portable maximum is %d", messageIndex+1, imageCount, maxPromptAttachments)
				}
				if totalImages > maxPortableRequestImages {
					return fmt.Errorf("model-visible history has %d images; portable request maximum is %d", totalImages, maxPortableRequestImages)
				}
				if err := validatePortablePersistedImagePart(part); err != nil {
					return fmt.Errorf("model-visible message %d has a nonportable image: %w", messageIndex+1, err)
				}
			case "image_url":
				return fmt.Errorf("model-visible message %d has an image URL; portable requests require prepared PNG, JPEG, or WebP bytes", messageIndex+1)
			}
		}
	}
	return nil
}

func validateEncodedImageBudget(history []messages.ChatMessage) error {
	total := 0
	for _, msg := range history {
		for _, part := range msg.Parts {
			switch part.Type {
			case "image_base64":
				total += len(part.ImageData)
			case "image_url":
				if strings.HasPrefix(part.ImageURL, "data:") {
					if comma := strings.IndexByte(part.ImageURL, ','); comma >= 0 {
						total += len(part.ImageURL) - comma - 1
					}
				}
			}
			if total > maxEncodedImageHistoryBytes {
				return fmt.Errorf("encoded images in model-visible history would use %d bytes; portable limit is %d MiB", total, maxEncodedImageHistoryBytes>>20)
			}
		}
	}
	return nil
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

// createCompletionRequest builds an LLM completion request from config.
func createCompletionRequest(config *Config, history []messages.ChatMessage, registry *tools.ToolRegistry, skillCatalog *skills.Catalog, schema *llm.Schema) *llm.CompletionRequest {
	// Parse thinking effort - already validated at config parsing time
	thinkingEffort, _ := llm.ParseThinkingEffort(config.ThinkingEffort)

	return &llm.CompletionRequest{
		BaseURL:          config.BaseURL,
		Timeout:          config.Timeout,
		Deadline:         config.Deadline,
		Temperature:      llm.Float32Ptr(float32(config.Temperature)),
		Model:            config.Model,
		MaxTokens:        config.MaxTokens,
		MaxContextTokens: config.MaxHistoryTokens,
		Messages:         history,
		Skills:           skillCatalog,
		Tools:            registry.All(),
		ResponseSchema:   schema,
		ThinkingEffort:   thinkingEffort,
	}
}

// initializeConversation handles all the setup needed before starting a conversation
func initializeConversation(ctx context.Context, config *Config, sessionStore sessions.SessionStore, contextID string, cmd *cli.Command) (string, *sessions.Metadata, error) {
	var needReset bool
	var originalContextInfo *sessions.Metadata

	// Load context settings if available
	if contextID != "" {
		metadata, err := sessionStore.GetAllMetadata(ctx)
		if err != nil {
			return "", nil, fmt.Errorf("list context metadata: %w", err)
		}
		if contextInfo := metadata[contextID]; contextInfo != nil {
			originalContextInfo = contextInfo

			// Check if system prompt is being changed (only if context has
			// existing conversation). A stored legacy default reads as the
			// empty persona, so -s "" against it does not wipe history.
			if cmd.IsSet("system") && cmd.String("system") != normalizeLegacySystemPrompt(contextInfo.SystemPrompt) {
				// Check if there's an existing conversation to reset
				exists, err := sessionStore.Exists(ctx, contextInfo.Name)
				if err != nil {
					return "", nil, fmt.Errorf("check context %q: %w", contextInfo.Name, err)
				}
				if exists {
					needReset = true
					fmt.Fprintf(os.Stderr, "System prompt changed, resetting conversation...\n")
				}
			}

			// Persisted settings are authoritative for an existing session. Zero
			// and empty values are intentional settings too, so copy every field
			// that was not explicitly overridden on this invocation.
			for _, spec := range settingSpecs {
				if !spec.flagged() || cmd.IsSet(spec.key) {
					continue
				}
				spec.fromMeta(config, contextInfo)
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
		// Store an explicitly changed prompt before Clear: Clear rebuilds the
		// system message from session metadata, including the meaningful empty
		// prompt case.
		if err := resetContextWithSystemPrompt(ctx, sessionStore, contextName, config.Settings.SystemPrompt); err != nil {
			return "", nil, fmt.Errorf("failed to reset context: %w", err)
		}
		// Context name remains the same after reset
		contextID = contextName
	}

	return contextID, originalContextInfo, nil
}

// applyFlagSettings copies only explicitly-set CLI flags onto md, so a plain
// --reset keeps stored settings instead of replacing them with defaults.
func applyFlagSettings(md *sessions.Metadata, config *Config, cmd *cli.Command) {
	for _, spec := range settingSpecs {
		if spec.flagSet(cmd) {
			spec.toMeta(config, md)
		}
	}
}

// updateContextInfo writes the resolved settings onto md, the metadata staged
// by newConversationState, and persists it: the startup write-back rows
// always (config holds the stored value unless a flag overrode it, see
// initializeConversation), every other row only when its flag was given.
// Name and LastUsed are storage-owned: SetMetadata overwrites both, so they
// are not written here.
func updateContextInfo(ctx context.Context, session sessions.Session, md *sessions.Metadata, config *Config, cmd *cli.Command) error {
	for _, spec := range settingSpecs {
		if spec.startupWriteBack() || spec.flagSet(cmd) {
			spec.toMeta(config, md)
		}
	}
	return session.SetMetadata(ctx, md)
}

// beforeExit is invoked synchronously by cleanupAndExit before os.Exit. Signal
// handling itself only cancels the run context, so ordinary shutdown unwinds
// through defers; this hook remains a final guard for explicit process exits.
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

// shutdownSignal is carried as the cancellation cause so main can distinguish
// an expected process signal from an ordinary context cancellation after all
// command defers have unwound.
type shutdownSignal struct {
	signal os.Signal
}

func (e *shutdownSignal) Error() string {
	return e.signal.String()
}

// splitSignalError finds the process signal while retaining independent
// cleanup failures joined during deferred unwinding. The expected signal branch
// stays silent, but losing the session/store/tool cleanup error would hide
// durable-state failures at exactly the point they matter most.
func splitSignalError(err error) (int, error, bool) {
	var shutdown *shutdownSignal
	if !errors.As(err, &shutdown) {
		return 0, nil, false
	}
	code := 1
	switch shutdown.signal {
	case os.Interrupt:
		code = 130
	case syscall.SIGTERM:
		code = 143
	}
	return code, stripShutdownSignal(err), true
}

func stripShutdownSignal(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := err.(*shutdownSignal); ok {
		return nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		remaining := make([]error, 0, len(children))
		for _, child := range children {
			if child = stripShutdownSignal(child); child != nil {
				remaining = append(remaining, child)
			}
		}
		return errors.Join(remaining...)
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		child := wrapped.Unwrap()
		if child == nil {
			return err
		}
		var shutdown *shutdownSignal
		if !errors.As(child, &shutdown) {
			return err
		}
		return stripShutdownSignal(child)
	}
	return err
}

// setupSignalHandling sets up signal handling for graceful shutdown. The
// returned stop function unregisters the process handlers and cancels the
// context when the caller finishes normally.
func setupSignalHandling(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(ctx)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case received := <-signals:
			cancel(&shutdownSignal{signal: received})
		case <-ctx.Done():
		}
	}()
	return ctx, func() {
		signal.Stop(signals)
		cancel(context.Canceled)
	}
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
