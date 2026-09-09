package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
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
	"github.com/alexschlessinger/pollytool/swarm"
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
	swarm          *swarm.Runtime
	workspaceEntry *workspaceEntry
	sessionStore   sessions.SessionStore
	session        sessions.Session
	// settings are this session's own: resolved from its stored metadata
	// when it was opened, changed by /set, and read by every turn on it.
	settings        Settings
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
	// instructionWarnings is the last set of repository-instruction warnings
	// shown, so a persistent problem is reported once rather than every turn.
	instructionWarnings []string
	// displayContract is composed into the request's system message each turn;
	// it is capability-specific and never persisted (see display_contract.go).
	displayContract string
	// outputCapabilities is resolved once per process run so the model-facing
	// contract and the concrete line renderer cannot disagree.
	outputCapabilities outputCapabilities
	// contextWindows caches per-model context-window discovery for this
	// process, including failed attempts (entry present, value 0).
	contextWindowsMu sync.Mutex
	contextWindows   map[string]int
	// memberUI, when set, takes the approvals of swarm members that run
	// outside a turn (launched by a command, or woken by peer mail): the
	// managed REPL binds the screen of the tab holding this session. turnUI
	// is the running turn's UI, the fallback for hosts without a screen.
	uiMu     sync.Mutex
	memberUI TurnUI
	turnUI   TurnUI
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
		contextID, err = generateSessionName(ctx, sessionStore)
		if err != nil {
			return nil, closeStoreAfterError(sessionStore, err)
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

	return r.runConversation()
}

