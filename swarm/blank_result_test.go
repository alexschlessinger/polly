package swarm

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/subagent"
	"github.com/alexschlessinger/pollytool/workflow"
)

func finalNudges(history []messages.ChatMessage) int {
	n := 0
	for _, message := range history {
		if message.Content == memberFinalNudge {
			n++
		}
	}
	return n
}

func TestMemberBlankFinalAfterPublicationRetriesOnce(t *testing.T) {
	var calls atomic.Int32
	r := runtimeTest(t, modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		n := calls.Add(1)
		var message messages.ChatMessage
		switch n {
		case 1:
			message = iterationTool("publish", "swarm_publish", `{"text":"The complete digest is retained."}`)
		case 2:
			message = answer("")
		case 3:
			if finalNudges(req.Messages) != 1 {
				t.Error("missing single final-answer nudge")
			}
			message = answer("The digest is complete; see the published finding.")
		default:
			t.Error("unexpected extra model call")
		}
		message.SetTokenUsage(10, 2)
		return message
	}), 1, 1)
	result, err := r.Agent(context.Background(), "", AgentRequest{Label: "Test agent", Task: "prepare digest", ReadOnly: true})
	if err != nil || result.Value != "The digest is complete; see the published finding." {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	s, err := r.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || len(s.Executions) != 1 || len(s.Publications) != 1 || result.Usage.Samples != 3 || *result.Usage.InputTokens != 30 || *result.Usage.OutputTokens != 6 {
		t.Fatalf("accounting calls=%d state=%+v usage=%+v", calls.Load(), s, result.Usage)
	}
	for _, e := range s.Executions {
		if !e.EmptyFinalRetried || e.Iterations != 3 || e.Status != "completed" {
			t.Fatalf("execution=%+v", e)
		}
	}
	for _, run := range s.Runs {
		if run.Starts != 1 {
			t.Fatal("final-answer retry spent another execution")
		}
	}
	for _, publication := range s.Publications {
		if publication.Text != "The complete digest is retained." {
			t.Fatal("publication lost")
		}
	}
}

func TestMemberRepeatedBlankFinalIsIncomplete(t *testing.T) {
	for _, content := range []string{"", " \t\n"} {
		t.Run(map[bool]string{true: "empty", false: "whitespace"}[content == ""], func(t *testing.T) {
			var calls atomic.Int32
			r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
				calls.Add(1)
				message := answer(content)
				message.Reasoning = "I have completed the investigation."
				return message
			}), 1, 1)
			result, err := r.Agent(context.Background(), "", AgentRequest{Label: "Test agent", Task: "review", ReadOnly: true})
			var incomplete *EmptyResultError
			if !errors.Is(err, ErrEmptyResult) || !errors.As(err, &incomplete) || incomplete.Session != result.Session || calls.Load() != 2 {
				t.Fatalf("result=%+v err=%v calls=%d", result, err, calls.Load())
			}
			s, _ := r.State(context.Background())
			e := s.Executions[incomplete.Execution]
			if e == nil || e.Status != "failed" || !e.EmptyFinalRetried || e.Iterations != 2 || !strings.Contains(e.Error, "incomplete") || s.Tasks[result.Task].Status != "blocked" {
				t.Fatalf("execution=%+v task=%+v", e, s.Tasks[result.Task])
			}
			for _, mail := range s.Messages {
				if strings.Contains(mail.Text, " completed.") || !strings.Contains(mail.Text, "incomplete") {
					t.Fatalf("misleading completion mail: %s", mail.Text)
				}
			}
		})
	}
}

func TestMemberBlankFailureRetainsPublishedWork(t *testing.T) {
	var calls atomic.Int32
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
		if calls.Add(1) == 1 {
			return iterationTool("publish", "swarm_publish", `{"text":"The completed digest remains available."}`)
		}
		return answer("")
	}), 1, 1)
	_, err := r.Agent(context.Background(), "", AgentRequest{Label: "Test agent", Task: "digest", ReadOnly: true})
	if !errors.Is(err, ErrEmptyResult) || calls.Load() != 3 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
	s, _ := r.State(context.Background())
	if len(s.Publications) != 1 {
		t.Fatalf("publication count=%d", len(s.Publications))
	}
	for _, p := range s.Publications {
		if p.Text != "The completed digest remains available." {
			t.Fatal("completed work changed")
		}
	}
}

