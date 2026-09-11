package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/subagent"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestPeersDiscoverRequestReplyAndPublishForReviewer(t *testing.T) {
	rosterReady, requestQueued := make(chan struct{}), make(chan struct{})
	call := func(id, name string, args any) messages.ChatMessageToolCall {
		return messages.ChatMessageToolCall{ID: id, Name: name, Arguments: tools.Result(args)}
	}
	batch := func(calls ...messages.ChatMessageToolCall) messages.ChatMessage {
		return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: calls}
	}
	lastTool := func(req *llm.CompletionRequest, name string) string {
		for i := len(req.Messages) - 1; i >= 0; i-- {
			m := req.Messages[i]
			if m.Role == messages.MessageRoleTool && m.ToolName == name {
				return m.Content
			}
		}
		return ""
	}
	wait := func(ctx context.Context, ready <-chan struct{}) {
		select {
		case <-ready:
		case <-ctx.Done():
		}
	}
	model := modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		role := ""
		for _, m := range req.Messages {
			if m.Role == messages.MessageRoleUser && !strings.HasPrefix(m.Content, "<peer_messages>") {
				role = m.Content
				break
			}
		}
		role = strings.SplitN(role, "\n\nCompletion: ", 2)[0]
		switch role {
		case "worker-a":
			wait(ctx, rosterReady)
			roster := lastTool(req, "list_agents")
			if roster == "" {
				return batch(call("roster", "list_agents", map[string]any{}))
			}
			if lastTool(req, "send_message") == "" {
				var list struct {
					Items []*Member `json:"items"`
				}
				if err := json.Unmarshal([]byte(roster), &list); err != nil {
					t.Error(err)
					return answer("bad roster")
				}
				for _, m := range list.Items {
					if m.Label == "worker-b" {
						return batch(call("ask", "send_message", map[string]any{"to": m.ID, "kind": "request", "text": "What is the shared answer?"}), call("wait", "swarm_wait", map[string]any{}))
					}
				}
				t.Error("teammate missing from roster")
			}
			found := false
			for _, m := range req.Messages {
				found = found || strings.Contains(m.Content, "shared answer is 42")
			}
			if !found {
				t.Error("peer reply not admitted before resumption")
			}
			return answer("private-worker-a-result")
		case "worker-b":
			wait(ctx, requestQueued)
			mail := lastTool(req, "read_messages")
			if mail == "" {
				return batch(call("mail", "read_messages", map[string]any{}))
			}
			if lastTool(req, "send_message") == "" {
				var page struct {
					Items []*Mail `json:"items"`
				}
				if err := json.Unmarshal([]byte(mail), &page); err != nil || len(page.Items) != 1 {
					t.Errorf("mail: %s %v", mail, err)
					return answer("bad mail")
				}
				return batch(call("reply", "send_message", map[string]any{"to": page.Items[0].From, "kind": "reply", "reply_to": page.Items[0].ID, "text": "shared answer is 42"}), call("publish", "swarm_publish", map[string]any{"text": "shared answer is 42", "sources": []string{"fixture/evidence"}}))
			}
			return answer("private-worker-b-result")
		case "reviewer":
			for _, m := range req.Messages {
				if strings.Contains(m.Content, "private-worker-") {
					t.Error("private peer result leaked to reviewer")
				}
			}
			publication := lastTool(req, "swarm_search")
			if publication == "" {
				return batch(call("find", "swarm_search", map[string]any{"query": "shared answer"}))
			}
			var pubs []*Publication
			if err := json.Unmarshal([]byte(publication), &pubs); err != nil || len(pubs) != 1 || pubs[0].Author == "" || pubs[0].Text != "shared answer is 42" {
				t.Errorf("publication: %s %v", publication, err)
			}
			return answer("review accepted shared evidence")
		}
		t.Errorf("unexpected assignment %q", role)
		return answer("unexpected")
	})
	r := runtimeTest(t, model, 2, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a, err := r.Spawn(ctx, subagent.Request{Task: "worker-a", Label: "worker-a", ReadOnly: true, Review: true, Background: true})
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Spawn(ctx, subagent.Request{Task: "worker-b", Label: "worker-b", ReadOnly: true, Review: true, Background: true})
	if err != nil {
		t.Fatal(err)
	}
	close(rosterReady)
	for {
		r.mu.Lock()
		notify := r.notify
		r.mu.Unlock()
		s, err := r.State(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(inbox(s, b.Session, false)) > 0 {
			break
		}
		select {
		case <-notify:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	close(requestQueued)
	for _, done := range []<-chan struct{}{a.Done, b.Done} {
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if _, err := r.Agent(ctx, "", AgentRequest{Task: "reviewer", ReadOnly: true, Review: true}); err != nil {
		t.Fatal(err)
	}
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Executions) != 3 || len(s.Publications) != 1 {
		t.Fatalf("extra execution or publication: %d %d", len(s.Executions), len(s.Publications))
	}
	requests, replies := 0, 0
	for _, mail := range s.Messages {
		if mail.Kind == "request" {
			requests++
			if mail.ReplyID == "" {
				t.Error("request has no reply")
			}
		}
		if mail.Kind == "reply" {
			replies++
			if !mail.Delivered {
				t.Error("reply not durably admitted")
			}
		}
	}
	if requests != 1 || replies != 1 {
		t.Fatalf("duplicate peer mail: %d requests %d replies", requests, replies)
	}
	for _, task := range s.Tasks {
		if err := r.Review(ctx, task.ID, task.Revision, true, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Settle(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestFailedWorkflowWithoutAgentsRequiresAcknowledgment(t *testing.T) {
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return answer("unused") }), 1, 1)
	ctx := context.Background()
	report, err := r.RunWorkflow(ctx, `polly.defineWorkflow({name:"failure",inputSchema:polly.schema.object({}),async run(){throw Error("verification failed");}});`, map[string]any{})
	if err == nil || report == nil {
		t.Fatalf("missing failure: %+v %v", report, err)
	}
	if err := assertSettleMatchesBlockers(t, r); err == nil || !strings.Contains(err.Error(), report.ID) {
		t.Fatalf("failure disappeared: %v", err)
	}
	if err := r.AcknowledgeWorkflow(ctx, report.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.Settle(ctx); err != nil {
		t.Fatal(err)
	}
	s, err := r.State(ctx)
	if err != nil || s.Workflows[report.ID].Status != "failed" || !s.Workflows[report.ID].Acknowledged || s.Workflows[report.ID].Run == "" {
		t.Fatalf("acknowledgment erased evidence: %+v %v", s, err)
	}
}

func TestMemberAssignmentWaitsForProjectionGate(t *testing.T) {
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
		t.Error("provider called after rejected gate")
		return answer("unexpected")
	}), 1, 1)
	r.config.Callbacks = func(context.Context, Member) *llm.AgentCallbacks {
		return &llm.AgentCallbacks{BeforeFirstRequest: func(llm.ProjectionStats) error { return errors.New("gate rejected") }}
	}
	ctx := context.Background()
	result, err := r.Agent(ctx, "", AgentRequest{Task: "unsaved assignment", ReadOnly: true, Review: true})
	if err == nil || !strings.Contains(err.Error(), "gate rejected") {
		t.Fatal(err)
	}
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	member := s.Members[result.Session]
	if member == nil {
		t.Fatal("lost failed member identity")
	}
	if s.Executions[member.Execution].InputSaved {
		t.Fatal("rejected assignment was persisted")
	}
	child, err := r.config.Store.Acquire(ctx, member.Name, sessions.AcquireOptions{ExpectedID: member.ID, ExistingOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	history, err := child.GetHistory(ctx)
	if err != nil || len(history) != 0 {
		t.Fatalf("gate left permanent unsendable history: %+v %v", history, err)
	}
}

func TestCheckpointDenialsPreserveAcceptedSequence(t *testing.T) {
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return answer("unused") }), 1, 1)
	cb := &llm.AgentCallbacks{}
	r.bindParent(cb, nil)
	ctx := context.Background()
	if err := cb.Checkpoint(ctx, llm.AgentCheckpoint{}); err != nil {
		t.Fatal(err)
	}
	denied := []messages.ChatMessage{{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "denied", Name: "work"}}}, {Role: messages.MessageRoleTool, ToolCallID: "denied", Content: llm.ToolDeniedContent}}
	if err := cb.Checkpoint(ctx, llm.AgentCheckpoint{Generated: denied}); err != nil {
		t.Fatal(err)
	}
	generated := append(denied, answer("retained output"))
	if err := cb.Checkpoint(ctx, llm.AgentCheckpoint{Generated: generated}); err != nil {
		t.Fatal(err)
	}
	history, err := r.config.Parent.GetHistory(ctx)
	if err != nil || len(history) != 1 || history[0].Content != "retained output" {
		t.Fatalf("denial polluted durable replay: %+v %v", history, err)
	}
}

func TestExhaustedBudgetRemainsBlockedAfterAcceptingFinishedWork(t *testing.T) {
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return answer("done") }), 1, 1)
	ctx := context.Background()
	result, err := r.Agent(ctx, "", AgentRequest{Task: "first", ReadOnly: true, Review: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Agent(ctx, "", AgentRequest{Task: "over budget", ReadOnly: true, Review: true}); !errors.Is(err, ErrBudget) {
		t.Fatal(err)
	}
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	task := s.Tasks[result.Task]
	if err := r.Review(ctx, task.ID, task.Revision, true, ""); err != nil {
		t.Fatal(err)
	}
	if err := assertSettleMatchesBlockers(t, r); !errors.Is(err, ErrBudget) {
		t.Fatalf("budget exhaustion disappeared: %v", err)
	}
	if err := r.Resume(ctx, "", 1); err != nil {
		t.Fatal(err)
	}
	if err := r.Settle(ctx); err != nil {
		t.Fatal(err)
	}
}
