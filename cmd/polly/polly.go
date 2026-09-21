package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/alexschlessinger/pollytool/internal/log"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/urfave/cli/v3"
)

func main() {
	command := getCommand()
	if err := command.Run(context.Background(), normalizeCommandArgs(os.Args)); err != nil {
		// Signal cancellation travels through the ordinary command return path so
		// session, store, and terminal defers all run before the process exits.
		// Do not render that expected shutdown as a generic command error.
		code, report := 1, err
		var ee *exitError
		if signalCode, remaining, ok := splitSignalError(err); ok {
			code, report = signalCode, remaining
		} else if errors.As(err, &ee) {
			code, report = ee.code, ee.err
		}
		if report != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", report)
		}
		cleanupAndExit(code)
	}
}

// normalizeCommandArgs treats the leading ask command exactly like -p,
// leaving prompt values and arguments to other commands untouched.
func normalizeCommandArgs(args []string) []string {
	if len(args) > 1 && args[1] == "ask" {
		args = append([]string(nil), args...)
		args[1] = "-p"
	}
	return args
}

type commandRunner struct {
	conversationOpener
	ctx       context.Context
	contextID string
	// autoContext marks a generated REPL context name (no -c given): its
	// creation is silent and it is discarded on exit if no turn ever ran.
	autoContext bool
}

type conversationMode int

const (
	conversationModeOneShot conversationMode = iota
	conversationModeREPL
	// conversationModeShot runs the same TUI as conversationModeREPL, off-screen
	// and driven by a shot script instead of a keyboard (--shot-script).
	conversationModeShot
)

type conversationInput struct {
	mode   conversationMode
	prompt string
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
			return nil, closeAfterError(sessionStore, "context store", fmt.Errorf("failed to find last context: %w", err))
		}
		if contextID == "" {
			return nil, closeAfterError(sessionStore, "context store", fmt.Errorf("no last context found"))
		}
	}
	if autoContext {
		contextID, err = generateSessionName(ctx, sessionStore)
		if err != nil {
			return nil, closeAfterError(sessionStore, "context store", err)
		}
	}

	return &commandRunner{
		conversationOpener: conversationOpener{config: config, llmClient: llm.NewMultiPass(loadAPIKeys()), sessionStore: sessionStore, cmd: cmd},
		ctx:                ctx,
		contextID:          contextID,
		autoContext:        autoContext,
	}, nil
}

// openNew opens the launch session and refuses to run without a credential
// for its provider: every launch path opens through here, and the session's
// own stored model is what gets judged. A workspace already open in another
// process never reaches this; its turns run there, on that process's keys.
func (r *commandRunner) openNew(ctx context.Context, contextID string, autoContext bool) (*conversationState, error) {
	state, err := r.conversationOpener.openNew(ctx, contextID, autoContext)
	if err != nil {
		return nil, err
	}
	if err := missingKeyError(r.llmClient, state.settings.Model, r.config.BaseURL); err != nil {
		return nil, errors.Join(err, state.Close())
	}
	return state, nil
}

// wantsAutoREPLContext reports whether this invocation will land in the
// interactive REPL with no context of its own: the same mode selection the
// conversation makes later, minus a context-management flag. Only those runs
// get an auto-generated persistent context.
func wantsAutoREPLContext(config *Config) bool {
	if needsFileStore(config, "") {
		return false
	}
	mode, err := selectConversationMode(config, hasStdinData())
	return err == nil && mode == conversationModeREPL
}

