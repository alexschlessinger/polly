package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/subagent"
)

func iterationTool(id, name, args string) messages.ChatMessage {
	return messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "partial finding", StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: id, Name: name, Arguments: args}}}
}

func waitIterationMember(t *testing.T, ctx context.Context, r *Runtime, id string) *State {
	t.Helper()
	r.mu.Lock()
	i := r.active[id]
	r.mu.Unlock()
	if i != nil {
		select {
		case <-i.done:
		case <-ctx.Done():
			t.Fatal("member did not settle")
		}
	}
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestModelAndWorkflowDelegationInheritHostIterations(t *testing.T) {
	for _, path := range []string{"spawn", "workflow"} {
		t.Run(path, func(t *testing.T) {
			var calls atomic.Int32
			r := runtimeTest(t, modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
				n := calls.Add(1)
				if n < 10 {
					return iterationTool(fmt.Sprint(n), "swarm_publish", `{"text":"review evidence"}`)
				}
				return answer("review complete")
			}), 1, 1)
			r.UpdateDefaults(r.config.Request, llm.AgentConfig{MaxIterations: 12}, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if path == "spawn" {
				r.RegisterParentTools(r.config.Registry)
				tool, _, _ := r.config.Registry.GetIfAllowed(subagent.ToolName)
				if _, err := tool.Execute(ctx, map[string]any{"task": "review", "read_only": true}); err != nil {
					t.Fatal(err)
				}
			} else {
				_, err := r.RunWorkflow(ctx, `polly.defineWorkflow({name:"review", inputSchema:polly.schema.object({}), async run(){return await polly.agent({task:"review",readOnly:true})}})`, map[string]any{})
				if err != nil {
					t.Fatal(err)
				}
			}
			s, err := r.State(ctx)
			if err != nil || len(s.Executions) != 1 || calls.Load() != 10 {
				t.Fatalf("calls=%d state=%+v err=%v", calls.Load(), s, err)
			}
			for _, e := range s.Executions {
				if e.Request.MaxIterations != 12 || e.Status != "completed" || e.Iterations != 10 {
					t.Fatalf("host allowance not inherited: %+v", e)
				}
			}
		})
	}
}

func TestWorkflowCannotOverrideIterationsThroughOptionsOrScope(t *testing.T) {
	for _, key := range []string{"maxIterations", "MaxIterations", "maxiterations", "max_iterations"} {
		for _, scoped := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/scoped=%t", key, scoped), func(t *testing.T) {
				r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
					t.Error("invalid override launched a member")
					return answer("unexpected")
				}), 1, 1)
				body := fmt.Sprintf(`return await polly.agent({task:"review",readOnly:true,%s:8})`, key)
				if scoped {
					body = fmt.Sprintf(`return await polly.scope({%s:8}, async w => await w.agent({task:"review",readOnly:true}))`, key)
				}
				report, err := r.RunWorkflow(context.Background(), `polly.defineWorkflow({name:"review",inputSchema:polly.schema.object({}),async run(){`+body+`}})`, map[string]any{})
				if err == nil || report.Error == nil || report.Error.Code != "invalid_args" {
					t.Fatalf("override accepted: %+v %v", report, err)
				}
				s, err := r.State(context.Background())
				if err != nil || len(s.Members) != 0 || len(s.Executions) != 0 {
					t.Fatalf("invalid override spent execution budget: %+v %v", s, err)
				}
			})
		}
	}
}

