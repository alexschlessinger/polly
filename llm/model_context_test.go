package llm

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestWithModelContextAddsBareModel(t *testing.T) {
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleSystem, Content: "persona"},
		{Role: messages.MessageRoleUser, Content: "work"},
	}
	got := WithModelContext(history, "openrouter/deepseek/deepseek-v4.1-flash-1017")
	system := got[0].GetContent()
	want := "<model_context>\nCurrent model: deepseek-v4.1-flash-1017\n</model_context>"
	if !strings.Contains(system, want) {
		t.Fatalf("system context = %q, want %q", system, want)
	}
	if strings.Count(system, "<model_context>") != 1 {
		t.Fatalf("model context was duplicated: %q", system)
	}
	if history[0].Content != "persona" {
		t.Fatalf("WithModelContext mutated its input: %q", history[0].Content)
	}
}

func TestWithModelContextWithoutModel(t *testing.T) {
	history := []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "work"}}
	if got := WithModelContext(history, ""); !reflect.DeepEqual(got, history) {
		t.Fatalf("model context without a model = %#v", got)
	}
}

func TestWithModelContextWithoutSystemMessage(t *testing.T) {
	got := WithModelContext([]messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "work"}}, "anthropic/claude-fable-5.2")
	if len(got) != 2 || got[0].Role != messages.MessageRoleSystem || !strings.Contains(got[0].Content, "Current model: claude-fable-5.2") {
		t.Fatalf("model context was not prepended: %#v", got)
	}
}

// The model id is user-supplied state, so the block must stay one line and
// keep its delimiters intact.
func TestWithModelContextKeepsTheBlockOneLine(t *testing.T) {
	got := WithModelContext([]messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "work"}}, "anthropic/claude\nfable<script>")
	if system := got[0].GetContent(); system != "<model_context>\nCurrent model: claudefablescript\n</model_context>" {
		t.Fatalf("model context = %q", system)
	}
}

func TestAgentInjectsModelContextWithoutPersistingIt(t *testing.T) {
	model := &promptCacheRecordingLLM{}
	agent := NewAgent(model, tools.NewToolRegistry(nil), AgentConfig{MaxIterations: 1})
	defer agent.Close()
	req := &CompletionRequest{
		Model:    "openrouter/deepseek/deepseek-v4.1-flash-1017",
		Messages: []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "work"}},
	}
	var checkpoints []messages.ChatMessage
	if _, err := agent.Run(context.Background(), req, &AgentCallbacks{Checkpoint: func(_ context.Context, checkpoint AgentCheckpoint) error {
		checkpoints = append(checkpoints, checkpoint.Generated...)
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	if len(model.requests) != 1 {
		t.Fatalf("model requests = %d", len(model.requests))
	}
	system := model.requests[0].Messages[0].GetContent()
	if !strings.Contains(system, "<model_context>\nCurrent model: deepseek-v4.1-flash-1017\n</model_context>") {
		t.Fatalf("request system context = %q", system)
	}
	if len(req.Messages) != 1 || strings.Contains(req.Messages[0].Content, "<model_context>") {
		t.Fatalf("the caller's history was mutated: %#v", req.Messages)
	}
	for _, msg := range checkpoints {
		if strings.Contains(msg.Content, "<model_context>") {
			t.Fatalf("model context reached durable history: %#v", msg)
		}
	}
}
