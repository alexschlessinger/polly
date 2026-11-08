package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

func TestContextCommandOpensPopupWithoutTranscriptOutput(t *testing.T) {
	for _, busy := range []bool{false, true} {
		t.Run(fmt.Sprintf("busy=%t", busy), func(t *testing.T) {
			store := testOpenMemoryStore(t, nil)
			session := testAcquireSession(t, store, "context-command")
			testAddMessage(t, session, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "question"})
			r := newManagedREPL(&Config{}, "context-command", 0, 0)
			r.state = &conversationState{session: session, settings: Settings{SystemPrompt: "test persona"}}
			m := r.model
			m.busy = busy
			m.appendLine("existing transcript")
			before := m.fullTranscript()
			m.ed.setText("/context")
			m.mu.Lock()
			quit := r.submitComposerLocked()
			m.mu.Unlock()
			if quit || m.modal == nil || m.modal.title != "Context" || m.modal.context == nil {
				t.Fatalf("/context did not open the live popup: quit=%t modal=%+v", quit, m.modal)
			}
			t.Cleanup(r.closeModal)
			if m.fullTranscript() != before || len(testSessionHistory(t, session)) != 1 {
				t.Fatal("opening Context added output or changed durable history")
			}
			if m.ed.text() != "" || m.busy != busy || len(m.queue) != 0 {
				t.Fatal("Context did not run immediately without affecting the active turn")
			}
		})
	}
}

func TestContextCommandTextAndLineFallback(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "context-report")
	testAddMessage(t, session, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "question"})
	state := &conversationState{
		session: session, displayContract: "terminal markdown",
		settings: Settings{Model: "openai/test", MaxHistoryTokens: 100_000, SystemPrompt: "test persona"},
	}
	for _, managed := range []bool{false, true} {
		for _, command := range []string{"/context", "/context text", "/context other", "/context text extra"} {
			t.Run(fmt.Sprintf("managed=%t/%s", managed, command), func(t *testing.T) {
				var output bytes.Buffer
				ctx := newWriterReplCommandContext(&Config{}, state, &output)
				opened := false
				if managed {
					ctx.openContextPopover = func() { opened = true }
				}
				ctx.lastTurn = func() string { return "1.0s · 100 in / 20 out · $0.01" }
				want := strings.Join(contextDetails(ctx), "\n") + "\n"
				handled, quit, err := defaultReplCommands.dispatch(command, ctx)
				if !handled || quit || err != nil {
					t.Fatalf("dispatch = %t, %t, %v", handled, quit, err)
				}
				wantPopup := managed && command == "/context"
				if opened != wantPopup {
					t.Fatalf("opened popup = %t, want %t", opened, wantPopup)
				}
				switch {
				case wantPopup:
					want = ""
				case command != "/context" && command != "/context text":
					want = "usage: /context [text]\n"
				default:
					for _, field := range []string{"context: context-report", "display: terminal markdown", "model: test", "last turn: 1.0s · 100 in / 20 out · $0.01", "requests carry:", "transcript: 1 msg", "tool calls: 0"} {
						if !strings.Contains(output.String(), field) {
							t.Fatalf("report missing %q: %s", field, output.String())
						}
					}
				}
				if output.String() != want {
					t.Fatalf("output = %q, want %q", output.String(), want)
				}
			})
		}
	}
}

type contextReportSession struct {
	sessions.Session
	reads int
}

func (s *contextReportSession) GetName(ctx context.Context) (string, error) {
	s.reads++
	return s.Session.GetName(ctx)
}

func (s *contextReportSession) GetMetadata(ctx context.Context) (*sessions.Metadata, error) {
	s.reads++
	return s.Session.GetMetadata(ctx)
}

func (s *contextReportSession) GetHistory(ctx context.Context) ([]messages.ChatMessage, error) {
	s.reads++
	return s.Session.GetHistory(ctx)
}

func TestContextTextCommandDefersStorageAndCapturesInputs(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	session := &contextReportSession{Session: testAcquireSession(t, store, "context-report")}
	testAddMessage(t, session, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "question"})
	state := &conversationState{session: session, displayContract: "original display", settings: Settings{Model: "provider/original", SystemPrompt: "persona"}}
	state.recordBudget(&state.settings, contextBudgetDetails{input: 1000, learned: true})
	config := &Config{SchemaPath: "schema.json"}
	var output bytes.Buffer
	ctx := newWriterReplCommandContext(config, state, &output)
	lastTurn := "original turn"
	ctx.lastTurn = func() string { return lastTurn }
	want := strings.Join(contextDetails(ctx), "\n")
	session.reads = 0
	var run func(context.Context) ([]string, error)
	ctx.storageWork = func(label string, work func(context.Context) ([]string, error)) error {
		if label != "context report" {
			t.Fatalf("work label = %q", label)
		}
		run = work
		return nil
	}
	handled, quit, err := defaultReplCommands.dispatch("/context text", ctx)
	if !handled || quit || err != nil || run == nil || session.reads != 0 || output.Len() != 0 {
		t.Fatalf("text report was not deferred: handled=%t quit=%t err=%v reads=%d", handled, quit, err, session.reads)
	}
	state.settings.Model, state.displayContract, config.SchemaPath, lastTurn = "provider/changed", "changed display", "", "changed turn"
	state.recordBudget(&state.settings, contextBudgetDetails{input: 9000})
	lines, err := run(t.Context())
	if err != nil || strings.Join(lines, "\n") != want || session.reads == 0 {
		t.Fatalf("deferred report changed inputs: got=%q want=%q err=%v", strings.Join(lines, "\n"), want, err)
	}
}

func TestContextCommandHelpAndCompletion(t *testing.T) {
	for _, input := range []string{"/context ", "/context t", "/context text"} {
		got, matches, ok := defaultReplCommands.complete(input, nil)
		if !ok || got != "/context text" || len(matches) != 1 || matches[0] != got {
			t.Fatalf("complete %q = %q, %v, %t", input, got, matches, ok)
		}
	}
	for _, input := range []string{"/context other", "/context text "} {
		if _, _, ok := defaultReplCommands.complete(input, nil); ok {
			t.Fatalf("offered unsupported completion for %q", input)
		}
	}
	help := strings.Join(defaultReplCommands.helpFor("/context"), "\n")
	for _, want := range []string{"usage: /context [text]", "popup", "full report", "line mode"} {
		if !strings.Contains(help, want) {
			t.Fatalf("help missing %q: %s", want, help)
		}
	}
	for _, command := range []string{"/context", "/context text"} {
		if !defaultReplCommands.busySafeCommand(command) {
			t.Fatalf("%s is not available during a turn", command)
		}
	}
}