func TestMemberBlankFinalDoesNotAcceptMissingFailedOrDeniedTools(t *testing.T) {
	for _, kind := range []string{"missing response", "failed response", "denied response", "denied ordinary"} {
		t.Run(kind, func(t *testing.T) {
			var calls, results atomic.Int32
			r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
				calls.Add(1)
				var message messages.ChatMessage
				switch kind {
				case "missing response":
					return answer("")
				case "failed response":
					message = iterationTool("failed", "swarm_publish", `{`)
				case "denied response":
					message = iterationTool("denied", "swarm_publish", `{"text":"unpublished"}`)
				default:
					message = iterationTool("denied", "list_agents", `{}`)
				}
				message.Content = ""
				return message
			}), 1, 1)
			if kind != "denied ordinary" {
				r.UpdateDefaults(r.config.Request, llm.AgentConfig{MaxIterations: 5, ResponseTool: "swarm_publish"}, nil)
			}
			r.config.Callbacks = func(context.Context, Member) *llm.AgentCallbacks {
				return &llm.AgentCallbacks{
					ApproveToolCalls: func(calls []messages.ChatMessageToolCall) []bool {
						approved := make([]bool, len(calls))
						for i := range approved {
							approved[i] = !strings.HasPrefix(kind, "denied")
						}
						return approved
					},
					OnToolResult: func(messages.ChatMessageToolCall, messages.ChatMessage) { results.Add(1) },
				}
			}
			result, err := r.Agent(context.Background(), "", AgentRequest{Label: "Test agent", Task: "review", ReadOnly: true})
			wantCalls := int32(1)
			if kind == "missing response" {
				wantCalls = 3 // The existing response-tool reminder precedes our retry.
			}
			if !errors.Is(err, ErrEmptyResult) || calls.Load() != wantCalls {
				t.Fatalf("result=%+v err=%v calls=%d", result, err, calls.Load())
			}
			if kind == "failed response" && results.Load() != 1 {
				t.Fatalf("host result callbacks=%d", results.Load())
			}
			s, _ := r.State(context.Background())
			if s.Tasks[result.Task].Status != "blocked" || len(s.Publications) != 0 {
				t.Fatalf("false success: task=%+v publications=%+v", s.Tasks[result.Task], s.Publications)
			}
			if kind != "missing response" {
				for _, e := range s.Executions {
					if e.EmptyFinalRetried || strings.Contains(e.Error, "after one retry") {
						t.Fatalf("tool outcome retried: %+v", e)
					}
				}
			}
		})
	}
}

func TestMemberBlankFinalWorkflowErrorRetainsIdentityAndCause(t *testing.T) {
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return answer("") }), 1, 1)
	h := &workflowHost{runtime: r, controller: "workflow"}
	defer h.close()
	result, err := h.Call(context.Background(), workflow.Operation{Kind: "agent", Args: map[string]any{"label": "Test agent", "task": "review", "readOnly": true}})
	var workflowErr *workflow.Error
	var empty *EmptyResultError
	if !errors.As(err, &workflowErr) || !errors.As(err, &empty) || workflowErr.Code != "agent_failed" || !strings.Contains(workflowErr.Message, "incomplete") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if result.(AgentResult).Session != empty.Session || workflowErr.Session != empty.Session || empty.Execution == "" {
		t.Fatalf("identity lost: result=%+v workflow=%+v empty=%+v", result, workflowErr, empty)
	}
}

