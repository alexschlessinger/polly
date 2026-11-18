package main

import (
	"context"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
)

func TestAuthorshipGuidanceNamesCurrentModel(t *testing.T) {
	got := authorshipGuidance(&Settings{Model: "anthropic/claude-fable-5.2"})
	want := "Co-authored-by: polly/claude-fable-5.2 <polly@pollytool>"
	if !strings.Contains(got, want) {
		t.Fatalf("guidance = %q, want %q", got, want)
	}
	if strings.Count(got, "Co-authored-by:") != 1 {
		t.Fatalf("guidance carries more than one trailer: %q", got)
	}
}

func TestAuthorshipGuidanceUsesBareModelID(t *testing.T) {
	for _, tc := range []struct{ model, want string }{
		{"anthropic/claude-fable-5.2", "polly/claude-fable-5.2"},
		{"openrouter/anthropic/claude-fable-5.2", "polly/claude-fable-5.2"},
		{"claude-fable-5.2", "polly/claude-fable-5.2"},
	} {
		got := authorshipGuidance(&Settings{Model: tc.model})
		if !strings.Contains(got, "Co-authored-by: "+tc.want+" <polly@pollytool>") {
			t.Fatalf("model %q: guidance = %q", tc.model, got)
		}
	}
}

func TestAuthorshipGuidanceWithoutModel(t *testing.T) {
	for _, settings := range []*Settings{nil, {}, {Model: "   "}} {
		if got := authorshipGuidance(settings); got != "" {
			t.Fatalf("guidance without a model = %q", got)
		}
	}
}

// The model id is user-supplied state, so the trailer must stay one line and
// keep its address brackets intact.
func TestAuthorshipGuidanceKeepsTheTrailerOneLine(t *testing.T) {
	got := authorshipGuidance(&Settings{Model: "anthropic/claude\nfable<script>"})
	for _, line := range strings.Split(got, "\n") {
		if !strings.HasPrefix(line, "Co-authored-by:") {
			continue
		}
		if line != "Co-authored-by: polly/claudefablescript <polly@pollytool>" {
			t.Fatalf("trailer = %q", line)
		}
		return
	}
	t.Fatalf("guidance has no trailer: %q", got)
}

func TestComposeSessionContractsCarryCommitAttribution(t *testing.T) {
	t.Chdir(t.TempDir())
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "attribution")
	state := &conversationState{session: session}
	ctx := context.Background()
	compose := func(settings *Settings, msgs []messages.ChatMessage) string {
		t.Helper()
		got, warnings, err := composeSessionContracts(ctx, state, settings, msgs)
		if err != nil {
			t.Fatal(err)
		}
		if len(warnings) != 0 {
			t.Fatal(warnings)
		}
		if len(got) == 0 || got[0].Role != messages.MessageRoleSystem {
			t.Fatalf("composed messages lack a system contract: %v", got)
		}
		return got[0].Content
	}
	user := []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "hello"}}
	persona := func(prompt string) []messages.ChatMessage {
		return []messages.ChatMessage{
			{Role: messages.MessageRoleSystem, Content: prompt},
			{Role: messages.MessageRoleUser, Content: "hello"},
		}
	}
	settings := &Settings{Model: "anthropic/claude-fable-5.2"}
	trailer := "Co-authored-by: polly/claude-fable-5.2 <polly@pollytool>"
	content := compose(settings, user)
	if !strings.Contains(content, codingContract) || !strings.Contains(content, trailer) {
		t.Fatalf("default contract lacks the coding policy or attribution: %q", content)
	}

	// A custom persona replaces the coding policy but keeps attribution.
	settings.SystemPrompt = "Translate everything into French."
	content = compose(settings, persona(settings.SystemPrompt))
	if strings.Contains(content, codingContract) || !strings.Contains(content, settings.SystemPrompt) || !strings.Contains(content, trailer) {
		t.Fatalf("custom persona contract: %q", content)
	}

	// Without a model there is no attribution to state.
	content = compose(&Settings{SystemPrompt: settings.SystemPrompt}, persona(settings.SystemPrompt))
	if strings.Contains(content, "Co-authored-by:") {
		t.Fatalf("attribution without a model: %q", content)
	}
}
