package main

import (
	"bytes"
	"context"
	"errors"
	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/sessions"
	"io"
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

// TestTurnProjectsOnceAndPersistsBeforeTheProviderCall pins the turn to a
// single projection: the user message becomes durable after the run's first
// projection and before the provider is called, and a selected image is read
// from the store once per provider request rather than once more for a
// separate validation pass.
func TestTurnProjectsOnceAndPersistsBeforeTheProviderCall(t *testing.T) {
	ctx := context.Background()
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "single-projection")
	registry := tools.NewToolRegistry(nil)
	reads := &countingArtifactStore{Store: session.ArtifactStore()}
	model := &persistedInputLLM{session: session}
	state := &conversationState{
		session: session, artifactStore: session.ArtifactStore(), toolRegistry: registry,
		agent:          llm.NewAgent(model, registry, llm.AgentConfig{ArtifactStore: reads}),
		settings:       Settings{Model: "test/model", MaxTokens: 128, MaxHistoryTokens: 8_000},
		contextWindows: map[string]int{"test/model": 0},
	}
	user := messages.ChatMessage{Role: messages.MessageRoleUser, Content: "inspect this", Parts: []messages.ContentPart{
		{Type: "image_base64", ImageData: portablePNGBase64Size(t, 400), MimeType: "image/png"},
	}}
	config := &Config{}
	var stdout, stderr bytes.Buffer
	ui := newLineTurnUI(config, nil)
	ui.writer, ui.errWriter = &stdout, &stderr
	code, err := executeTurnWithUserMessage(ctx, config, state, user, nil, nil, ui, false)
	if code != 0 || err != nil {
		t.Fatalf("turn failed: code=%d err=%v", code, err)
	}
	if !reflect.DeepEqual(model.persisted, []bool{true}) {
		t.Fatalf("user message durable at each provider call = %v, want [true]", model.persisted)
	}
	if reads.opens != 1 {
		t.Fatalf("the selected image was read %d times for one provider request", reads.opens)
	}
	if history := testSessionHistory(t, session); len(history) != 2 || history[0].Role != messages.MessageRoleUser || history[1].Content != "done" {
		t.Fatalf("history = %+v", history)
	}
}

// persistedInputLLM answers "done" and records, per call, whether the user
// message was already durable when the provider was called.
type persistedInputLLM struct {
	session   sessions.Session
	persisted []bool
}

func (l *persistedInputLLM) ChatCompletionStream(_ context.Context, _ *llm.CompletionRequest, processor llm.EventStreamProcessor) <-chan *messages.StreamEvent {
	history, err := l.session.GetHistory(context.Background())
	l.persisted = append(l.persisted, err == nil && len(history) > 0 && history[len(history)-1].Role == messages.MessageRoleUser)
	input := make(chan messages.ChatMessage, 1)
	input <- messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "done", StopReason: messages.StopReasonEndTurn}
	close(input)
	return processor.ProcessMessagesToEvents(input)
}

type countingArtifactStore struct {
	artifacts.Store
	opens int
}

func (s *countingArtifactStore) Open(ctx context.Context, id string) (io.ReadCloser, error) {
	s.opens++
	return s.Store.Open(ctx, id)
}