func TestMemberFinalRetryHonorsIterationGrant(t *testing.T) {
	var calls atomic.Int32
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
		if calls.Add(1) < 3 {
			return answer("")
		}
		return answer("complete")
	}), 1, 1)
	ctx := context.Background()
	result, err := r.Agent(ctx, "", AgentRequest{Label: "Test agent", Task: "review", ReadOnly: true, MaxIterations: 1})
	var exhausted *IterationLimitError
	if !errors.As(err, &exhausted) || calls.Load() != 1 {
		t.Fatalf("result=%+v err=%v calls=%d", result, err, calls.Load())
	}
	s := waitIterationMember(t, ctx, r, result.Session)
	if e := s.Executions[exhausted.Execution]; e.EmptyFinalRetried || e.Status != "paused" || e.Iterations != 1 {
		t.Fatalf("retry spent without allowance: %+v", e)
	}
	if err := r.ResumeWithIterations(ctx, result.Session, 2); err != nil {
		t.Fatal(err)
	}
	s = waitIterationMember(t, ctx, r, result.Session)
	if e := s.Executions[exhausted.Execution]; e.Status != "completed" || e.Iterations != 3 || !e.EmptyFinalRetried || calls.Load() != 3 {
		t.Fatalf("grant result: %+v calls=%d", e, calls.Load())
	}
}

func TestMemberFinalRetrySurvivesYield(t *testing.T) {
	var calls atomic.Int32
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
		if calls.Add(1) == 2 {
			return iterationTool("wait", "wait_agent", `{}`)
		}
		return answer("")
	}), 1, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := r.Spawn(ctx, subagent.Request{Label: "Test agent", Task: "review", ReadOnly: true})
	if err != nil || !result.Yielded {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if _, err := r.Send(ctx, r.ID, result.Session, "request", "", "finish the saved result"); err != nil {
		t.Fatal(err)
	}
	s := waitIterationMember(t, ctx, r, result.Session)
	e := s.Executions[s.Members[result.Session].Execution]
	if e.Status != "failed" || !strings.Contains(e.Error, "incomplete") || e.Iterations != 3 || !e.EmptyFinalRetried || calls.Load() != 3 {
		t.Fatalf("yield reset retry: %+v calls=%d", e, calls.Load())
	}
	view, err := r.config.Store.(sessions.ViewStore).ReadView(ctx, sessions.ViewTarget{ID: result.Session}, "")
	if err != nil || finalNudges(view.History) != 1 {
		t.Fatalf("nudge history=%+v err=%v", view, err)
	}
}

