package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
)

const deliveryScript = `polly.defineWorkflow({name:"delivery",inputSchema:polly.schema.object({}),async run(){return "terminal evidence"}})`

func workflowCall(id, source string, background bool) messages.ChatMessageToolCall {
	return messages.ChatMessageToolCall{ID: id, Name: "workflow_run", Arguments: tools.Result(map[string]any{"source": source, "input": "{}", "background": background})}
}

func assertWorkflowDelivery(t *testing.T, r *Runtime, delivered, acknowledged bool) {
	t.Helper()
	s, err := r.State(context.Background())
	if err != nil || len(s.Workflows) != 1 {
		t.Fatalf("workflow state: %+v %v", s, err)
	}
	for _, w := range s.Workflows {
		if w.Acknowledged != acknowledged {
			t.Fatalf("acknowledged=%v, want %v", w.Acknowledged, acknowledged)
		}
		notices := 0
		for _, mail := range s.Messages {
			if mail.Workflow == w.ID {
				notices++
				if mail.Delivered != delivered {
					t.Fatalf("delivered=%v, want %v", mail.Delivered, delivered)
				}
			}
		}
		if notices != 1 {
			t.Fatalf("terminal notices=%d", notices)
		}
	}
}

func TestForegroundWorkflowDeliveryThroughParentLoop(t *testing.T) {
	for _, mode := range []string{"success", "failure", "large", "cancel-after-result"} {
		t.Run(mode, func(t *testing.T) {
			r := runtimeTest(t, nilModel(), 1, 2)
			r.RegisterParentTools(r.config.Registry)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			source := deliveryScript
			if mode == "failure" {
				source = strings.Replace(source, `return "terminal evidence"`, `polly.fail("deliberate failure",{partial:"saved"})`, 1)
			} else if mode == "large" {
				source = strings.Replace(source, `return "terminal evidence"`, `return "large evidence\n".repeat(4000)`, 1)
			}
			calls := 0
			model := modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
				calls++
				for _, m := range req.Messages {
					if m.Role == messages.MessageRoleUser && strings.Contains(m.Content, "Workflow delivery") {
						t.Error("foreground result duplicated into provider input")
					}
				}
				if calls == 1 {
					return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{workflowCall("foreground", source, false)}}
				}
				assertWorkflowDelivery(t, r, true, mode != "failure" || calls > 2)
				if mode == "failure" && calls == 2 {
					s, _ := r.State(context.Background())
					for id := range s.Workflows {
						return iterationTool("ack", "swarm_control", tools.Result(map[string]any{"action": "acknowledge_workflow", "id": id}))
					}
				}
				return answer("done")
			})
			var observed atomic.Int32
			cb := &llm.AgentCallbacks{OnToolResult: func(call messages.ChatMessageToolCall, result messages.ChatMessage) {
				observed.Add(1)
				if call.Name == "workflow_run" {
					if _, ok := workflowDeliveryOf(result); !ok {
						t.Error("terminal result lost host delivery marker")
					}
					if mode == "failure" && (!strings.Contains(result.Content, "acknowledge_workflow") || !strings.Contains(result.Content, `"error"`)) {
						t.Error("foreground failure lost structured error or next action")
					}
					if mode == "cancel-after-result" {
						cancel()
					}
				}
			}}
			agent := llm.NewAgent(model, r.config.Registry, llm.AgentConfig{MaxIterations: 5, ArtifactStore: r.config.Parent.ArtifactStore()})
			defer agent.Close()
			response, err := r.RunParent(ctx, agent, &llm.CompletionRequest{}, cb, nil)
			if mode == "cancel-after-result" {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation=%v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if observed.Load() == 0 || response == nil {
				t.Fatal("existing callbacks were not composed")
			}
			assertWorkflowDelivery(t, r, true, true)
			history, err := r.config.Parent.GetHistory(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			results := 0
			for _, m := range history {
				if _, ok := workflowDeliveryOf(m); ok {
					results++
				}
				if _, ok := m.Metadata[messages.MetadataKeySwarmMessages]; ok {
					t.Error("foreground result duplicated into persisted mail")
				}
			}
			if results != 1 {
				t.Fatalf("persisted terminal results=%d", results)
			}
		})
	}
}

