package main

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

func TestPersistUserMessageForTurnReusesMatchingFinalUser(t *testing.T) {
	session := newTurnPersistenceTestSession(t)
	userMsg := messages.ChatMessage{Role: messages.MessageRoleUser, Content: "try this"}
	if err := session.AddMessage(userMsg); err != nil {
		t.Fatalf("AddMessage() error = %v", err)
	}

	if err := persistUserMessageForTurn(session, userMsg, true); err != nil {
		t.Fatalf("persistUserMessageForTurn() error = %v", err)
	}
	if got := session.GetHistory(); !slices.EqualFunc(got, []messages.ChatMessage{userMsg}, equalTurnTestMessage) {
		t.Fatalf("matching retry should reuse final user message, history = %#v", got)
	}
}

func TestPersistUserMessageForTurnOnlyReusesOnExplicitMatchingRetry(t *testing.T) {
	userMsg := messages.ChatMessage{Role: messages.MessageRoleUser, Content: "try this"}

	tests := []struct {
		name     string
		reuse    bool
		history  []messages.ChatMessage
		wantSize int
	}{
		{
			name:     "ordinary turn persists duplicate text",
			history:  []messages.ChatMessage{userMsg},
			wantSize: 2,
		},
		{
			name:     "empty history",
			reuse:    true,
			wantSize: 1,
		},
		{
			name:     "different final user",
			reuse:    true,
			history:  []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "something else"}},
			wantSize: 2,
		},
		{
			name:  "matching user is not final",
			reuse: true,
			history: []messages.ChatMessage{
				userMsg,
				{Role: messages.MessageRoleAssistant, Content: "partial"},
			},
			wantSize: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := newTurnPersistenceTestSession(t)
			if err := session.AddMessages(tt.history); err != nil {
				t.Fatalf("AddMessages() error = %v", err)
			}
			if err := persistUserMessageForTurn(session, userMsg, tt.reuse); err != nil {
				t.Fatalf("persistUserMessageForTurn() error = %v", err)
			}
			got := session.GetHistory()
			if len(got) != tt.wantSize {
				t.Fatalf("history length = %d, want %d; history = %#v", len(got), tt.wantSize, got)
			}
			if last := got[len(got)-1]; !equalTurnTestMessage(last, userMsg) {
				t.Fatalf("last message = %#v, want persisted user %#v", last, userMsg)
			}
		})
	}
}

func TestPersistUserMessageForTurnComparesAttachedParts(t *testing.T) {
	userMsg := messages.ChatMessage{
		Role: messages.MessageRoleUser,
		Parts: []messages.ContentPart{
			{Type: "text", Text: "inspect this"},
			{Type: "file", FileName: "notes.txt", MimeType: "text/plain", Text: "alpha"},
		},
	}

	t.Run("same payload is reused", func(t *testing.T) {
		session := newTurnPersistenceTestSession(t)
		if err := session.AddMessage(userMsg); err != nil {
			t.Fatalf("AddMessage() error = %v", err)
		}
		if err := persistUserMessageForTurn(session, userMsg, true); err != nil {
			t.Fatalf("persistUserMessageForTurn() error = %v", err)
		}
		if got := len(session.GetHistory()); got != 1 {
			t.Fatalf("same attached payload should be reused, history length = %d", got)
		}
	})

	t.Run("changed payload is persisted", func(t *testing.T) {
		session := newTurnPersistenceTestSession(t)
		oldMsg := userMsg
		oldMsg.Parts = slices.Clone(userMsg.Parts)
		oldMsg.Parts[1].Text = "older contents"
		if err := session.AddMessage(oldMsg); err != nil {
			t.Fatalf("AddMessage() error = %v", err)
		}
		if err := persistUserMessageForTurn(session, userMsg, true); err != nil {
			t.Fatalf("persistUserMessageForTurn() error = %v", err)
		}
		if got := len(session.GetHistory()); got != 2 {
			t.Fatalf("changed attached payload must be persisted, history length = %d", got)
		}
	})
}