func TestMemberFinalRetrySurvivesCancellationAndDiskRestore(t *testing.T) {
	var calls atomic.Int32
	second := make(chan struct{})
	r := runtimeTest(t, modelFunc(func(ctx context.Context, _ *llm.CompletionRequest) messages.ChatMessage {
		if calls.Add(1) == 2 {
			close(second)
			<-ctx.Done()
		}
		return answer("")
	}), 1, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	spawned, err := r.Spawn(ctx, subagent.Request{Label: "Test agent", Task: "review", ReadOnly: true, Background: true})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-second:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := r.StopMember(ctx, spawned.Session); err != nil {
		t.Fatal(err)
	}
	s := waitIterationMember(t, ctx, r, spawned.Session)
	id := s.Members[spawned.Session].Execution
	if e := s.Executions[id]; e.Status != "paused" || !e.EmptyFinalRetried || e.Iterations != 2 {
		t.Fatalf("canceled retry: %+v", e)
	}
	db := filepath.Join(t.TempDir(), "swarm.db")
	if err := r.config.Store.(sessions.DurableStore).Promote(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.config.Parent.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.config.Store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := sessions.OpenStore(sessions.StoreConfig{Mode: sessions.ModeDisk, Path: db})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	parent, err := store.Acquire(ctx, "parent", sessions.AcquireOptions{ExistingOnly: true, ExpectedID: r.ID})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	cfg := r.config
	cfg.Store, cfg.Parent = store, parent
	restored, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if err := restored.Resume(ctx, spawned.Session, 0); err != nil {
		t.Fatal(err)
	}
	s = waitIterationMember(t, ctx, restored, spawned.Session)
	if e := s.Executions[id]; e.Status != "failed" || !strings.Contains(e.Error, "incomplete") || e.Iterations != 3 || !e.EmptyFinalRetried || calls.Load() != 3 {
		t.Fatalf("restore reset retry: %+v calls=%d", e, calls.Load())
	}
	view, err := store.ReadView(ctx, sessions.ViewTarget{ID: spawned.Session}, "")
	if err != nil || finalNudges(view.History) != 1 {
		t.Fatalf("nudge history=%+v err=%v", view, err)
	}
}

func TestMemberFinalPreservesHostContinuationAndCancellation(t *testing.T) {
	for _, stop := range []string{"continue", "error", "cancel"} {
		t.Run(stop, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var modelCalls, callbackCalls, usages atomic.Int32
			r := runtimeTest(t, modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
				if modelCalls.Add(1) == 2 && (finalNudges(req.Messages) != 0 || req.Messages[len(req.Messages)-1].Content != "host continuation") {
					t.Error("host continuation replaced")
				}
				if modelCalls.Load() == 3 && finalNudges(req.Messages) != 1 {
					t.Error("runtime retry missing after host continuation")
				}
				return answer("")
			}), 1, 1)
			hostError := errors.New("host stopped")
			r.config.Callbacks = func(context.Context, Member) *llm.AgentCallbacks {
				return &llm.AgentCallbacks{
					OnIterationUsage: func(int, int, int) { usages.Add(1) },
					ContinueAfterFinal: func(callbackCtx context.Context, _ *messages.ChatMessage) ([]messages.ChatMessage, error) {
						if callbackCalls.Add(1) > 1 {
							return nil, nil
						}
						switch stop {
						case "continue":
							return messages.User("host continuation"), nil
						case "error":
							return nil, hostError
						default:
							cancel()
							// Agent forwards caller cancellation to its detached
							// invocation from a separate goroutine. Exercise the
							// wrapper only once that cancellation has arrived.
							<-callbackCtx.Done()
							return nil, nil
						}
					},
				}
			}
			result, err := r.Agent(ctx, "", AgentRequest{Label: "Test agent", Task: "review", ReadOnly: true})
			wantErr, wantCalls := ErrEmptyResult, int32(3)
			if stop == "error" {
				wantErr, wantCalls = hostError, 1
			} else if stop == "cancel" {
				wantErr, wantCalls = context.Canceled, 1
				// The caller returns before the member finishes checkpointing.
				// Join it before asserting that cancellation issued no retry.
				waitCtx, stopWait := context.WithTimeout(context.Background(), 5*time.Second)
				defer stopWait()
				s := waitIterationMember(t, waitCtx, r, result.Session)
				for _, e := range s.Executions {
					if e.Status != "paused" || e.Iterations != 1 || e.EmptyFinalRetried {
						t.Fatalf("cancellation issued a retry: %+v", e)
					}
				}
			}
			if !errors.Is(err, wantErr) || modelCalls.Load() != wantCalls || callbackCalls.Load() != wantCalls || usages.Load() != wantCalls {
				t.Fatalf("err=%v model=%d callbacks=%d usage=%d", err, modelCalls.Load(), callbackCalls.Load(), usages.Load())
			}
		})
	}
}

// Direct completed events preserve current media parts, unlike text adapters.
type memberFinalModel func() messages.ChatMessage

func (f memberFinalModel) ChatCompletionStream(_ context.Context, _ *llm.CompletionRequest, _ llm.EventStreamProcessor) <-chan *messages.StreamEvent {
	message := f()
	events := make(chan *messages.StreamEvent, 1)
	events <- &messages.StreamEvent{Type: messages.EventTypeComplete, Message: &message}
	close(events)
	return events
}

