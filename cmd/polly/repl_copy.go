package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	ui "github.com/metaspartan/gotui/v5"
)

func replCopyCommand(ctx *replCommandContext, args []string) replCommandResult {
	if len(args) != 1 {
		return replCommandResult{err: ctx.replyLine("usage: /copy")}
	}
	if ctx != nil && ctx.copyResponseAsync != nil {
		if err := ctx.copyResponseAsync(); err != nil {
			return replCommandResult{err: ctx.replyLine("copy failed: " + err.Error())}
		}
		return replCommandResult{}
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

// copyResponseSource contains only captured read handles and identity, never a
// mutable tab/state. Display-only tabs must not acquire a session to copy.
type copyResponseSource struct {
	store   sessions.SessionStore
	session sessions.Session
	target  sessions.ViewTarget
}

// copyTranscriptBlock bounds message selection, not answer size. Byte-limited
// pages can split a block; an oversized message remains intact. A long trailing
// run with no answer necessarily costs a proportional scan, not a fixed read.
const copyTranscriptBlock = 32

func (s copyResponseSource) read(ctx context.Context) (string, error) {
	headers, hasHeaders := s.store.(sessions.ViewHeaderStore)
	pages, hasPages := s.store.(sessions.TranscriptPageStore)
	if hasHeaders && hasPages {
		target := s.target
		if target.ID == "" && s.session != nil {
			// Older supplied sessions may not expose ViewIdentity. Resolve their
			// current name off-screen, then pin all pages to the header's ID.
			name, err := s.session.GetName(ctx)
			if err != nil {
				return "", fmt.Errorf("read conversation: %w", err)
			}
			target = sessions.ViewTarget{Name: name}
		}
		return readCopyResponsePages(ctx, headers, pages, target)
	}
	// Only missing capabilities take the legacy full-history path. Never hide
	// a native read/epoch/identity error by falling back to a different read.
	var history []messages.ChatMessage
	if s.session != nil {
		var err error
		history, err = s.session.GetHistory(ctx)
		if err != nil {
			return "", fmt.Errorf("read conversation: %w", err)
		}
	} else {
		store, ok := s.store.(sessions.ViewStore)
		if !ok {
			return "", fmt.Errorf("saved conversation is unavailable")
		}
		view, err := store.ReadView(ctx, s.target, "")
		if err != nil {
			return "", fmt.Errorf("read conversation: %w", err)
		}
		if view == nil || view.Unchanged || (s.target.ID != "" && view.ID != s.target.ID) {
			return "", fmt.Errorf("read conversation: invalid saved view")
		}
		history = view.History
	}
	return lastCompletedResponse(history)
}

func readCopyResponsePages(ctx context.Context, headers sessions.ViewHeaderStore, pages sessions.TranscriptPageStore, target sessions.ViewTarget) (string, error) {
	header, err := headers.ReadViewHeader(ctx, target, "")
	if err != nil {
		return "", fmt.Errorf("read conversation: %w", err)
	}
	if header == nil || header.ID == "" || header.Epoch == "" || header.Sequence < 0 || (target.ID != "" && target.ID != header.ID) {
		return "", fmt.Errorf("read conversation: invalid transcript header")
	}
	target = sessions.ViewTarget{ID: header.ID}
	for through := header.Sequence; through > 0; {
		first := max(int64(0), through-copyTranscriptBlock)
		var answer string
		for next := first; next < through; {
			if err := ctx.Err(); err != nil {
				return "", fmt.Errorf("read conversation: %w", err)
			}
			page, err := pages.ReadTranscriptPage(ctx, target, sessions.TranscriptQuery{
				Epoch: header.Epoch, After: next, Through: through,
				Limit: copyTranscriptBlock, MaxBytes: sessions.DefaultTranscriptPageBytes,
			})
			if err != nil {
				return "", fmt.Errorf("read conversation: %w", err)
			}
			if page.ID != header.ID || page.Epoch != header.Epoch {
				return "", fmt.Errorf("read conversation: %w", sessions.ErrStaleTranscriptEpoch)
			}
			if page.First != next || page.Next <= next || page.Next > through || page.Through != through || int64(len(page.Messages)) != page.Next-next || len(page.Messages) > copyTranscriptBlock {
				return "", fmt.Errorf("read conversation: invalid transcript page")
			}
			if text, err := lastCompletedResponse(page.Messages); err == nil {
				answer = text
			}
			next = page.Next
		}
		// Pages arrive oldest-first inside each backwards block. Only the
		// completed block tells us which answer is latest, even across budgets.
		if answer != "" {
			return answer, nil
		}
		through = first
	}
	return lastCompletedResponse(nil)
}

// Called on the UI loop with the model lock held. All storage I/O belongs to
// work; only the originating tab's UI task writes the clipboard and confirms it.
func (r *managedREPL) copyLastResponse() error {
	screen := ui.DefaultBackend.Screen
	if screen == nil {
		return fmt.Errorf("no terminal screen")
	}
	tab, m, state := r.visibleTab(), r.model, r.state
	if state == nil {
		return fmt.Errorf("no active session")
	}
	source := copyResponseSource{store: state.sessionStore, target: tab.viewTarget}
	id := tab.viewID()
	displayOnly := tab.childView != nil
	if displayOnly {
		if tab.childView.ID != "" {
			source.target = sessions.ViewTarget{ID: tab.childView.ID}
		}
	} else {
		if state.session == nil {
			return fmt.Errorf("no active session")
		}
		source.session = state.session
		if id != "" {
			source.target = sessions.ViewTarget{ID: id}
		}
	}
	parent := state.sessionContext()
	if !r.background(func() {
		ctx, cancel := context.WithCancel(parent)
		stop := context.AfterFunc(r.work.ctx, cancel)
		defer cancel()
		defer stop()
		text, err := source.read(ctx)
		r.postUI(r.work.ctx, func() {
			if r.work.ctx.Err() != nil || !slices.Contains(r.tabs, tab) || tab.model != m || tab.state != state || tab.viewID() != id || ui.DefaultBackend.Screen != screen {
				return
			}
			if displayOnly && (tab.childView == nil || tab.childView.ID != source.target.ID) {
				return
			}
			m.mu.Lock()
			defer m.mu.Unlock()
			if err == nil {
				err = parent.Err()
			}
			if err != nil {
				m.appendNoticeLine("copy failed: " + err.Error())
				return
			}
			// HasClipboard is advisory: many OSC 52 terminals do not report it.
			screen.SetClipboard([]byte(text))
			m.appendNoticeLine("copied last response to clipboard")
		})
	}) {
		return fmt.Errorf("workspace is closing")
	}
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