func TestIterationGrantRestoresSameExecutionAndWorktreeFromDisk(t *testing.T) {
	skipIfWindows(t)
	var calls atomic.Int32
	model := modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		switch calls.Add(1) {
		case 1:
			msg := iterationTool("write", "write_file", `{"path":"candidate.txt","content":"saved edit"}`)
			msg.ToolCalls = append(msg.ToolCalls, messages.ChatMessageToolCall{ID: "publish", Name: "swarm_publish", Arguments: `{"text":"saved finding"}`})
			return msg
		case 2:
			var assignment, write, reply int
			for _, m := range req.Messages {
				if m.Content == "review and fix" {
					assignment++
				}
				if m.ToolCallID == "write" {
					write++
				}
				if strings.Contains(m.Content, "continue with the saved edit") {
					reply++
				}
			}
			if assignment != 1 || write != 1 || reply != 1 {
				t.Errorf("restored assignment=%d write receipt=%d peer reply=%d", assignment, write, reply)
			}
			return iterationTool("read", "read_file", `{"path":"candidate.txt"}`)
		default:
			if !strings.Contains(req.Messages[len(req.Messages)-1].Content, "saved edit") {
				t.Error("continuation did not read the original member worktree")
			}
			return answer("review complete")
		}
	})
	r := runtimeTest(t, model, 1, 1)
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-qm", "base"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = r.config.Root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	for _, name := range []string{"write_file", "read_file"} {
		if _, err := r.config.Registry.LoadToolAuto(name); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// A trusted Go host can still set a deliberate per-execution limit.
	result, err := r.Agent(ctx, "", AgentRequest{Task: "review and fix", MaxIterations: 1})
	var limit *IterationLimitError
	if !errors.As(err, &limit) || limit.Used != 1 || limit.Limit != 1 || result.Value != "partial finding" {
		t.Fatalf("not a recoverable partial result: %+v %v", result, err)
	}
	s := waitIterationMember(t, ctx, r, result.Session)
	e := s.Executions[limit.Execution]
	member := s.Members[result.Session]
	if e.Status != "paused" || e.StopReason != messages.StopReasonMaxIterations || MemberState(s, member).Lifecycle != LifecyclePaused || s.Tasks[result.Task].Status != "blocked" || len(s.Publications) != 1 {
		t.Fatalf("exhaustion state: %+v member=%+v", e, member)
	}
	root := s.Contexts[result.Context].Root
	if data, err := os.ReadFile(filepath.Join(root, "candidate.txt")); err != nil || string(data) != "saved edit" {
		t.Fatalf("lost partial edits: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(r.config.Root, "candidate.txt")); !os.IsNotExist(err) {
		t.Fatal("parent checkout changed")
	}
	if !errors.As(assertSettleMatchesBlockers(t, r), &limit) {
		t.Fatal("settle hid iteration exhaustion")
	}
	if _, err := r.Send(ctx, r.ID, result.Session, "request", "", "continue with the saved edit"); err != nil {
		t.Fatal(err)
	}
	r.wake(result.Session)
	if err := r.Resume(ctx, result.Session, 0); !errors.Is(err, llm.ErrMaxIterations) {
		t.Fatalf("plain resume reset the allowance: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatal("peer traffic or plain resume silently restarted a paused member")
	}
	// Saturating the separate start budget cannot be bypassed with an iteration grant.
	if _, err := r.Agent(ctx, "", AgentRequest{Task: "another"}); !errors.Is(err, ErrBudget) {
		t.Fatalf("budget: %v", err)
	}
	if err := r.ResumeWithIterations(ctx, result.Session, 2); !errors.Is(err, ErrBudget) {
		t.Fatalf("iteration grant bypassed paused run: %v", err)
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
	parent, err := store.Acquire(ctx, "parent", sessions.AcquireOptions{ExpectedID: r.ID, ExistingOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	config := r.config
	config.Store, config.Parent = store, parent
	config.Agent.MaxIterations = 100
	recovered, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if err := recovered.Resume(ctx, "", 1); err != nil {
		t.Fatal(err)
	}
	if err := recovered.Resume(ctx, result.Session, 0); !errors.Is(err, llm.ErrMaxIterations) {
		t.Fatalf("restart/default update reset the cap: %v", err)
	}
	if err := recovered.ResumeWithIterations(ctx, result.Session, 2); err != nil {
		t.Fatal(err)
	}
	s = waitIterationMember(t, ctx, recovered, result.Session)
	e = s.Executions[limit.Execution]
	if calls.Load() != 3 || len(s.Executions) != 1 || e.Iterations != 3 || e.Request.MaxIterations != 3 || e.Status != "completed" || e.StopReason != "" {
		t.Fatalf("grant restarted instead of continuing: calls=%d execution=%+v", calls.Load(), e)
	}
	if s.Members[result.Session].Context != result.Context || s.Members[result.Session].Task != result.Task || s.Tasks[result.Task].Status != "awaiting_review" || len(s.Publications) != 1 || s.Runs[e.Run].Starts != 1 {
		t.Fatalf("continuation lost identity, findings or budget: %+v", s)
	}
}

func TestWorkflowIterationPauseAllowsExplicitTakeover(t *testing.T) {
	var calls atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	r := runtimeTest(t, modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		if calls.Add(1) == 1 {
			return iterationTool("publish", "swarm_publish", `{"text":"partial evidence"}`)
		}
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return answer("finished review")
	}), 1, 3)
	r.UpdateDefaults(r.config.Request, llm.AgentConfig{MaxIterations: 1}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	report, err := r.RunWorkflow(ctx, `polly.defineWorkflow({name:"review",inputSchema:polly.schema.object({}),async run(){return await polly.agent({task:"review",readOnly:true})}})`, map[string]any{})
	if err == nil || report == nil || report.Error == nil || report.Error.Code != "iteration_limit" || report.Error.Session == "" {
		t.Fatalf("workflow lost the pause reason/session: %+v %v", report, err)
	}
	data, _ := json.Marshal(report.Error.Result)
	if !strings.Contains(string(data), "partial finding") || len(report.Steps) != 1 || report.Steps[0].Error.Code != "iteration_limit" {
		t.Fatalf("workflow lost partial result: %s %+v", data, report.Steps)
	}
	id := report.Error.Session
	s := waitIterationMember(t, ctx, r, id)
	executionID := s.Members[id].Execution
	r.RegisterParentTools(r.config.Registry)
	control, _, _ := r.config.Registry.GetIfAllowed("swarm_control")
	_, err = control.Execute(ctx, map[string]any{"action": "resume", "id": id, "additional_iterations": 100})
	if !llm.IsIterationLimit(err) {
		t.Fatalf("model resume replenished the allowance: %v", err)
	}
	if err := r.ResumeWithIterations(ctx, id, 1); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("takeover did not start")
	}
	if err := r.ResumeWithIterations(ctx, id, 1); err == nil {
		t.Fatal("concurrent grant accepted for running member")
	}
	close(release)
	s = waitIterationMember(t, ctx, r, id)
	e := s.Executions[executionID]
	if len(s.Executions) != 1 || e.Status != "completed" || e.Iterations != 2 || e.Request.MaxIterations != 2 || s.Members[id].Controller != "" || s.Runs[e.Run].Starts != 1 {
		t.Fatalf("takeover replaced member/execution or replayed JS: %+v", s)
	}
	// The workflow report remains a faithful record of the interrupted attempt.
	if s.Workflows[report.ID].Error.Code != "iteration_limit" {
		t.Fatal("takeover rewrote the original workflow report")
	}
}

func TestIterationGrantValidatesTaskAndAllowanceAtomically(t *testing.T) {
	for _, condition := range []string{"canceled", "reassigned", "dependency", "overflow", "active_workflow"} {
		t.Run(condition, func(t *testing.T) {
			r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
				return iterationTool("publish", "swarm_publish", `{"text":"partial evidence"}`)
			}), 1, 3)
			ctx := context.Background()
			result, err := r.Agent(ctx, "", AgentRequest{Task: "review", ReadOnly: true, MaxIterations: 1})
			if !llm.IsIterationLimit(err) {
				t.Fatal(err)
			}
			s := waitIterationMember(t, ctx, r, result.Session)
			eID := s.Members[result.Session].Execution
			task := s.Tasks[result.Task]
			additional := 2
			err = nil
			switch condition {
			case "canceled":
				err = r.CancelTask(ctx, task.ID)
			case "reassigned":
				err = r.UpdateTask(ctx, task.ID, task.Revision, "", nil)
			case "dependency":
				dependency, e := r.CreateTask(ctx, "dependency", "accepted", nil, "")
				if e != nil {
					t.Fatal(e)
				}
				err = r.UpdateTask(ctx, task.ID, task.Revision, task.Owner, []string{dependency.ID})
			case "overflow":
				additional = int(^uint(0) >> 1)
			case "active_workflow":
				err = r.update(ctx, func(s *State) error { s.Members[result.Session].Controller = "active-workflow"; return nil })
				r.mu.Lock()
				r.workflowCancels["active-workflow"] = func() {}
				r.mu.Unlock()
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := r.ResumeWithIterations(ctx, result.Session, additional); err == nil {
				t.Fatal("invalid grant was accepted")
			}
			s, err = r.State(ctx)
			if err != nil || s.Executions[eID].Request.MaxIterations != 1 || s.Executions[eID].Iterations != 1 || len(s.Executions) != 1 {
				t.Fatalf("failed grant mutated budget: %+v %v", s, err)
			}
		})
	}
}