func TestMemberFinalPreservesMediaStructuredAndResponseTools(t *testing.T) {
	for _, kind := range []string{"image", "image artifact", "text part", "structured", "invalid structured", "response tool"} {
		t.Run(kind, func(t *testing.T) {
			var calls atomic.Int32
			model := memberFinalModel(func() messages.ChatMessage {
				n := calls.Add(1)
				message := answer("")
				switch kind {
				case "image":
					message.Parts = []messages.ContentPart{{Type: "image_base64", ImageData: "aW1hZ2U=", MimeType: "image/png"}}
				case "image artifact":
					message.Parts = []messages.ContentPart{{Type: "image_artifact", Artifact: &artifacts.Ref{ID: "final-image", Kind: artifacts.KindImage, MIMEType: "image/png"}}}
				case "text part":
					message.Parts = []messages.ContentPart{{Type: "text", Text: "answer in parts"}}
				case "structured":
					message = completion(`{"ok":true}`)
				case "response tool":
					if n == 1 {
						message = iterationTool("publish", "swarm_publish", `{"text":"response tool answer"}`)
						message.Content = ""
					}
				}
				return message
			})
			r := runtimeTest(t, model, 1, 1)
			req := AgentRequest{Label: "Test agent", Task: "review", ReadOnly: true}
			if strings.Contains(kind, "structured") {
				req.Schema = map[string]any{"type": "object", "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}, "required": []string{"ok"}, "additionalProperties": false}
			}
			wantCalls := int32(1)
			if kind == "invalid structured" {
				wantCalls = 3
			}
			if kind == "response tool" {
				r.UpdateDefaults(r.config.Request, llm.AgentConfig{MaxIterations: 4, ResponseTool: "swarm_publish"}, nil)
			}
			result, err := r.Agent(context.Background(), "", req)
			if kind == "invalid structured" {
				if err == nil || errors.Is(err, ErrEmptyResult) {
					t.Fatalf("schema error replaced: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			s, _ := r.State(context.Background())
			if kind != "invalid structured" && kind != "structured" {
				value := result.Value.(string)
				if strings.TrimSpace(value) == "" || s.Tasks[result.Task].Result != value {
					t.Fatalf("final handoff lost: result=%+v task=%+v", result, s.Tasks[result.Task])
				}
				switch kind {
				case "text part":
					if value != "answer in parts" {
						t.Fatalf("text handoff=%q", value)
					}
				case "response tool":
					if !strings.Contains(value, "swarm_publish") || !strings.Contains(value, result.Session) {
						t.Fatalf("tool handoff=%q", value)
					}
				default:
					if !strings.Contains(value, result.Session) || !strings.Contains(value, "image/png") || strings.Contains(value, "aW1hZ2U=") || kind == "image artifact" && !strings.Contains(value, "final-image") {
						t.Fatalf("media handoff=%q", value)
					}
					view, err := r.config.Store.(sessions.ViewStore).ReadView(context.Background(), sessions.ViewTarget{ID: result.Session}, "")
					if err != nil || len(view.History[len(view.History)-1].Parts) != 1 {
						t.Fatalf("saved media lost: view=%+v err=%v", view, err)
					}
				}
				if len(s.Messages) != 1 {
					t.Fatalf("completion mail=%+v", s.Messages)
				}
				for _, mail := range s.Messages {
					if mail.Task != result.Task || !strings.Contains(admittedMailText(s, mail), value) {
						t.Fatalf("completion mail lost handoff: %+v", mail)
					}
				}
			}
			for _, e := range s.Executions {
				if e.EmptyFinalRetried || calls.Load() != wantCalls {
					t.Fatalf("specialized result retried: %+v calls=%d", e, calls.Load())
				}
			}
		})
	}
}

func TestMemberFinalIgnoresHistoricProjectionArtifacts(t *testing.T) {
	for _, kind := range []artifacts.Kind{artifacts.KindText, artifacts.KindImage, artifacts.KindBinary} {
		message := answer(" ")
		message.Parts = []messages.ContentPart{{Type: "artifact", Artifact: &artifacts.Ref{ID: "historic", Kind: kind}}}
		if meaningfulMemberFinal(&message) {
			t.Fatalf("historic %s ref accepted as a current final", kind)
		}
	}
}
