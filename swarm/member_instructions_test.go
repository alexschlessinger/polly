package swarm

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestMemberInstructionsReplaceStoreDefaultsAndPreserveContinuation(t *testing.T) {
	for _, factory := range []bool{false, true} {
		t.Run(map[bool]string{false: "parent prompt", true: "instruction factory"}[factory], func(t *testing.T) {
			ctx := context.Background()
			store, err := sessions.OpenStore(sessions.StoreConfig{Mode: sessions.ModeMemory, DefaultMetadata: &sessions.Metadata{SystemPrompt: "stale launch prompt"}})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			parent, err := store.Acquire(ctx, "parent", sessions.AcquireOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer parent.Close()
			metadata, err := parent.GetMetadata(ctx)
			if err != nil {
				t.Fatal(err)
			}
			metadata.SystemPrompt = "current parent prompt"
			if err := parent.SetMetadata(ctx, metadata); err != nil {
				t.Fatal(err)
			}
			registry := tools.NewToolRegistry(nil, tools.WithUnsafeNoSandbox())
			defer registry.Close()
			var prompts []string
			config := Config{Store: store, Parent: parent, Registry: registry,
				Client: modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
					for _, tool := range req.Tools {
						if tool.GetName() == "swarm_help" || tool.GetName() == "workflow_help" {
							t.Error("child acquired the parent guide tool")
						}
					}
					var system []string
					for _, message := range req.Messages {
						if message.Role == messages.MessageRoleSystem {
							system = append(system, message.Content)
						}
					}
					prompts = append(prompts, strings.Join(system, "\n"))
					return answer("saved finding")
				}), Root: t.TempDir(), Directory: filepath.Join(t.TempDir(), "members")}
			want := "current parent prompt"
			if factory {
				want = "current repository instructions"
				config.Instructions = func(*tools.ToolRegistry) string { return want }
			}
			r, err := New(config)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			r.RegisterParentTools(registry)
			first, err := r.Agent(ctx, "", AgentRequest{Label: "Test agent", Task: "investigate", ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			if len(prompts) != 1 || !strings.Contains(prompts[0], "Your identity is "+first.Session) || !strings.Contains(prompts[0], want) || strings.Contains(prompts[0], "stale launch prompt") {
				t.Fatalf("incorrect member instructions: %q", prompts)
			}
			if !strings.Contains(prompts[0], memberCoordinationGuidance) || strings.Contains(prompts[0], coordinationGuide) || strings.Contains(prompts[0], workflowGuide) {
				t.Fatalf("member lost its own guidance or inherited the parent playbook: %q", prompts)
			}
			metadata.SystemPrompt = "later parent prompt"
			if err := parent.SetMetadata(ctx, metadata); err != nil {
				t.Fatal(err)
			}
			r.UpdateDefaults(config.Request, config.Agent, func(*tools.ToolRegistry) string { return "later repository instructions" })
			if _, err := r.Agent(ctx, "", AgentRequest{Session: first.Session, Task: "continue investigation"}); err != nil {
				t.Fatal(err)
			}
			if len(prompts) != 2 || prompts[1] != prompts[0] {
				t.Fatalf("continuation replaced its existing instructions: %q", prompts)
			}
			s, err := r.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			member := s.Members[first.Session]
			child, err := store.Acquire(ctx, member.Name, sessions.AcquireOptions{ExpectedID: member.ID, ExistingOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer child.Close()
			history, err := child.GetHistory(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(history) != 5 || history[0].Content != strings.Split(prompts[0], "\n\n"+delegationGuidance)[0] || !strings.HasPrefix(history[1].Content, "investigate\n\nCompletion: ") || history[2].Content != "saved finding" || !strings.HasPrefix(history[3].Content, "continue investigation\n\nCompletion: ") {
				t.Fatalf("member history changed during continuation: %+v", history)
			}
		})
	}
}