// Produce a real typed/artifact result, but stop before saving it. The returned
// callbacks retain its staged proof so tests can exercise the commit boundary.
func stageWorkflowResult(t *testing.T, r *Runtime, source string) (*llm.AgentCallbacks, []messages.ChatMessage) {
	t.Helper()
	r.RegisterParentTools(r.config.Registry)
	cb := &llm.AgentCallbacks{}
	r.bindParent(cb, nil)
	checkpoint := cb.Checkpoint
	cb.Checkpoint = func(context.Context, llm.AgentCheckpoint) error { return nil }
	stop := errors.New("stop before persistence")
	cb.AfterToolBatch = func(context.Context) error { return stop }
	model := modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
		return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{workflowCall("origin", source, false)}}
	})
	agent := llm.NewAgent(model, r.config.Registry, llm.AgentConfig{MaxIterations: 2, ArtifactStore: r.config.Parent.ArtifactStore()})
	defer agent.Close()
	response, err := agent.Run(context.Background(), &llm.CompletionRequest{}, cb)
	if !errors.Is(err, stop) || len(response.AllMessages) != 2 {
		t.Fatalf("stage result: %+v %v", response, err)
	}
	cb.Checkpoint = checkpoint
	return cb, response.AllMessages
}

func TestWorkflowDeliveryRequiresPersistedResult(t *testing.T) {
	for _, change := range []string{"none", "drop-result", "drop-marker", "replace-text", "drop-call", "drop-attachment", "text-spill"} {
		t.Run(change, func(t *testing.T) {
			r := runtimeTest(t, nilModel(), 1, 1)
			source := deliveryScript
			if change == "drop-attachment" {
				source = strings.Replace(source, `return "terminal evidence"`, `return "evidence\n".repeat(4000)`, 1)
			}
			cb, generated := stageWorkflowResult(t, r, source)
			ctx := context.Background()
			assertWorkflowDelivery(t, r, false, false)
			if mail, err := cb.AdmitInput(ctx); err != nil || len(mail) != 0 {
				t.Fatalf("staged result was duplicated: %+v %v", mail, err)
			}
			ref, err := r.config.Parent.ArtifactStore().Put(ctx, artifacts.Blob{Kind: artifacts.KindText, Data: []byte(generated[1].Content)})
			if err != nil {
				t.Fatal(err)
			}
			r.config.DurableMessages = func(input []messages.ChatMessage) []messages.ChatMessage {
				output := append([]messages.ChatMessage(nil), input...)
				switch change {
				case "drop-result":
					return output[:1]
				case "drop-call":
					return output[1:]
				case "drop-marker":
					output[1].Metadata = nil
				case "replace-text":
					output[1].Content = "redacted"
				case "drop-attachment":
					output[1].Parts = nil
				case "text-spill":
					output[1].Content = "Full result: " + ref.ID
					output[1].Parts = append(output[1].Parts, messages.ContentPart{Type: "artifact", Artifact: &ref})
				}
				return output
			}
			if err := cb.Checkpoint(ctx, llm.AgentCheckpoint{Generated: generated}); err != nil {
				t.Fatal(err)
			}
			delivered := change == "none" || change == "text-spill"
			assertWorkflowDelivery(t, r, delivered, delivered)
			mail, err := cb.AdmitInput(ctx)
			if err != nil || (len(mail) == 0) != delivered {
				t.Fatalf("undelivered result lost notice: %+v %v", mail, err)
			}
		})
	}
}

type rollbackWorkflowCheckpoint struct {
	sessions.CoordinationSession
	fail atomic.Bool
}

