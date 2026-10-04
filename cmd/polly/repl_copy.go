package main

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	ui "github.com/metaspartan/gotui/v5"
)

func replCopyCommand(ctx *replCommandContext, args []string) replCommandResult {
	if len(args) != 1 {
		return replCommandResult{err: ctx.replyLine("usage: /copy")}
	}
	if ctx == nil || ctx.copyResponse == nil {
		return replCommandResult{err: ctx.replyLine("clipboard copying requires a terminal")}
	}
	if err := ctx.copyResponse(); err != nil {
		return replCommandResult{err: ctx.replyLine("copy failed: " + err.Error())}
	}
	return replCommandResult{err: ctx.replyLine("copied last response to clipboard")}
}

// lastCompletedResponse uses durable answers, not rendered transcript rows or
// streaming buffers. GetContent excludes reasoning and phase-tagged commentary
// while preserving the answer's original Markdown and whitespace.
func lastCompletedResponse(history []messages.ChatMessage) (string, error) {
	for i := len(history) - 1; i >= 0; i-- {
		msg := history[i]
		if msg.Role != messages.MessageRoleAssistant || len(msg.ToolCalls) > 0 || msg.IsError() {
			continue
		}
		switch msg.StopReason {
		case "", messages.StopReasonEndTurn, messages.StopReasonMaxTokens:
			// Empty stop reasons are supported for older saved responses.
		default:
			continue
		}
		if text := msg.GetContent(); strings.TrimSpace(text) != "" {
			return text, nil
		}
	}
	return "", fmt.Errorf("no completed response to copy")
}

func (r *managedREPL) copyLastResponse() error {
	screen := ui.DefaultBackend.Screen
	if screen == nil {
		return fmt.Errorf("no terminal screen")
	}
	var history []messages.ChatMessage
	var err error
	if tab := r.visibleTab(); tab != nil && tab.childView != nil {
		// Display-only agent tabs have no session lease. Read their saved
		// history without promoting the tab to an executable runtime.
		store, ok := tab.state.sessionStore.(sessions.ViewStore)
		if !ok {
			return fmt.Errorf("saved conversation is unavailable")
		}
		view, readErr := store.ReadView(r.state.sessionContext(), tab.viewTarget, "")
		if readErr != nil {
			return fmt.Errorf("read conversation: %w", readErr)
		}
		history = view.History
	} else {
		if r.state == nil || r.state.session == nil {
			return fmt.Errorf("no active session")
		}
		history, err = r.state.session.GetHistory(r.state.sessionContext())
		if err != nil {
			return fmt.Errorf("read conversation: %w", err)
		}
	}
	text, err := lastCompletedResponse(history)
	if err != nil {
		return err
	}
	// HasClipboard is advisory: many OSC 52-capable terminals do not report
	// the capability. Let the terminal handle the write, including over SSH.
	screen.SetClipboard([]byte(text))
	return nil
}

func copyWriterResponse(ctx *replCommandContext, w io.Writer) error {
	file, ok := w.(*os.File)
	if !ok || !terminalFD(int(file.Fd())) || strings.EqualFold(os.Getenv("TERM"), "dumb") {
		return fmt.Errorf("clipboard copying requires a terminal with OSC 52 support")
	}
	if ctx.state == nil || ctx.state.session == nil {
		return fmt.Errorf("no active session")
	}
	history, err := ctx.state.session.GetHistory(ctx.operationContext())
	if err != nil {
		return fmt.Errorf("read conversation: %w", err)
	}
	text, err := lastCompletedResponse(history)
	if err != nil {
		return err
	}
	return writeClipboardText(w, text)
}

// writeClipboardText is the line frontend's equivalent of tcell.SetClipboard.
// The caller must ensure w is a terminal; redirected output gets no escapes.
func writeClipboardText(w io.Writer, text string) error {
	_, err := fmt.Fprintf(w, "\x1b]52;c;%s\x1b\\", base64.StdEncoding.EncodeToString([]byte(text)))
	return err
}