func (r *commandRunner) Run() (retErr error) {
	defer func() {
		if err := r.sessionStore.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close context store: %w", err))
		}
	}()
	if op := r.config.Management; op != nil {
		return op.run(r, r.config.ManagementArg)
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

func runCommand(ctx context.Context, cmd *cli.Command) error {
	runner, err := newCommandRunner(ctx, cmd)
	if err != nil {
		return err
	}

	return runner.Run()
}

// runConversation runs the one-shot path, the interactive TUI, or a headless
// shot run.
func (r *commandRunner) runConversation() (retErr error) {
	ctx, config := r.ctx, r.config
	input, err := resolveConversationInput(config)
	if err != nil {
		return err
	}
	// A shot run plays a script and writes PNGs, so its output is files, not a
	// terminal: it needs no tty to paint and must not require one to start.
	var script *headlessRun
	if input.mode == conversationModeShot {
		size, err := parseHeadlessSize(config.ShotSize)
		if err != nil {
			return err
		}
		if script, err = loadHeadlessRun(config.ShotScript, size); err != nil {
			return err
		}
	}

	// The frontend is fixed for the life of the run; resolve it once so the
	// display contract and the REPL flavor cannot disagree.
	managedREPL := supportsManagedREPL() || script != nil
	if config.Setup && !(input.mode == conversationModeREPL && managedREPL) {
		return fmt.Errorf("--setup opens a form in the interactive TUI: run polly --setup in a terminal without a prompt or piped input")
	}
	// The first run opens the setup form whatever else was passed; the
	// form starts from the session's resolved settings. A shot run never takes
	// it implicitly: its input is a script, and a form would swallow it, so an
	// unconfigured run captures polly's defaults instead.
	config.Setup = config.Setup || (managedREPL && script == nil && firstRunPending())
	r.outputCapabilities = outputCapabilitiesForRun(input.mode, managedREPL)
	if script != nil {
		// The frame is laid out for the scripted size, so width-dependent
		// display decisions follow it instead of an absent terminal.
		r.outputCapabilities.columns = script.width
	}
	r.displayContract = displayContractFor(r.outputCapabilities)
	// The theme is applied here, once: it rewrites the process-global color
	// table every surface resolves through, and both frontends read it. A bad
	// theme is a notice, never a startup failure.
	r.applyStartupTheme(os.Stderr)

	// Set up signal handling
	signalCtx, cancelSignal := setupSignalHandling(ctx)
	defer cancelSignal()

	if (input.mode == conversationModeREPL || input.mode == conversationModeShot) && managedREPL {
		// Each session's lease context parents that session's turns and ends
		// the run when it is lost (see managedREPL.Run), so the run context
		// carries signals only. The REPL owns the opened session from here:
		// it closes every session it holds at exit, which also discards a
		// generated session that never ran a turn.
		first, err := r.openFirstWorkspace(ctx)
		if err != nil {
			return err
		}
		opener := &sessionOpener{
			prepare: r.prepare,
			open:    r.open,
			newName: func(ctx context.Context) (string, error) {
				return generateSessionName(ctx, r.sessionStore)
			},
		}
		if script != nil {
			return runHeadlessREPL(signalCtx, config, first, opener, script)
		}
		return runManagedREPL(signalCtx, config, first, opener)
	}

	state, err := r.openNew(ctx, r.contextID, r.autoContext)
	if err != nil {
		return err
	}

	defer func() {
		if err := state.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close conversation state: %w", err))
		}
	}()
	// The lease context is the direct parent so lease loss is observable
	// synchronously by the agent and TUI; signal/caller cancellation is
	// bridged into the same typed-cause context.
	ctx, cancelRun := turnContext(signalCtx, state)
	defer cancelRun()

	switch input.mode {
	case conversationModeOneShot:
		drainSandboxWarningsToWriter(os.Stderr, state)
		defer drainSandboxWarningsToWriter(os.Stderr, state)
		schema, err := loadSchemaFile(config.SchemaPath)
		if err != nil {
			return fmt.Errorf("failed to load schema: %w", err)
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

// openFirstWorkspace opens the run's first session for the managed REPL.
// A named context is resolved to its workspace: the open lands on the root
// of its ancestry, under the root's stored identity, and the named session
// is selected for inspection when it is a descendant. A root another polly
// holds is not opened; the result carries the store for a read-only tab. A
// generated or unnamed context opens directly.
func (r *commandRunner) openFirstWorkspace(ctx context.Context) (openResult, error) {
	res := openResult{name: r.contextID, store: r.sessionStore}
	reader, ok := r.sessionStore.(sessions.ViewStore)
	if ok && !r.autoContext && r.contextID != "" {
		entry, err := resolveWorkspaceEntry(ctx, reader, r.contextID)
		if err != nil && !errors.Is(err, sessions.ErrSessionNotFound) {
			return openResult{}, err
		}
		if entry != nil {
			res.workspaceEntry = entry
			res.name = entry.root.Metadata.Name
			if entry.root.InUse {
				return res, nil
			}
			ctx = context.WithValue(ctx, childViewIdentityKey{}, entry.root.ID)
		}
	}
	state, err := r.openNew(ctx, res.name, r.autoContext)
	if err != nil {
		return openResult{}, err
	}
	res.state = state
	return res, nil
}

func selectConversationMode(config *Config, stdinAvailable bool) (conversationMode, error) {
	// A shot script is the input, so it decides the mode before anything asks
	// about a prompt or a pipe: stdin may be the script itself.
	if config.ShotScript != "" {
		if config.PromptSet {
			return conversationModeShot, errors.New("--shot-script plays its own input: drop --prompt")
		}
		if err := validateREPLConfig(config); err != nil {
			return conversationModeShot, err
		}
		return conversationModeShot, nil
	}
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
	case conversationModeShot:
		// The script is the input; nothing is read from stdin here, since
		// stdin may be the script itself.
		return conversationInput{mode: conversationModeShot}, nil
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