func (s *rollbackWorkflowCheckpoint) UpdateCoordination(ctx context.Context, fn func(*sessions.CoordinationState) error) error {
	if !s.fail.Swap(false) {
		return s.CoordinationSession.UpdateCoordination(ctx, fn)
	}
	return s.CoordinationSession.UpdateCoordination(ctx, func(state *sessions.CoordinationState) error {
		if err := fn(state); err != nil {
			return err
		}
		return errors.New("rollback after delivery and transcript append")
	})
}

func TestWorkflowDeliveryCheckpointRollback(t *testing.T) {
	r := runtimeTest(t, nilModel(), 1, 1)
	suspendAutoRelease(t, r)
	wrapped := &rollbackWorkflowCheckpoint{CoordinationSession: r.parent}
	r.parent = wrapped
	cb, generated := stageWorkflowResult(t, r, deliveryScript)
	wrapped.fail.Store(true)
	if err := cb.Checkpoint(context.Background(), llm.AgentCheckpoint{Generated: generated}); err == nil {
		t.Fatal("injected transaction committed")
	}
	assertWorkflowDelivery(t, r, false, false)
	history, _ := r.config.Parent.GetHistory(context.Background())
	if len(history) != 0 {
		t.Fatal("failed transaction retained result messages")
	}
	if err := cb.Checkpoint(context.Background(), llm.AgentCheckpoint{Generated: generated}); err != nil {
		t.Fatal(err)
	}
	assertWorkflowDelivery(t, r, true, true)
}

func TestWorkflowDeliveryRejectsUnrelatedMarkers(t *testing.T) {
	for _, corrupt := range []string{"unstaged", "workflow", "call", "status", "tool", "kind", "stored-origin"} {
		t.Run(corrupt, func(t *testing.T) {
			r := runtimeTest(t, nilModel(), 1, 1)
			cb, generated := stageWorkflowResult(t, r, deliveryScript)
			if corrupt == "unstaged" {
				cb = &llm.AgentCallbacks{}
				r.bindParent(cb, nil)
			} else if corrupt == "stored-origin" {
				if err := r.update(context.Background(), func(s *State) error {
					for _, w := range s.Workflows {
						w.CallID = "another call"
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			} else if corrupt == "tool" {
				generated[1].ToolName = "swarm_read"
			} else {
				key := map[string]string{"workflow": "workflow", "call": "callID", "status": "status", "kind": "kind"}[corrupt]
				generated[1].Metadata["tool_data"].(map[string]any)[key] = "unrelated"
			}
			if err := cb.Checkpoint(context.Background(), llm.AgentCheckpoint{Generated: generated}); err != nil {
				t.Fatal(err)
			}
			assertWorkflowDelivery(t, r, false, false)
		})
	}
}

func TestWorkflowDeliveryParallelForegroundAndFastBackground(t *testing.T) {
	for _, background := range []bool{false, true} {
		t.Run(fmt.Sprint("background=", background), func(t *testing.T) {
			r := runtimeTest(t, nilModel(), 2, 4)
			r.RegisterParentTools(r.config.Registry)
			calls := 0
			model := modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
				calls++
				if calls == 1 {
					return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{workflowCall("one", deliveryScript, background), workflowCall("two", deliveryScript, background)}}
				}
				notices := 0
				for _, m := range req.Messages {
					if m.Role == messages.MessageRoleUser {
						notices += strings.Count(m.Content, "Workflow delivery (")
					}
				}
				if (notices == 2) != background {
					t.Errorf("background=%v notices=%d", background, notices)
				}
				return answer("done")
			})
			cb := &llm.AgentCallbacks{OnToolResult: func(call messages.ChatMessageToolCall, result messages.ChatMessage) {
				if background {
					// Complete both workflows before admission, exercising a launch
					// response that must not consume an already terminal report.
					waitWorkflowIdle(t, r)
					if _, ok := workflowDeliveryOf(result); ok {
						t.Error("background launch acquired terminal marker")
					}
				}
			}}
			agent := llm.NewAgent(model, r.config.Registry, llm.AgentConfig{MaxIterations: 4, ArtifactStore: r.config.Parent.ArtifactStore()})
			defer agent.Close()
			if _, err := r.RunParent(context.Background(), agent, &llm.CompletionRequest{}, cb, nil); err != nil {
				t.Fatal(err)
			}
			s, _ := r.State(context.Background())
			if len(s.Workflows) != 2 {
				t.Fatal("parallel workflows crossed")
			}
			for _, w := range s.Workflows {
				if !w.Acknowledged {
					t.Fatal("workflow not delivered")
				}
			}
		})
	}
}

