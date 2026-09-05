package main

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestTurnValidatesEffectiveContextBeforePersist(t *testing.T) {
	for _, tc := range []struct {
		name           string
		budget, window int
		maxTokens      int
		system, prompt string
		image          bool
	}{
		{name: "clamped model window", budget: 256_000, window: 20_000, maxTokens: 4_096, prompt: strings.Repeat("x", 64_000)},
		{name: "system prompt", budget: 2_000, system: strings.Repeat("s", 6_400), prompt: strings.Repeat("x", 1_200)},
		{name: "tool schemas", budget: 1_500, prompt: strings.Repeat("x", 4_000)},
		{name: "hydrated image", budget: 2_000, prompt: "inspect this", image: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := testOpenMemoryStore(t, nil)
			session := testAcquireSession(t, store, "pre-persist-context")
			if tc.system != "" {
				if err := session.AddMessage(ctx, messages.ChatMessage{Role: messages.MessageRoleSystem, Content: tc.system}); err != nil {
					t.Fatal(err)
				}
			}
			before := testSessionHistory(t, session)
			registry := tools.NewToolRegistry(nil)
			artifactStore := session.ArtifactStore()
			model := &captureCompletionLLM{response: messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "done", StopReason: messages.StopReasonEndTurn}}
			state := &conversationState{
				session: session, artifactStore: artifactStore, toolRegistry: registry,
				agent:          llm.NewAgent(model, registry, llm.AgentConfig{ArtifactStore: artifactStore}),
				settings:       Settings{Model: "test/model", MaxTokens: tc.maxTokens, MaxHistoryTokens: tc.budget},
				contextWindows: map[string]int{"test/model": tc.window},
			}
			user := messages.ChatMessage{Role: messages.MessageRoleUser, Content: tc.prompt}
			if tc.image {
				user.Parts = []messages.ContentPart{{Type: "image_base64", ImageData: portablePNGBase64Size(t, 400), MimeType: "image/png"}}
			}
			config := &Config{}
			var stdout, stderr bytes.Buffer
			ui := newLineTurnUI(config, nil)
			ui.writer, ui.errWriter = &stdout, &stderr
			code, err := executeTurnWithUserMessage(ctx, config, state, user, nil, nil, ui, false)
			var limitErr *llm.ContextLimitError
			if code != 1 || !errors.As(err, &limitErr) {
				t.Fatalf("expected context rejection: code=%d err=%v", code, err)
			}
			if !strings.Contains(err.Error(), "not added to the conversation") {
				t.Fatalf("rejection does not explain persistence: %v", err)
			}
			if len(model.request) != 0 {
				t.Fatal("provider was called for an unsendable prompt")
			}
			if after := testSessionHistory(t, session); !reflect.DeepEqual(after, before) {
				t.Fatalf("rejected input changed history: %d -> %d messages", len(before), len(after))
			}
		})
	}
}
