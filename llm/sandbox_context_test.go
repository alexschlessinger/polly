package llm

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

type contextSandbox struct{}

func (contextSandbox) Wrap(*exec.Cmd) error { return nil }

func contextRegistry(cfg sandbox.Config) *tools.ToolRegistry {
	return tools.NewToolRegistry(nil, tools.WithNativeTools(), tools.WithSandboxFactory(func(sandbox.Config) (sandbox.Sandbox, error) {
		return contextSandbox{}, nil
	}, cfg))
}

func TestAgentRefreshesSandboxContextWithoutPersistingIt(t *testing.T) {
	for _, structured := range []bool{false, true} {
		t.Run(map[bool]string{false: "custom persona", true: "structured output"}[structured], func(t *testing.T) {
			path, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			registry := contextRegistry(sandbox.Config{})
			defer registry.Close()
			changes := 0
			registry.Register(&tools.Func{Name: "change_policy", Exclusive: true, Run: func(context.Context, tools.Args) (string, error) {
				changes++
				var err error
				switch changes {
				case 1:
					_, err = registry.SetSandboxLayer("profile", &tools.SandboxLayer{Config: sandbox.Config{WritablePaths: []string{path}, AllowNetwork: true}})
				case 2:
					_, err = registry.SetSandboxLayer("profile", nil)
				}
				return "policy updated", err
			}})
			model := &promptCacheRecordingLLM{}
			for _, id := range []string{"grant", "revoke", "unchanged"} {
				model.responses = append(model.responses, messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse,
					ToolCalls: []messages.ChatMessageToolCall{{ID: id, Name: "change_policy", Arguments: `{}`}}})
			}
			agent := NewAgent(model, registry, AgentConfig{MaxIterations: 4})
			defer agent.Close()
			req := &CompletionRequest{Messages: []messages.ChatMessage{
				{Role: messages.MessageRoleSystem, Parts: []messages.ContentPart{{Type: "text", Text: "custom persona"}}},
				{Role: messages.MessageRoleUser, Content: "work"},
			}}
			if structured {
				req.ResponseSchema = &Schema{Raw: map[string]any{"type": "object"}}
			}
			original := cloneMessages(req.Messages)
			var checkpoints []messages.ChatMessage
			result, err := agent.Run(context.Background(), req, &AgentCallbacks{Checkpoint: func(_ context.Context, checkpoint AgentCheckpoint) error {
				checkpoints = append(checkpoints, checkpoint.Generated...)
				return nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			if len(model.requests) != 4 || changes != 3 {
				t.Fatalf("got %d requests and %d changes", len(model.requests), changes)
			}
			for i, sent := range model.requests {
				system := sent.Messages[0].GetContent()
				if sent.Messages[0].Role != messages.MessageRoleSystem || !strings.Contains(system, "custom persona") || strings.Count(system, "<sandbox_context>") != 1 {
					t.Fatalf("request %d has incorrect system context: %s", i, system)
				}
				if got := strings.Contains(system, path); got != (i == 1) {
					t.Fatalf("request %d granted path present = %v: %s", i, got, system)
				}
				if got := strings.Contains(system, "Process network: allowed"); got != (i == 1) {
					t.Fatalf("request %d network permission is stale: %s", i, system)
				}
				if structured && sent.ResponseSchema == nil {
					t.Fatal("sandbox context removed structured output")
				}
			}
			if model.requests[0].PromptCacheKey == model.requests[1].PromptCacheKey || model.requests[0].PromptCacheKey != model.requests[2].PromptCacheKey || model.requests[2].PromptCacheKey != model.requests[3].PromptCacheKey {
				t.Fatal("prompt cache key does not follow current sandbox policy")
			}
			if !reflect.DeepEqual(req.Messages, original) {
				t.Fatal("caller history was modified")
			}
			for _, msg := range append(checkpoints, result.AllMessages...) {
				if strings.Contains(msg.GetContent(), "<sandbox_context>") {
					t.Fatal("runtime context leaked into durable output")
				}
			}
		})
	}
}

func TestAgentSandboxContextUsesBoundChildPolicyOnResume(t *testing.T) {
	parent := contextRegistry(sandbox.Config{WritablePaths: []string{t.TempDir()}, PrivateHome: true})
	defer parent.Close()
	ec, err := parent.ExecutionPolicy(t.TempDir(), tools.ExecutionGrant{ReadOnly: true, Scratch: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	child, _, err := parent.BindExecutionContext(ec, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	model := &promptCacheRecordingLLM{}
	agent := NewAgent(model, child, AgentConfig{})
	defer agent.Close()
	req := &CompletionRequest{Messages: []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "inspect"}}}
	first, err := agent.Run(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	// An existing child keeps its bound policy, even when the parent changes.
	if _, err := parent.SetSandboxLayer("parent", &tools.SandboxLayer{Config: sandbox.Config{AllowNetwork: true}}); err != nil {
		t.Fatal(err)
	}
	req.Messages = append(req.Messages, first.AllMessages...)
	req.Messages = append(req.Messages, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "continue"})
	if _, err := agent.Run(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	for _, request := range model.requests {
		context := request.Messages[0].GetContent()
		for _, want := range []string{ec.Root, ec.Scratch, "Working directory: writes denied", "Home: private", "Process network: blocked", "TMPDIR"} {
			if !strings.Contains(context, want) {
				t.Fatalf("child context lacks %q: %s", want, context)
			}
		}
	}
	if model.requests[0].Messages[0].Content != model.requests[1].Messages[0].Content {
		t.Fatal("resumed child inherited changed parent policy")
	}
}

func TestSandboxContextPreservesSystemParts(t *testing.T) {
	registry := contextRegistry(sandbox.Config{})
	defer registry.Close()
	history := []messages.ChatMessage{{Role: messages.MessageRoleSystem, Content: "persona", Parts: []messages.ContentPart{
		{Type: "text", Text: "additional instructions"}, {Type: "image_url"},
	}}}
	original := cloneMessages(history)
	got, err := WithSandboxContext(history, registry)
	if err != nil {
		t.Fatal(err)
	}
	if len(got[0].Parts) != 4 || got[0].Parts[0].Text != "persona" || !reflect.DeepEqual(got[0].Parts[1:3], original[0].Parts) || !reflect.DeepEqual(history, original) {
		t.Fatalf("system content changed: %+v", got)
	}
}

func TestSandboxContextCountsAgainstRequestBudget(t *testing.T) {
	registry := contextRegistry(sandbox.Config{})
	defer registry.Close()
	model := &promptCacheRecordingLLM{}
	agent := NewAgent(model, registry, AgentConfig{DisableTools: true})
	defer agent.Close()
	_, err := agent.Run(context.Background(), &CompletionRequest{MaxContextTokens: 64, Messages: messages.User("hello")}, nil)
	var limit *ContextLimitError
	if !errors.As(err, &limit) || len(model.requests) != 0 {
		t.Fatalf("sandbox context skipped budgeting: %v, %d requests", err, len(model.requests))
	}
}
