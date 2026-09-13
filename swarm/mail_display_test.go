package swarm

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/workflow"
)

func TestMailboxAdmissionRetainsSyntheticHistoryAndReceipt(t *testing.T) {
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return answer("unused") }), 1, 1)
	ctx := context.Background()
	mail, err := r.Send(ctx, r.ID, r.ID, "info", "", "first line\nsecond line")
	if err != nil {
		t.Fatal(err)
	}
	cb := &llm.AgentCallbacks{}
	r.bindParent(cb, nil)
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
	r.bindParent(cb, nil)
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
			result, err := r.Agent(ctx, "", AgentRequest{Label: "Test agent", Task: "report findings", ReadOnly: true, Tools: []string{}, Schema: shape})
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
					if mail.Task != result.Task || mail.Execution != result.Execution || mail.Revision != result.Revision || strings.Contains(mail.Text, "swarm_tasks") {
						t.Fatalf("incorrect result header: %+v", mail)
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

// A workflow reports for its agents once: no per-agent completion mail while
// it runs, and one notice committed with its terminal status.
func TestWorkflowPostsOneCompletionMail(t *testing.T) {
	for _, tc := range []struct {
		name, source, status string
		contains, excludes   []string
	}{
		{
			name:     "completed",
			source:   `polly.defineWorkflow({name:"two",inputSchema:polly.schema.object({}),async run(){await polly.agent({label:"Test agent",task:"one",readOnly:true});return await polly.agent({label:"Test agent",task:"two",readOnly:true});}})`,
			status:   "completed",
			contains: []string{"Workflow two (", "completed: 2 agents", "swarm_read({view: \"workflows\", id: \""},
			excludes: []string{"Reason:", "failed or paused", "defer: true"},
		},
		{
			name:     "failed",
			source:   `polly.defineWorkflow({name:"defer fixture",inputSchema:polly.schema.object({}),async run(){await polly.agent({label:"Test agent",task:"investigate",readOnly:true});polly.fail("verification incomplete")}})`,
			status:   "failed",
			contains: []string{"Workflow defer fixture (", "failed: 1 agents", "defer: true", "Reason: ", "verification incomplete"},
			excludes: []string{"failed or paused"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return answer("done") }), 2, 2)
			ctx := context.Background()
			report, err := r.RunWorkflow(ctx, tc.source, map[string]any{})
			if report == nil || report.Status != tc.status || (err == nil) != (tc.status == "completed") {
				t.Fatalf("workflow outcome: %+v %v", report, err)
			}
			s, err := r.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(s.Messages) != 1 {
				t.Fatalf("mail count = %d, want the workflow notice alone: %+v", len(s.Messages), s.Messages)
			}
			for _, mail := range s.Messages {
				if mail.To != r.ID || mail.From != report.ID || mail.Kind != "info" {
					t.Fatalf("notice envelope: %+v", mail)
				}
				for _, want := range append(tc.contains, report.ID) {
					if !strings.Contains(mail.Text, want) {
						t.Errorf("notice lacks %q: %s", want, mail.Text)
					}
				}
				for _, unwanted := range tc.excludes {
					if strings.Contains(mail.Text, unwanted) {
						t.Errorf("notice carries %q: %s", unwanted, mail.Text)
					}
				}
			}
		})
	}
}

// finish decides at finish time: a running workflow's agent stays silent, and
// the same member, restarted once the report is terminal, reports as usual.
func TestWorkflowAgentPostsNoCompletionMail(t *testing.T) {
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return answer("done") }), 1, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	setStatus := func(status string) {
		t.Helper()
		if err := r.update(ctx, func(s *State) error {
			s.Workflows["wf"] = &workflow.Report{ID: "wf", Status: status}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	setStatus("running")
	result, err := r.Agent(ctx, "wf", AgentRequest{Label: "Test agent", Task: "report", ReadOnly: true, Review: true, Tools: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 0 {
		t.Fatalf("workflow agent mailed the parent: %+v", s.Messages)
	}
	first := s.Members[result.Session].Execution
	setStatus("completed")
	if err := r.Resume(ctx, result.Session, 0); err != nil {
		t.Fatal(err)
	}
	s = awaitState(t, r, ctx, func(s *State) bool {
		m := s.Members[result.Session]
		e := s.Executions[m.Execution]
		return m.Execution != first && e != nil && e.Status == "completed"
	})
	var texts []string
	for _, mail := range s.Messages {
		if mail.From == result.Session {
			texts = append(texts, mail.Text)
		}
	}
	if len(texts) != 1 || !strings.Contains(texts[0], "completed") || !strings.Contains(texts[0], "swarm_review") {
		t.Fatalf("restarted member mail = %q", texts)
	}
}

// A member interrupted by a canceled workflow is an ordinary member again once
// the report is terminal: resuming it reports the outcome to the parent.
func TestInterruptedWorkflowMemberResumeReportsToParent(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	r := runtimeTest(t, modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		if calls.Add(1) == 1 {
			// The cancel must interrupt this call, never the resumed one.
			close(started)
			<-ctx.Done()
			return answer("interrupted")
		}
		return answer("resumed")
	}), 1, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, err := r.StartWorkflow(ctx, `polly.defineWorkflow({name:"cancel",inputSchema:polly.schema.object({}),async run(){await polly.agent({label:"Test agent",task:"inspect",readOnly:true});}})`, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("workflow agent never called the model")
	}
	if err := r.CancelWorkflow(id); err != nil {
		t.Fatal(err)
	}
	s := waitWorkflowIdle(t, r)
	if s.Workflows[id].Status != "interrupted" {
		t.Fatalf("workflow status = %s, want interrupted", s.Workflows[id].Status)
	}
	var member string
	for _, m := range s.Members {
		member = m.ID
	}
	if e := s.Executions[s.Members[member].Execution]; e == nil || e.Status != "paused" {
		t.Fatalf("interrupted execution = %+v", e)
	}
	notices, before := 0, 0
	for _, mail := range s.Messages {
		switch mail.From {
		case id:
			notices++
			if !strings.Contains(mail.Text, "interrupted") || !strings.Contains(mail.Text, "defer: true") {
				t.Fatalf("interrupted notice: %s", mail.Text)
			}
		case member:
			before++
		}
	}
	if notices != 1 {
		t.Fatalf("workflow notices = %d, want 1", notices)
	}
	if err := r.Resume(ctx, member, 0); err != nil {
		t.Fatal(err)
	}
	s = awaitState(t, r, ctx, func(s *State) bool {
		e := s.Executions[s.Members[member].Execution]
		return e != nil && e.Status == "completed"
	})
	after, completed := 0, false
	for _, mail := range s.Messages {
		if mail.From == member {
			after++
			completed = completed || strings.Contains(mail.Text, "completed")
		}
	}
	if after != before+1 || !completed {
		t.Fatalf("member mail after resume = %d (before %d), completed notice %v", after, before, completed)
	}
}