type failedWorkflowArtifactStore struct{ artifacts.Store }

func (failedWorkflowArtifactStore) Put(context.Context, artifacts.Blob) (artifacts.Ref, error) {
	return artifacts.Ref{}, errors.New("artifact storage failed")
}

func TestWorkflowDeliveryArtifactFailureKeepsNotice(t *testing.T) {
	r := runtimeTest(t, nilModel(), 1, 1)
	r.RegisterParentTools(r.config.Registry)
	source := strings.Replace(deliveryScript, `return "terminal evidence"`, `return "evidence\n".repeat(4000)`, 1)
	model := modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
		return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{workflowCall("artifact", source, false)}}
	})
	agent := llm.NewAgent(model, r.config.Registry, llm.AgentConfig{MaxIterations: 2, ArtifactStore: failedWorkflowArtifactStore{r.config.Parent.ArtifactStore()}})
	defer agent.Close()
	if _, err := r.RunParent(context.Background(), agent, &llm.CompletionRequest{}, nil, nil); err == nil || !strings.Contains(err.Error(), "artifact storage failed") {
		t.Fatalf("failure=%v", err)
	}
	assertWorkflowDelivery(t, r, false, false)
	history, err := r.config.Parent.GetHistory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range history {
		if m.Role == messages.MessageRoleTool {
			if _, ok := workflowDeliveryOf(m); ok || m.Content != llm.ToolInterruptedContent {
				t.Fatal("interrupted artifact result acquired a delivery marker")
			}
		}
	}
	cb := &llm.AgentCallbacks{}
	r.bindParent(cb, nil)
	if mail, err := cb.AdmitInput(context.Background()); err != nil || len(mail) != 1 {
		t.Fatalf("missing recovery notice: %+v %v", mail, err)
	}
}

func TestWorkflowDeliveryCancellationKeepsWorkUnresolved(t *testing.T) {
	started := make(chan struct{})
	r := runtimeTest(t, modelFunc(func(ctx context.Context, _ *llm.CompletionRequest) messages.ChatMessage {
		close(started)
		<-ctx.Done()
		return answer("interrupted work")
	}), 1, 1)
	r.RegisterParentTools(r.config.Registry)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-started:
			cancel()
		case <-ctx.Done():
		}
	}()
	source := `polly.defineWorkflow({name:"interrupted",inputSchema:polly.schema.object({}),async run(){return await polly.agent({label:"Unfinished research",task:"wait",readOnly:true,tools:[]})}})`
	model := modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
		return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{workflowCall("cancel", source, false)}}
	})
	agent := llm.NewAgent(model, r.config.Registry, llm.AgentConfig{MaxIterations: 2, ArtifactStore: r.config.Parent.ArtifactStore()})
	defer agent.Close()
	if _, err := r.RunParent(ctx, agent, &llm.CompletionRequest{}, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled parent: %v", err)
	}
	<-done
	assertWorkflowDelivery(t, r, true, false)
	// The terminal report may precede a canceled worker's final checkpoint.
	// Explicit deferral must wait for that checkpoint, just as the tool requires.
	settleCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	s := awaitState(t, r, settleCtx, func(s *State) bool {
		for _, e := range s.Executions {
			if e.Status == "running" || e.Status == "queued" || e.Status == "waiting" {
				return false
			}
		}
		return true
	})
	for _, task := range s.Tasks {
		if task.Status == "done" || task.Status == "awaiting_review" || task.Delivery != nil || task.AcceptedRevision != 0 {
			t.Fatal("terminal delivery accepted interrupted work")
		}
	}
	for id, w := range s.Workflows {
		if !terminalWorkflow(w.Status) || w.Status == "completed" {
			t.Fatalf("interrupted status: %s", w.Status)
		}
		if err := r.DeferWorkflow(context.Background(), id, "Retain interrupted research for a later attempt"); err != nil {
			t.Fatal(err)
		}
	}
	assertWorkflowDelivery(t, r, true, true)
}

