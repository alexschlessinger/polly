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
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

// Entry points: the managed REPL runner and the line-mode fallback.

func runManagedREPL(ctx context.Context, config *Config, state *conversationState, opener *sessionOpener, onStateChange func(*conversationState)) error {
	repl := newManagedREPL(config, "-", 0, 0)
	repl.opener = opener
	repl.onStateChange = onStateChange
	if err := repl.attachState(state); err != nil {
		return err
	}
	return repl.Run(ctx, func(turnCtx context.Context, prompt string, turnUI TurnUI) error {
		reuseUser := false
		userMsg := messages.ChatMessage{Role: messages.MessageRoleUser, Content: prompt}
		// The turn binds the session it started on: a switch that lands while
		// this goroutine runs must not redirect its writes.
		state := repl.state
		if tui, ok := turnUI.(*gotuiTurnUI); ok {
			reuseUser = tui.reuseUser
			userMsg = cloneChatMessage(tui.turn.userMessage)
			if tui.state != nil {
				state = tui.state
			}
		}
		// The exit code is a one-shot concern; the REPL already rendered
		// any warning.
		_, err := executeTurnWithUserMessage(turnCtx, config, state, userMsg, nil, nil, turnUI, reuseUser)
		return err
	})
}

// attachState makes state the REPL's live session. The screen model is
// rebuilt from the session's history; the screen-level facts the previous
// model carried (turn generation, image geometry, prompt history, focus) move
// across so a switch is indistinguishable from a fresh start on that session.
// Runs on the UI goroutine with no turn in flight.
func (r *managedREPL) attachState(state *conversationState) error {
	ctx := state.session.Context()
	name, err := state.session.GetName(ctx)
	if err != nil {
		return fmt.Errorf("read session name: %w", err)
	}
	history, err := state.session.GetHistory(ctx)
	if err != nil {
		return fmt.Errorf("read session history: %w", err)
	}
	config := r.config
	m := newReplModel()
	m.status = newSessionStatus(config, name, toolCount(state.toolRegistry), skillCount(state.skillCatalog))
	m.quiet = config.Quiet
	if old := r.model; old != nil {
		old.mu.Lock()
		// The generation keeps climbing so a detached turn from the previous
		// session can never match a turn started on this one.
		m.turnID = old.turnID
		m.nativeImages = old.nativeImages
		m.imageCellWidth, m.imageCellHeight = old.imageCellWidth, old.imageCellHeight
		m.hist.entries = old.hist.entries
		m.focusKnown, m.focused = old.focusKnown, old.focused
		old.mu.Unlock()
	}
	m.artifactStore = state.artifactStore
	m.hydrateHistory(history, name)
	// Seed the bar without network traffic. This is explicitly approximate
	// until a provider reports the first real request usage.
	if total, totalErr := state.session.GetTotalTokens(ctx); totalErr == nil {
		limit := config.MaxHistoryTokens
		if md, mdErr := state.session.GetMetadata(ctx); mdErr == nil && md != nil {
			if window := md.ContextWindows[config.Model]; window > 0 {
				limit = llm.ClampContextBudget(limit, window, config.MaxTokens)
			}
		}
		m.status.recordContextUsage(total, limit, total > 0)
	}
	r.model = m
	r.state = state
	if r.onStateChange != nil {
		r.onStateChange(state)
	}
	return nil
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