func TestDurableTurnMessagesMarksDeniedCompletionAndFiltersItFromModels(t *testing.T) {
	generated := []messages.ChatMessage{
		{
			Role: messages.MessageRoleAssistant,
			ToolCalls: []messages.ChatMessageToolCall{
				{ID: "1", Name: "bash", Arguments: `{}`},
			},
		},
		{
			Role:       messages.MessageRoleTool,
			Content:    llm.ToolDeniedContent,
			ToolCallID: "1",
			ToolName:   "bash",
		},
	}
	durable := durableTurnMessages(generated)
	if len(durable) != 1 || durable[0].Role != messages.MessageRoleInternal {
		t.Fatalf("durable denied turn = %#v, want one internal marker", durable)
	}
	if status, _ := durable[0].Metadata[messages.MetadataKeyTurnStatus].(string); status != messages.TurnStatusToolDenied {
		t.Fatalf("durable denied status = %q", status)
	}

	history := append([]messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "do it"}}, durable...)
	visible := modelVisibleHistory(history)
	if len(visible) != 1 || visible[0].Role != messages.MessageRoleUser {
		t.Fatalf("model-visible history leaked internal marker: %#v", visible)
	}
}

func TestDurableTurnMessagesKeepsProseAndDeniedOutcome(t *testing.T) {
	generated := []messages.ChatMessage{
		{
			Role:    messages.MessageRoleAssistant,
			Content: "I will inspect that.",
			ToolCalls: []messages.ChatMessageToolCall{
				{ID: "1", Name: "bash", Arguments: `{}`},
			},
		},
		{Role: messages.MessageRoleTool, Content: llm.ToolDeniedContent, ToolCallID: "1"},
	}
	durable := durableTurnMessages(generated)
	if len(durable) != 2 || durable[0].Content != "I will inspect that." || durable[1].Role != messages.MessageRoleInternal {
		t.Fatalf("prose + denial durable messages = %#v", durable)
	}

	m := newReplModel()
	m.hydrateHistory(append([]messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "inspect"}}, durable...), "ctx")
	plain := plainStyledText(strings.Join(m.flattenTranscript(), "\n"))
	if !strings.Contains(plain, "I will inspect that.") || !strings.Contains(plain, "tool request denied") || strings.Contains(plain, "incomplete") {
		t.Fatalf("prose + denial hydration = %q", plain)
	}
}

func TestLineTurnUIFinishOwnsOneTerminalNewline(t *testing.T) {
	tests := []struct {
		name    string
		content []string
		want    string
	}{
		{name: "unterminated content", content: []string{"answer"}, want: "answer\n"},
		{name: "terminated content", content: []string{"answer\n"}, want: "answer\n"},
		{name: "newline in separate chunk", content: []string{"answer", "\n"}, want: "answer\n"},
		{name: "no content", want: "\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			ui := newLineTurnUI(&Config{}, nil)
			ui.writer = &out
			for _, content := range tt.content {
				ui.AppendAssistantText(content)
			}
			ui.FinishTextTurn()
			if got := out.String(); got != tt.want {
				t.Fatalf("output = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLineTurnUIToolBoundaryPreservesSeparatorWithoutExtraFinalNewline(t *testing.T) {
	old := terminalFD
	terminalFD = func(int) bool { return true }
	t.Cleanup(func() { terminalFD = old })

	for _, before := range []string{"before", "before\n"} {
		t.Run("before "+before, func(t *testing.T) {
			var out bytes.Buffer
			var errOut bytes.Buffer
			ui := newLineTurnUI(&Config{}, nil)
			ui.writer = &out
			ui.errWriter = &errOut
			ui.AppendAssistantText(before)
			ui.AppendToolStart([]messages.ChatMessageToolCall{{Name: "read"}})
			ui.AppendAssistantText("after\n")
			ui.FinishTextTurn()

			if got, want := out.String(), "before\n\nafter\n"; got != want {
				t.Fatalf("output = %q, want %q", got, want)
			}
		})
	}
}

func TestLineTurnUIWarningAlreadyTerminatesOutput(t *testing.T) {
	var out bytes.Buffer
	ui := newLineTurnUI(&Config{}, nil)
	ui.writer = &out
	ui.AppendAssistantText("answer")
	ui.AppendWarning("truncated")
	ui.FinishTextTurn()

	if got, want := out.String(), "answer\nWarning: truncated\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func newTurnPersistenceTestSession(t *testing.T) sessions.Session {
	t.Helper()
	store := sessions.NewSyncMapSessionStore(nil)
	session, err := store.Get(t.Name())
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	t.Cleanup(session.Close)
	return session
}

func equalTurnTestMessage(a, b messages.ChatMessage) bool {
	return a.Role == b.Role && a.Content == b.Content && slices.Equal(a.Parts, b.Parts)
}