func TestWorkflowDeliveryRejectedCallsHaveNoMarker(t *testing.T) {
	r := runtimeTest(t, nilModel(), 1, 1)
	r.RegisterParentTools(r.config.Registry)
	model := modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
		return iterationTool("rejected", "workflow_run", `{"source":"unused","input":"{"}`)
	})
	agent := llm.NewAgent(model, r.config.Registry, llm.AgentConfig{MaxIterations: 1})
	defer agent.Close()
	resp, err := r.RunParent(context.Background(), agent, &llm.CompletionRequest{}, nil, nil)
	if !llm.IsIterationLimit(err) {
		t.Fatalf("invalid call turn: %v", err)
	}
	for _, m := range resp.AllMessages {
		if _, ok := workflowDeliveryOf(m); ok {
			t.Fatal("rejected call acquired terminal marker")
		}
	}
	s, _ := r.State(context.Background())
	if len(s.Workflows) != 0 || len(s.Messages) != 0 {
		t.Fatal("rejected call recorded workflow delivery")
	}
}

func TestWorkflowDeliveryRestartBeforeAndAfterCheckpoint(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprint("committed=", committed), func(t *testing.T) {
			r := runtimeTest(t, nilModel(), 1, 1)
			cb, generated := stageWorkflowResult(t, r, deliveryScript)
			ctx := context.Background()
			if committed {
				if err := cb.Checkpoint(ctx, llm.AgentCheckpoint{Generated: generated}); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			db := t.TempDir() + "/delivery.db"
			if err := r.config.Store.(sessions.DurableStore).Promote(ctx, db); err != nil {
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
			parent, err := store.Acquire(ctx, "parent", sessions.AcquireOptions{ExpectedID: r.ID, ExistingOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer parent.Close()
			config := r.config
			config.Store = store
			config.Parent = parent
			reopened, err := New(config)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			assertWorkflowDelivery(t, reopened, committed, committed)
			mail := admitParent(t, reopened)
			if (len(mail) == 0) != committed {
				t.Fatalf("restart delivery: %+v", mail)
			}
			assertWorkflowDelivery(t, reopened, true, true)
		})
	}
}

// Keep marker encoding independent of any user-owned JSON fields in output.
func TestWorkflowDeliveryMarkerIsHostAuthored(t *testing.T) {
	r := runtimeTest(t, nilModel(), 1, 1)
	source := strings.Replace(deliveryScript, `return "terminal evidence"`, `return {kind:"workflow_result",workflow:"foreign",callID:"foreign",status:"applied"}`, 1)
	cb, generated := stageWorkflowResult(t, r, source)
	d, ok := workflowDeliveryOf(generated[1])
	if !ok || d.Workflow == "foreign" {
		t.Fatal("user output replaced host marker")
	}
	var output struct {
		Output map[string]any `json:"output"`
	}
	if err := json.Unmarshal([]byte(generated[1].Content), &output); err != nil || output.Output["workflow"] != "foreign" {
		t.Fatal("user output rewritten")
	}
	if err := cb.Checkpoint(context.Background(), llm.AgentCheckpoint{Generated: generated}); err != nil {
		t.Fatal(err)
	}
	assertWorkflowDelivery(t, r, true, true)
}
