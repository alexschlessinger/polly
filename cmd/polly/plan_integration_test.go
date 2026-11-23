package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
)

// Exercise the real command controller, agent loop, tool dispatcher and DB.
// A provider may return a call even if it was not in the advertised schemas.
func TestPlanReviewAndImplementAcrossAgentTurns(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	ctx := context.Background()
	const outcome = "Verify title matches"
	const draft = "# Search\n\n## Steps\n- " + outcome + "\n"
	phase := 0
	var path string
	model := &compactTestLLM{reply: func(_ context.Context, req *llm.CompletionRequest) (messages.ChatMessage, error) {
		phase++
		call := func(name string, args any) (messages.ChatMessage, error) {
			b, err := json.Marshal(args)
			return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{Name: name, ID: fmt.Sprintf("step-%d", phase), Arguments: string(b)}}}, err
		}
		switch phase {
		case 1:
			// Neither forged call may execute, even when the provider ignores
			// the draft turn's restricted tool schema.
			return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{
				{ID: "forged-write", Name: "project_writer", Arguments: `{}`},
				{ID: "forged-todo", Name: "update_todos", Arguments: `{"operation":"add","text":"Start implementing"}`},
			}}, nil
		case 2:
			// The first investigation in humble-gecko batched these three reads.
			return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{
				{ID: "inspect-cli", Name: "list_dir", Arguments: `{"path":".","limit":1}`},
				{ID: "inspect-tools", Name: "list_dir", Arguments: `{"path":"../../tools","limit":1}`},
				{ID: "inspect-draft", Name: "read_plan_draft", Arguments: `{}`},
			}}, nil
		case 3:
			text, err := os.ReadFile(path)
			if err != nil {
				return messages.ChatMessage{}, err
			}
			return call("write_plan_draft", map[string]string{"revision": sessions.PlanRevision(string(text)), "text": draft})
		case 5:
			return call("update_todos", map[string]any{"operation": "init_plan"})
		case 6:
			return call("update_todos", map[string]any{"operation": "status", "number": 1, "state": "active"})
		case 7:
			return call("project_writer", map[string]string{})
		case 8:
			return call("update_todos", map[string]any{"operation": "status", "number": 1, "state": "done"})
		}
		return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonEndTurn, Content: "Finished this turn."}, nil
	}}
	writes := 0
	reg := tools.NewToolRegistry([]tools.Tool{&tools.Func{Name: "project_writer", Run: func(context.Context, tools.Args) (string, error) { writes++; return "Verified a title-only match", nil }}})
	reg.Register(tools.NewListDirTool(reg))
	defer reg.Close()
	store := testOpenMemoryStore(t, nil)
	sess := testAcquireSession(t, store, "plan-flow")
	s := sess.(sessions.PlanSession)
	agent := llm.NewAgent(model, reg, llm.AgentConfig{MaxIterations: 12, ArtifactStore: sess.ArtifactStore()})
	defer agent.Close()
	state := &conversationState{session: sess, sessionStore: store, artifactStore: sess.ArtifactStore(), agent: agent, toolRegistry: reg, settings: Settings{Model: "test/model", MaxIterations: 12, MaxTokens: 512, SystemPrompt: "Test"}}
	var out bytes.Buffer
	cfg := &Config{}
	command := newWriterReplCommandContext(cfg, state, &out)
	command.startTurn = func(_ string, msg messages.ChatMessage) error {
		ui := newLineTurnUI(cfg, nil)
		ui.writer, ui.errWriter, ui.interactive = io.Discard, &out, true
		_, err := executeTurnWithUserMessage(ctx, cfg, state, msg, nil, nil, ui, false)
		return err
	}
	run := func(line string) {
		t.Helper()
		handled, quit, err := defaultReplCommands.dispatch(line, command)
		if !handled || quit || err != nil {
			t.Fatalf("%q: handled=%v quit=%v err=%v notices=%q", line, handled, quit, err, out.String())
		}
	}
	run("/plan")
	p, err := s.GetPlan(ctx)
	if err != nil || p == nil || p.Mode != sessions.PlanDraft {
		t.Fatalf("no draft: %+v err=%v notices=%q", p, err, out.String())
	}
	path = p.Path
	if err := command.startTurn("", messages.ChatMessage{Role: messages.MessageRoleUser, Content: "Plan this change"}); err != nil {
		t.Fatal(err)
	}
	if text, err := os.ReadFile(path); err != nil || string(text) != draft || writes != 0 || phase != 4 {
		t.Fatalf("planning: draft=%q err=%v project writes=%d phase=%d output=%q", text, err, writes, phase, out.String())
	}
	run("/plan review")
	run("/plan accept")
	p, err = s.GetPlan(ctx)
	if err != nil || p == nil || p.Mode != sessions.PlanAccepted || p.Snapshot != draft || len(p.Todos) != 1 || p.Todos[0].State != sessions.TodoDone || writes != 1 || !strings.Contains(out.String(), "TODO 1/1 done") {
		t.Fatalf("implementation: plan=%+v err=%v writes=%d phase=%d output=%q", p, err, writes, phase, out.String())
	}
	run("/plan accept")
	if phase != 9 || writes != 1 {
		t.Fatalf("repeat approval ran work: phase=%d writes=%d", phase, writes)
	}
}
