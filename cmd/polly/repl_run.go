package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/sessions"
)

// Entry points: the managed REPL runner and the line-mode fallback.

// runManagedREPL runs the TUI on state and owns every session the REPL opens
// from there, the initial one included: all of them close when the loop
// exits, and a generated session that never ran a turn is discarded by that
// close.
func runManagedREPL(ctx context.Context, config *Config, state *conversationState, opener *sessionOpener) (retErr error) {
	repl := newManagedREPL(config, "-", 0, 0)
	repl.opener = opener
	defer func() {
		retErr = errors.Join(retErr, repl.closeTabs())
	}()
	if err := repl.addTab(state); err != nil {
		return errors.Join(err, state.Close())
	}
	return repl.Run(ctx, func(turnCtx context.Context, _ string, turnUI TurnUI) error {
		// The turn binds the session of the tab it started on: a tab shown
		// while this goroutine runs must not redirect its writes.
		tui, ok := turnUI.(*gotuiTurnUI)
		if !ok || tui.state == nil {
			return errors.New("turn started without a session")
		}
		// The exit code is a one-shot concern; the REPL already rendered
		// any warning.
		_, err := executeTurnWithUserMessage(turnCtx, config, tui.state, cloneChatMessage(tui.turn.userMessage), nil, nil, turnUI, tui.reuseUser)
		return err
	})
}

// newTabModel builds the screen model for a tab on state: the status row
// from the session's settings and the transcript from its history. It
// returns the session's name alongside. May run off the UI goroutine, before
// the tab is published.
func (r *managedREPL) newTabModel(state *conversationState) (string, *replModel, error) {
	return r.newTabModelContext(state.session.Context(), state)
}

func (r *managedREPL) newTabModelContext(ctx context.Context, state *conversationState) (string, *replModel, error) {
	name, err := state.session.GetName(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("read session name: %w", err)
	}
	history, err := state.session.GetHistory(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("read session history: %w", err)
	}
	settings := &state.settings
	m := newReplModel()
	// Off screen until shown; addTab shows a new tab at once.
	m.hidden = true
	m.status = newSessionStatus(settings, name, toolCount(state.effectiveTools()), skillCount(state.skillCatalog))
	if md, err := state.session.GetMetadata(ctx); err == nil && md != nil {
		m.status.parentName = md.Parent
	}
	m.quiet = r.config.Quiet
	m.artifactStore = state.artifactStore
	m.hydrateHistory(history, name)
	if m.hasAgentRows() && state.sessionStore != nil {
		if summaries, err := state.sessionStore.ListSummaries(ctx); err == nil {
			m.hydrateAgentSessions(name, summaries)
		} else {
			m.appendNoticeLine("agent sessions unavailable: " + err.Error())
		}
	}
	// Seed the bar without network traffic. This is explicitly approximate
	// until a provider reports the first real request usage.
	if total, totalErr := state.session.GetTotalTokens(ctx); totalErr == nil {
		limit := settings.MaxHistoryTokens
		if md, mdErr := state.session.GetMetadata(ctx); mdErr == nil && md != nil {
			if window := md.ContextWindows[settings.Model]; window > 0 {
				limit = llm.ClampContextBudget(limit, window, settings.MaxTokens)
			}
		}
		m.status.recordContextUsage(total, limit, total > 0)
	}
	return name, m, nil
}

func runFallbackREPL(ctx context.Context, config *Config, state *conversationState) error {
	reader := bufio.NewReader(os.Stdin)
	drainSandboxWarningsToWriter(os.Stderr, state)
	writeFallbackSandboxNotice(os.Stderr, config, state)
	commandCtx := newWriterReplCommandContext(config, state, os.Stderr)
	commandCtx.ctx = ctx
	return runREPLLoopWithCommands(ctx, reader, os.Stderr, commandCtx, func(prompt string) error {
		turnCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		// The exit code is a one-shot concern; the REPL already rendered
		// any warning.
		_, err := executeTurn(turnCtx, config, state, prompt, nil, reader, nil)
		drainSandboxWarningsToWriter(os.Stderr, state)
		// If the turn was cancelled but the parent context is still alive
		// (not a shutdown signal), treat it as a recoverable per-turn
		// cancellation and let the loop continue.
		if errors.Is(err, context.Canceled) && ctx.Err() == nil {
			return fmt.Errorf("cancelled")
		}
		return err
	})
}

func drainSandboxWarningsToWriter(w io.Writer, state *conversationState) {
	if w == nil || state == nil {
		return
	}
	for _, body := range state.drainSandboxWarnings() {
		fmt.Fprintln(w, "Warning: "+body)
	}
}

func writeFallbackSandboxNotice(w io.Writer, config *Config, state *conversationState) {
	if config == nil || config.Quiet {
		return
	}
	fmt.Fprintln(w, sandboxNoticeLine(config, state))
}

type fallbackLineResult struct {
	line string
	err  error
}

// readFallbackLine starts at most one blocked read while the fallback REPL is
// idle. Cancellation can therefore release the session/store promptly without
// racing a background stdin consumer against confirmation reads during turns.
func readFallbackLine(ctx context.Context, reader *bufio.Reader) (string, error) {
	result := make(chan fallbackLineResult, 1)
	go func() {
		line, err := readLine(reader)
		result <- fallbackLineResult{line: line, err: err}
	}()
	select {
	case <-ctx.Done():
		return "", context.Cause(ctx)
	case read := <-result:
		if err := context.Cause(ctx); err != nil {
			return "", err
		}
		return read.line, read.err
	}
}

func terminalSessionError(err error) bool {
	return errors.Is(err, sessions.ErrSessionLeaseLost) || errors.Is(err, sessions.ErrStoreClosed)
}

func runREPLLoopWithCommands(ctx context.Context, reader *bufio.Reader, promptWriter io.Writer, commandCtx *replCommandContext, runTurn func(string) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		if _, err := fmt.Fprint(promptWriter, "> "); err != nil {
			return err
		}
		line, err := readFallbackLine(ctx, reader)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "/") {
			handled, quit, err := defaultReplCommands.dispatch(trimmed, commandCtx)
			if err != nil {
				return err
			}
			if quit {
				return nil
			}
			if handled {
				continue
			}
			if _, err := fmt.Fprintln(promptWriter, defaultReplCommands.unknownCommandNotice(trimmed)); err != nil {
				return err
			}
			continue
		}
		if err := runTurn(line); err != nil {
			// Lease loss and store shutdown end the whole session. Other per-turn
			// failures remain recoverable and are rendered inline.
			if terminalSessionError(err) {
				return err
			}
			if cause := context.Cause(ctx); cause != nil {
				return cause
			}
			if errors.Is(err, context.Canceled) {
				return nil
			}
			if _, werr := fmt.Fprintf(promptWriter, "Error: %v\n", err); werr != nil {
				return werr
			}
		}
	}
}