func (r *commandRunner) handleManagementFlags() (bool, error) {
	cfg := r.config
	store := r.sessionStore

	if cfg.ResetContext != "" {
		return true, handleResetContext(r.ctx, store, cfg, r.cmd, cfg.ResetContext)
	}
	if cfg.ListContexts {
		return true, handleListContexts(r.ctx, store, cfg.FlatList)
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
func newConversationState(ctx context.Context, config *Config, llmClient *llm.MultiPass, sessionStore sessions.SessionStore, contextID string, autoContext bool, cmd *cli.Command, sandboxWarnings *broadWritablePathWarner) (*conversationState, error) {
	contextID, settings, err := initializeConversation(ctx, config, sessionStore, contextID, cmd)
	if err != nil {
		return nil, err
	}
	return openConversationState(ctx, config, settings, llmClient, sessionStore, contextID, autoContext, cmd, sandboxWarnings)
}

// openConversationState is the second half of newConversationState: it
// acquires contextID and builds its runtime from the settings that
// initializeConversation resolved for it. It only reads config, so the
// managed REPL may run it off the UI goroutine while the visible session
// keeps serving input.
func openConversationState(ctx context.Context, config *Config, settings Settings, llmClient *llm.MultiPass, sessionStore sessions.SessionStore, contextID string, autoContext bool, cmd *cli.Command, sandboxWarnings *broadWritablePathWarner) (state *conversationState, retErr error) {
	var err error
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
	if metadata.SwarmID != "" {
		return nil, fmt.Errorf("this swarm member is inspected through /sessions and resumed through its parent %q with /swarm resume; independent execution would lose its worktree binding", metadata.Parent)
	}

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

	registryOpts, probe, err := sandboxRegistryOptionsWithWarnings(config, sandboxWarnings)
	if err != nil {
		return nil, err
	}
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
		toolRegistry, err = loadTools(metadata.ActiveTools, registryOpts...)
		if err != nil {
			return nil, loadErr(err)
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
	if err := updateContextInfo(ctx, session, metadata, &settings, cmd); err != nil {
		return nil, err
	}

	artifactStore := session.ArtifactStore()
	agent := llm.NewAgent(llmClient, toolRegistry, llm.AgentConfig{
		MaxIterations: settings.MaxIterations,
		ToolTimeout:   settings.ToolTimeout,
		ArtifactStore: artifactStore,
	})
	state = &conversationState{
		sessionStore:    sessionStore,
		session:         session,
		settings:        settings,
		agent:           agent,
		artifactStore:   artifactStore,
		toolRegistry:    toolRegistry,
		skillCatalog:    skillResult.catalog,
		skillRuntime:    skillRuntime,
		skillSources:    skillResult.sources,
		sandboxWarnings: sandboxWarnings,
		sandboxProbe:    probe,
	}
	registerSessionTitleTool(state)
	if err := registerSwarm(state, config, llmClient); err != nil {
		return nil, err
	}
	return state, nil
}

func sandboxRegistryOptionsWithWarnings(config *Config, warnings *broadWritablePathWarner) ([]tools.RegistryOption, *sandboxProbe, error) {
	if config.NoSandbox {
		return []tools.RegistryOption{tools.WithUnsafeNoSandbox()}, nil, nil
	}
	if warnings == nil {
		warnings = newBroadWritablePathWarner()
	}

	baseCfg, err := sandbox.ParsePreset(config.SandboxPreset)
	if err != nil {
		return nil, nil, err
	}
	baseCfg = baseCfg.Merge(sandbox.Config{
		WritablePaths: config.WritePaths,
		DenyPaths:     config.DenyPaths,
		AllowNetwork:  config.AllowNet,
	})
	baseCfg, err = sandbox.PrepareConfig(baseCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("prepare sandbox config: %w", err)
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
		return nil, nil, fmt.Errorf("sandbox requested but unavailable: %w", err)
	}
	// ...and that it can actually start a command. Construction alone misses
	// environments where the backend is present but fails at runtime; without
	// this probe every bash call would silently return a refusal while the run
	// still exits 0/ok. The spawn costs tens of milliseconds, so it runs off
	// the open; the first turn waits on it before any tool can run, and the
	// open itself consults it only when a tool that spawns while loading
	// fails (see openConversationState).
	return []tools.RegistryOption{tools.WithSandboxFactory(warningFactory, baseCfg)}, startSandboxProbe(sb), nil
}

// sandboxProbe is one asynchronous sandbox.Probe. wait blocks until the
// spawned command has reported, returning the startup failure with its
// escape hatch, or the caller's cancellation.
type sandboxProbe struct {
	done chan struct{}
	err  error
}

func startSandboxProbe(sb sandbox.Sandbox) *sandboxProbe {
	p := &sandboxProbe{done: make(chan struct{})}
	go func() {
		defer close(p.done)
		if err := sandbox.Probe(sb); err != nil {
			p.err = fmt.Errorf("sandbox requested but failed to start: %w\n"+
				"Set POLLYTOOL_NOSANDBOX=1 (or pass --nosandbox) to run without the sandbox", err)
		}
	}()
	return p
}

// wait is safe on a nil probe, which is what --nosandbox produces.
func (p *sandboxProbe) wait(ctx context.Context) error {
	if p == nil {
		return nil
	}
	select {
	case <-p.done:
		return p.err
	case <-ctx.Done():
		return context.Cause(ctx)
	}
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

func (r *commandRunner) runConversation() (retErr error) {
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
	var entry *workspaceEntry
	contextID := r.contextID
	openCtx := ctx
	if input.mode == conversationModeREPL && managedREPL && !r.autoContext && contextID != "" {
		if reader, ok := r.sessionStore.(sessions.ViewStore); ok {
			entry, err = resolveWorkspaceEntry(ctx, reader, contextID)
			if err != nil && !errors.Is(err, sessions.ErrSessionNotFound) {
				return err
			}
			if entry != nil {
				contextID = entry.root.Metadata.Name
				openCtx = context.WithValue(ctx, childViewIdentityKey{}, entry.root.ID)
			}
		}
	}
	var state *conversationState
	if entry != nil && entry.root.InUse {
		state = readOnlyConversationState(config, r.sessionStore, entry.root)
	} else {
		state, err = newConversationState(openCtx, config, r.llmClient, r.sessionStore, contextID, r.autoContext, r.cmd, newBroadWritablePathWarner())
	}
	if err != nil {
		return err
	}
	state.workspaceEntry = entry
	state.displayContract = displayContractFor(outputCapabilities)
	state.outputCapabilities = outputCapabilities
	session := state.session

	// Set up signal handling
	signalCtx, cancelSignal := setupSignalHandling(ctx)
	defer cancelSignal()

	if input.mode == conversationModeREPL && managedREPL {
		// Each session's lease context parents that session's turns and ends
		// the run when it is lost (see managedREPL.Run), so the run context
		// carries signals only. The REPL owns state from here: it closes
		// every session it holds at exit, which also discards a generated
		// session that never ran a turn.
		opener := &sessionOpener{
			prepare: func(ctx context.Context, name string, notify func(string)) (string, Settings, error) {
				return prepareConversation(ctx, config, r.sessionStore, name, r.cmd, notify)
			},
			open: func(ctx context.Context, name string, settings Settings, auto bool) (*conversationState, error) {
				opened, err := openConversationState(ctx, config, settings, r.llmClient, r.sessionStore, name, auto, r.cmd, newBroadWritablePathWarner())
				if err != nil {
					return nil, err
				}
				opened.displayContract = displayContractFor(outputCapabilities)
				opened.outputCapabilities = outputCapabilities
				return opened, nil
			},
			newName: func(ctx context.Context) (string, error) {
				return generateSessionName(ctx, r.sessionStore)
			},
		}
		return runManagedREPL(signalCtx, config, state, opener)
	}

	defer func() {
		if err := state.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close conversation state: %w", err))
		}
	}()

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
		replErr := runFallbackREPL(ctx, config, state)
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

	// The details trailer is one-shot chrome; an exported
	// POLLYTOOL_ACTIVITY_DETAILS must not change how the REPL runs.
	config.ActivityDetails = false
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
// The phases live on turnExecution; this sequences them and owns the turn
// UI's lifecycle.
func executeTurnWithUserMessage(ctx context.Context, config *Config, state *conversationState, userMsg messages.ChatMessage, schema *llm.Schema, inputReader *bufio.Reader, turnUI TurnUI, reuseUser bool) (exitCode int, finalErr error) {
	t := &turnExecution{ctx: ctx, config: config, state: state, settings: &state.settings, schema: schema, userMsg: userMsg, reuseUser: reuseUser}
	requestMessages, instructionWarnings, err := t.prepareRequest()
	if err != nil {
		return 1, err
	}

	if turnUI == nil {
		turnUI = newLineTurnUIWithCapabilities(config, inputReader, state.outputCapabilities)
	}
	t.turnUI = turnUI
	turnUI.Start()
	defer turnUI.Stop()
	state.setTurnUI(turnUI)
	defer state.setTurnUI(nil)
	activityStart := time.Now()
	completed := false
	complete := func(reason messages.StopReason, err error) {
		if !completed {
			completed = true
			turnUI.CompleteTurn(turnCompletion{Reason: reason, Err: err, Elapsed: time.Since(activityStart), ProgressSaved: err == nil || turnProgressSaved(err)})
		}
	}
	defer func() {
		if !completed {
			if outputErr := flushTurnOutputError(turnUI); outputErr != nil {
				finalErr, exitCode = errors.Join(finalErr, outputErr), 1
			}
			complete(messages.StopReasonError, finalErr)
		}
	}()
	for _, warning := range instructionWarnings {
		turnUI.AppendWarning(warning)
	}

	req := createCompletionRequest(config, t.settings, requestMessages, state.effectiveTools(), state.skillCatalog, schema)
	req.MaxContextTokens = resolveContextBudget(ctx, state)
	req.CacheSessionID, err = cacheSessionIDForSession(ctx, state.session)
	if err != nil {
		return 1, err
	}
	if tui, ok := turnUI.(*gotuiTurnUI); ok {
		t.reportIDs = tui.turn.reportIDs
	}

	// The sandbox probe started with the open and has normally long
	// finished. A backend that cannot start fails the turn here, before the
	// first request and before the user message persists, rather than as
	// silent tool refusals later.
	if err := state.sandboxProbe.wait(ctx); err != nil {
		return 1, err
	}

	turnStart := time.Now()
	line, lineOutput := turnUI.(*lineTurnUI)
	t.settledOutput = lineOutput && !config.Stream && !line.interactive
	if lineOutput {
		line.settledOutput = t.settledOutput
	}
	callbacks := t.callbacks(req)
	if state.swarm != nil {
		updateSwarmDefaults(state, req, *t.settings)
		state.swarm.BindParent(callbacks, turnUI.TurnPersistenceAllowed)
	}
	resp, err := state.agent.Run(ctx, req, callbacks)
	if ctx.Err() != nil {
		// Cancellation outranks whatever error the aborted run surfaced, but
		// the turn still flows through persistence below: tools that completed
		// changed the world whether or not the user hit cancel.
		err = context.Cause(ctx)
	}
	if err != nil && !t.persistAttempted && !t.reusingPersistedUser {
		// The run stopped before its first projection cleared the request.
		err = fmt.Errorf("prompt was not added to the conversation: %w", err)
	}
	in, out := t.recordUsage(resp)

	// Folding every later stage's error into runErr means the trailer and
	// exit code below always describe the turn's final state, whichever
	// stage failed.
	runErr := t.settlePersistence(resp, err)
	if runErr == nil {
		runErr = t.finishOutput(resp)
	}
	if runErr != nil && t.settledOutput && config.SchemaPath == "" {
		name, _ := state.session.GetName(context.WithoutCancel(ctx))
		turnUI.AppendAssistantText(settledAnswer(resp, runErr, name))
		turnUI.FinishTextTurn()
	}

	if outputErr := flushTurnOutputError(turnUI); outputErr != nil {
		runErr = errors.Join(runErr, outputErr)
	}
	stopReason, code := classifyOutcome(resp, runErr)
	complete(stopReason, runErr)
	if outputErr := flushTurnOutputError(turnUI); outputErr != nil {
		runErr = errors.Join(runErr, outputErr)
		stopReason, code = classifyOutcome(resp, runErr)
	}
	if config.Meta {
		writeMetaTrailer(os.Stderr, buildMeta(stopReason, resp, runErr, t.settings.Model, &t.stats, in, out, time.Since(turnStart).Milliseconds()))
	}
	return code, runErr
}

// settledAnswer is what a failed one-shot run still prints on stdout: the
// answer the model produced, so a consumer keeps it and reads the failure from
// stderr, or a blocker report naming the session when there is no answer.
func settledAnswer(resp *llm.AgentResponse, runErr error, session string) string {
	if resp != nil && resp.Message != nil && strings.TrimSpace(resp.Message.Content) != "" {
		return resp.Message.Content
	}
	return "Blocked: " + runErr.Error() + "\n\nSession: " + session + "\n"
}

// externalizeMessageImages replaces prepared base64 image parts with private
// content-addressed references. Artifact storage is authoritative, so a write
// failure is returned instead of silently persisting a second inline format.
func externalizeMessageImages(ctx context.Context, msg messages.ChatMessage, store artifacts.Store) (messages.ChatMessage, error) {
	msg = msg.Clone()
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
		part, err := messages.PortableImagePart(part)
		if err != nil {
			continue
		}
		if part.Type != "image_base64" {
			msg.Parts[i] = part
			continue
		}
		ref, err := storeImagePart(ctx, store, part, part.Reference)
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
	// StopReason is already persisted on ChatMessage. Keep an explicit cap
	// reason on this display-only marker so hydration need not parse errors.
	reason := messages.StopReason("")
	if onlyIterationLimit(cause) {
		reason = messages.StopReasonMaxIterations
	}
	return messages.ChatMessage{
		Role:       messages.MessageRoleInternal,
		StopReason: reason,
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
		if reasoning, thinking := deniedDisplayReasoning(generated); reasoning != "" {
			metadata[messages.MetadataKeyDisplayReasoning] = reasoning
			if thinking > 0 {
				metadata[messages.MetadataKeyThinkingMillis] = int(max(thinking.Milliseconds(), 1))
			}
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
// provider message and double-counting it as durable model reasoning. The
// summed thinking time of those stripped messages rides along so the resumed
// disclosure keeps its elapsed label.
func deniedDisplayReasoning(generated []messages.ChatMessage) (string, time.Duration) {
	deniedIDs := make(map[string]struct{})
	for _, msg := range generated {
		if msg.Role == messages.MessageRoleTool && msg.Content == llm.ToolDeniedContent {
			deniedIDs[msg.ToolCallID] = struct{}{}
		}
	}
	var segments []string
	var thinking time.Duration
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
			thinking += msg.ThinkingDuration()
		}
	}
	return strings.Join(segments, "\n"), thinking
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

// persistUserMessageForTurn appends the turn's user message unless a matching
// retry already persisted it. Report input consumes its reports in the same
// write, including a restored report draft whose first persist failed.
func persistUserMessageForTurn(ctx context.Context, session sessions.Session, userMsg messages.ChatMessage, reuseUser bool, reportIDs []int64) error {
	if reuseUser {
		equivalent, err := sessionEndsWithEquivalentUserMessage(ctx, session, userMsg)
		if err != nil {
			return err
		}
		if equivalent {
			return nil
		}
	}
	if len(reportIDs) > 0 {
		return session.AddReportMessage(ctx, userMsg, reportIDs)
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

// prepareSessionImageRequest projects the exact history that AddMessage will
// expose to llm.Agent. Image hydration and context budgeting happen inside the
// agent; this boundary only avoids duplicating an unchanged persisted draft.
func prepareSessionImageRequest(ctx context.Context, session sessions.Session, userMsg messages.ChatMessage, reuseUser bool) ([]messages.ChatMessage, error) {
	if err := messages.ValidateImageMessage(userMsg); err != nil {
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
	return messages.NormalizeImages(messages.ModelVisible(history)), nil
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
		MaxTokens:        settings.MaxTokens,
		MaxContextTokens: settings.MaxHistoryTokens,
		Messages:         history,
		Skills:           skillCatalog,
		Tools:            registry.All(),
		ResponseSchema:   schema,
		ThinkingEffort:   thinkingEffort,
	}
}

// initializeConversation resolves contextID's settings before a conversation
// starts, reporting a history reset on stderr.
func initializeConversation(ctx context.Context, config *Config, sessionStore sessions.SessionStore, contextID string, cmd *cli.Command) (string, Settings, error) {
	return prepareConversation(ctx, config, sessionStore, contextID, cmd, func(line string) {
		fmt.Fprintln(os.Stderr, line)
	})
}

// prepareConversation resolves the settings contextID will run with: the
// launch settings, with every stored value that no flag overrode restored
// from the session's metadata. When an explicit --system differs from the
// stored prompt the session's history is reset, reported through notify so
// the managed REPL can show it without writing to a terminal it owns. The
// returned name is the session's stored name when it exists.
func prepareConversation(ctx context.Context, config *Config, sessionStore sessions.SessionStore, contextID string, cmd *cli.Command, notify func(string)) (string, Settings, error) {
	settings := config.Launch.clone()
	var needReset bool
	var originalContextInfo *sessions.Metadata

	// Load context settings if available
	if contextID != "" {
		metadata, err := sessionStore.GetAllMetadata(ctx)
		if err != nil {
			return "", Settings{}, fmt.Errorf("list context metadata: %w", err)
		}
		if contextInfo := metadata[contextID]; contextInfo != nil {
			originalContextInfo = contextInfo

			// Check if system prompt is being changed (only if context has
			// existing conversation).
			if cmd.IsSet("system") && cmd.String("system") != contextInfo.SystemPrompt {
				// Check if there's an existing conversation to reset
				exists, err := sessionStore.Exists(ctx, contextInfo.Name)
				if err != nil {
					return "", Settings{}, fmt.Errorf("check context %q: %w", contextInfo.Name, err)
				}
				if exists {
					needReset = true
					notify("System prompt changed, resetting conversation...")
				}
			}

			// Persisted settings are authoritative for an existing session. Zero
			// and empty values are intentional settings too, so copy every field
			// that was not explicitly overridden on this invocation.
			for _, spec := range settingSpecs {
				if !spec.flagged() || cmd.IsSet(spec.key) {
					continue
				}
				spec.fromMeta(&settings, contextInfo)
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
		if err := resetContextWithSystemPrompt(ctx, sessionStore, contextName, settings.SystemPrompt); err != nil {
			return "", Settings{}, fmt.Errorf("failed to reset context: %w", err)
		}
		// Context name remains the same after reset
		contextID = contextName
	}

	return contextID, settings, nil
}

// applyFlagSettings copies only explicitly-set CLI flags onto md, so a plain
// --reset keeps stored settings instead of replacing them with defaults.
func applyFlagSettings(md *sessions.Metadata, settings *Settings, cmd *cli.Command) {
	for _, spec := range settingSpecs {
		if spec.flagSet(cmd) {
			spec.toMeta(settings, md)
		}
	}
}

// updateContextInfo writes the resolved settings onto md, the metadata staged
// by openConversationState, and persists it: every flagged row, since the
// resolved settings hold the stored value unless a flag overrode it (see
// prepareConversation), so the copy is a no-op for an untouched row and an
// override for a set flag. Name and LastUsed are storage-owned: SetMetadata
// overwrites both, so they are not written here.
func updateContextInfo(ctx context.Context, session sessions.Session, md *sessions.Metadata, settings *Settings, cmd *cli.Command) error {
	for _, spec := range settingSpecs {
		if spec.flagged() {
			spec.toMeta(settings, md)
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

// readFromStdin reads all of stdin as one prompt: CRLF line endings are
// normalized and the trailing newline dropped. The whole input is read rather
// than scanned line by line, so a single long line — minified JSON, a source
// map, a document without breaks — is not rejected at bufio.Scanner's default
// 64 KiB line limit.
func readFromStdin() (string, error) {
	return readAllInput(os.Stdin)
}

func readAllInput(r io.Reader) (string, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("error reading stdin: %w", err)
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	return strings.TrimSuffix(text, "\n"), nil
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

// outputStructured validates and prints a structured response. Only output
// that parses as JSON and satisfies the schema reaches stdout; anything else
// is an error, with the raw reply on stderr for inspection, so a pipeline
// reading --schema output never mistakes a malformed or off-schema reply for
// success.
func outputStructured(content string, schema *llm.Schema) error {
	return writeStructured(os.Stdout, os.Stderr, content, schema)
}

func writeStructured(stdout, stderr io.Writer, content string, schema *llm.Schema) error {
	// Empty content means no structured output was produced — e.g. the model
	// emitted a tool call that was denied and the turn short-circuited. Report
	// it instead of printing a silent blank line that looks like success.
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("no structured output produced")
	}
	var data any
	if err := json.Unmarshal([]byte(content), &data); err != nil {
		fmt.Fprintln(stderr, content)
		return fmt.Errorf("structured output is not valid JSON: %w", err)
	}
	if err := validateJSONAgainstSchema(content, schema); err != nil {
		fmt.Fprintln(stderr, content)
		return fmt.Errorf("structured output does not match the schema: %w", err)
	}
	jsonBytes, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("format structured output: %w", err)
	}
	fmt.Fprintln(stdout, string(jsonBytes))
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
