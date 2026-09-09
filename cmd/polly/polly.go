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
	if err := command.Run(context.Background(), os.Args); err != nil {
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

func (r *commandRunner) runConversation() (retErr error) {
	ctx, config := r.ctx, r.config
	input, err := resolveConversationInput(config)
	if err != nil {
		return err
	}

	// The frontend is fixed for the life of the run; resolve it once so the
	// display contract and the REPL flavor cannot disagree.
	managedREPL := supportsManagedREPL()
	r.outputCapabilities = outputCapabilitiesForRun(input.mode, managedREPL)
	r.displayContract = displayContractFor(r.outputCapabilities)

	// Set up signal handling
	signalCtx, cancelSignal := setupSignalHandling(ctx)
	defer cancelSignal()

	if input.mode == conversationModeREPL && managedREPL {
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
