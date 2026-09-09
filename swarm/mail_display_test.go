package swarm

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestMailboxAdmissionRetainsSyntheticHistoryAndReceipt(t *testing.T) {
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return answer("unused") }), 1, 1)
	ctx := context.Background()
	mail, err := r.Send(ctx, r.ID, r.ID, "info", "", "first line\nsecond line")
	if err != nil {
		t.Fatal(err)
	}
	cb := &llm.AgentCallbacks{}
	r.BindParent(cb, nil)
	input, err := cb.AdmitInput(ctx)
	if err != nil || len(input) != 1 {
		t.Fatalf("admission: %+v %v", input, err)
	}
	if input[0].Metadata[messages.MetadataKeyAgentSynthetic] != true || input[0].Role != messages.MessageRoleUser || !strings.Contains(input[0].Content, "first line\nsecond line") {
		t.Errorf("mail is not model-visible synthetic input: %+v", input[0])
	}
	if err := cb.Checkpoint(ctx, llm.AgentCheckpoint{Generated: input}); err != nil {
		t.Fatal(err)
	}
	history, err := r.config.Parent.GetHistory(ctx)
	if err != nil || len(history) != 1 || history[0].Metadata[messages.MetadataKeyAgentSynthetic] != true || history[0].Content != input[0].Content {
		t.Fatalf("durable admission: %+v %v", history, err)
	}
	s, err := r.State(ctx)
	if err != nil || !s.Messages[mail.ID].Delivered {
		t.Fatalf("missing delivery receipt: %v", err)
	}
	if again, err := cb.AdmitInput(ctx); err != nil || len(again) != 0 {
		t.Fatalf("duplicate admission: %+v %v", again, err)
	}
}

func TestSettlementNudgeIsSynthetic(t *testing.T) {
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return answer("unused") }), 1, 1)
	ctx := context.Background()
	if _, err := r.CreateTask(ctx, "pending work", "review", nil, ""); err != nil {
		t.Fatal(err)
	}
	cb := &llm.AgentCallbacks{}
	r.BindParent(cb, nil)
	input, err := cb.ContinueAfterFinal(ctx, nil)
	if err != nil || len(input) != 1 || input[0].Metadata[messages.MetadataKeyAgentSynthetic] != true {
		t.Fatalf("settlement nudge is not synthetic: %+v %v", input, err)
	}
}

func TestCompletionMailReferencesPreservedResults(t *testing.T) {
	for _, structured := range []bool{false, true} {
		name, content := "text", "first line\nsecond line"
		var shape map[string]any
		if structured {
			name, content = "structured", `{"ok":true}`
			shape = map[string]any{"type": "object", "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}, "required": []string{"ok"}, "additionalProperties": false}
		}
		t.Run(name, func(t *testing.T) {
			r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return answer(content) }), 1, 1)
			ctx := context.Background()
			result, err := r.Agent(ctx, "", AgentRequest{Task: "report findings", ReadOnly: true, Tools: []string{}, Schema: shape})
			if err != nil {
				t.Fatal(err)
			}
			s, err := r.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var report string
			for _, mail := range s.Messages {
				if mail.From == result.Session {
					if !strings.Contains(mail.Text, result.Task) || !strings.Contains(mail.Text, "swarm_tasks") {
						t.Fatalf("missing retrieval reference: %s", mail.Text)
					}
					report = agentResultText(s.Tasks[result.Task].Result)
				}
			}
			if structured {
				var value map[string]any
				if err := json.Unmarshal([]byte(report), &value); err != nil || value["ok"] != true {
					t.Fatalf("structured report: %q %v", report, err)
				}
			} else if report != content {
				t.Fatalf("plain report was JSON-escaped: %q", report)
			}
		})
	}
}