func TestResumeReportsNewTurnBudgetRefusal(t *testing.T) {
	r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return answer("done") }), 1, 1)
	ctx := context.Background()
	result, err := r.Agent(ctx, "", AgentRequest{Task: "review", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Resume(ctx, result.Session, 0); !errors.Is(err, ErrBudget) {
		t.Fatalf("resume swallowed launch refusal: %v", err)
	}
	s, err := r.State(ctx)
	if err != nil || len(s.Executions) != 1 || MemberState(s, s.Members[result.Session]).Lifecycle != LifecycleIdle {
		t.Fatalf("refused resume changed the member: %+v %v", s, err)
	}
}

type grantCommitGate struct {
	sessions.CoordinationSession
	armed              atomic.Bool
	committed, release chan struct{}
}

func (g *grantCommitGate) UpdateCoordination(ctx context.Context, fn func(*sessions.CoordinationState) error) error {
	err := g.CoordinationSession.UpdateCoordination(ctx, fn)
	if err == nil && g.armed.CompareAndSwap(true, false) {
		close(g.committed)
		select {
		case <-g.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func TestIterationResumeSerializesAssignmentAndStop(t *testing.T) {
	for _, action := range []string{"reassign", "stop"} {
		t.Run(action, func(t *testing.T) {
			var calls atomic.Int32
			releaseModel := make(chan struct{})
			r := runtimeTest(t, modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
				if calls.Add(1) == 1 {
					return iterationTool("publish", "swarm_publish", `{"text":"saved"}`)
				}
				select {
				case <-releaseModel:
				case <-ctx.Done():
				}
				return answer("done")
			}), 1, 2)
			defer close(releaseModel)
			gate := &grantCommitGate{CoordinationSession: r.parent, committed: make(chan struct{}), release: make(chan struct{})}
			r.parent = gate
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := r.Agent(ctx, "", AgentRequest{Task: "review", ReadOnly: true, MaxIterations: 1})
			if !llm.IsIterationLimit(err) {
				t.Fatal(err)
			}
			gate.armed.Store(true)
			releaseCommit := sync.OnceFunc(func() { close(gate.release) })
			defer releaseCommit()
			resumed := make(chan error, 1)
			go func() { resumed <- r.ResumeWithIterations(ctx, result.Session, 1) }()
			select {
			case <-gate.committed:
			case <-ctx.Done():
				t.Fatal("grant did not commit")
			}
			s, err := r.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			task := s.Tasks[result.Task]
			competing := make(chan error, 1)
			go func() {
				if action == "stop" {
					competing <- r.StopMember(ctx, result.Session)
					return
				}
				competing <- r.UpdateTask(ctx, task.ID, task.Revision, "", nil)
			}()
			select {
			case err := <-competing:
				t.Fatalf("%s overtook a committed but unregistered resume: %v", action, err)
			case <-time.After(20 * time.Millisecond):
			}
			releaseCommit()
			if err := <-resumed; err != nil {
				t.Fatal(err)
			}
			err = <-competing
			if action == "reassign" && err == nil {
				t.Fatal("reassigned an active resumed member")
			}
			if action == "stop" && err != nil {
				t.Fatal(err)
			}
			s, err = r.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if action == "stop" && s.Members[result.Session].Control != MemberControlStopped {
				t.Fatal("resume overwrote the concurrent stop")
			}
			if s.Tasks[result.Task].Owner != result.Session {
				t.Fatal("resume lost task ownership")
			}
		})
	}
}
