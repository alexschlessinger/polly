package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

// execParentTool runs one of the parent's registered tools by name.
func execParentTool(t *testing.T, r *Runtime, name string, args map[string]any) (string, error) {
	t.Helper()
	tool, _, _ := r.config.Registry.GetIfAllowed(name)
	if tool == nil {
		r.RegisterParentTools(r.config.Registry)
		if tool, _, _ = r.config.Registry.GetIfAllowed(name); tool == nil {
			t.Fatalf("%s is not registered", name)
		}
	}
	return tool.Execute(context.Background(), args)
}

func doneModel() llm.LLM {
	return modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return answer("done") })
}

// Accepting read-only research through swarm_review retires the member: its
// record reads idle · retired, its context and copy are gone, and settlement
// is unaffected.
func TestSwarmReviewAcceptRetiresResearch(t *testing.T) {
	for _, git := range []bool{false, true} {
		t.Run(map[bool]string{true: "checkout", false: "live"}[git], func(t *testing.T) {
			r := scratchRuntime(t, doneModel(), git)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			result, err := r.Agent(ctx, "", AgentRequest{Task: "look around", ReadOnly: true, Tools: []string{}})
			if err != nil {
				t.Fatal(err)
			}
			s, err := r.State(ctx)
			if err != nil {
				t.Fatal(err)
			}
			c := onlyContext(t, s)
			task := s.Tasks[result.Task]
			out, err := execParentTool(t, r, "swarm_review", map[string]any{"task": task.ID, "revision": task.Revision, "accept": true})
			if err != nil {
				t.Fatal(err)
			}
			var reviewed map[string]any
			if err := json.Unmarshal([]byte(out), &reviewed); err != nil {
				t.Fatal(err)
			}
			if reviewed["status"] != "done" || reviewed["retired"] != float64(1) || reviewed["retirement"] != nil {
				t.Fatalf("review result = %s", out)
			}
			if s, err = r.State(ctx); err != nil {
				t.Fatal(err)
			}
			m := s.Members[result.Session]
			if m.Control != MemberControlRetired || MemberState(s, m).Display != "idle · retired" {
				t.Fatalf("member after acceptance: control=%q display=%q", m.Control, MemberState(s, m).Display)
			}
			if s.Contexts[c.ID] != nil {
				t.Fatal("context record survived retirement")
			}
			if _, err := os.Stat(c.Scratch); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("scratch survived retirement: %v", err)
			}
			if git {
				if _, err := os.Stat(c.Root); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("checkout survived retirement: %v", err)
				}
			}
			if err := r.Settle(ctx); err != nil {
				t.Fatalf("settlement after retirement: %v", err)
			}
		})
	}
}

// Acknowledging a completed workflow accepts its consumed research and
// retires those members in the same call, reporting both counts.
func TestAcknowledgeRetiresConsumedResearch(t *testing.T) {
	r := runtimeTest(t, nilModel(), 1, 2)
	ctx := context.Background()
	report, err := r.RunWorkflow(ctx, `polly.defineWorkflow({name:"two",inputSchema:polly.schema.object({}),async run(){await polly.agent({task:"a",readOnly:true});return await polly.agent({task:"b",readOnly:true});}})`, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := execParentTool(t, r, "workflow_acknowledge", map[string]any{"id": report.ID})
	if err != nil || out != `"acknowledged; accepted 2 research results; retired 2 members"` {
		t.Fatalf("acknowledge = %s, %v", out, err)
	}
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Contexts) != 0 || len(s.Members) != 2 {
		t.Fatalf("contexts=%d members=%d after acknowledgment", len(s.Contexts), len(s.Members))
	}
	for _, m := range s.Members {
		if m.Control != MemberControlRetired || s.Tasks[m.Task].Status != "done" {
			t.Fatalf("member %s: control=%q task=%s", m.ID, m.Control, s.Tasks[m.Task].Status)
		}
	}
	if out, err := execParentTool(t, r, "workflow_acknowledge", map[string]any{"id": report.ID}); err != nil || out != `"acknowledged"` {
		t.Fatalf("second acknowledge = %s, %v", out, err)
	}
	if err := r.Settle(ctx); err != nil {
		t.Fatal(err)
	}
}

// A script that accepts research retires the member while the workflow still
// runs, and releasing the retired copy afterwards reports a release.
func TestInScriptReviewRetiresDuringWorkflow(t *testing.T) {
	type observed struct {
		contexts int
		retired  bool
	}
	seen := make(chan observed, 1)
	var calls atomic.Int32
	var r *Runtime
	r = runtimeTest(t, modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		if calls.Add(1) == 2 {
			s, err := r.State(ctx)
			if err != nil {
				t.Error(err)
			} else {
				retired := false
				for _, m := range s.Members {
					retired = retired || m.Control == MemberControlRetired
				}
				seen <- observed{len(s.Contexts), retired}
			}
		}
		return answer("done")
	}), 1, 2)
	ctx := context.Background()
	report, err := r.RunWorkflow(ctx, `polly.defineWorkflow({name:"consume",inputSchema:polly.schema.object({}),async run(){
const a=await polly.agent({task:"one",readOnly:true});
const t=await polly.tasks.read(a.task);
await polly.tasks.review({task:t.id,revision:t.revision,accept:true});
const released=await polly.release(a.context);
const b=await polly.agent({task:"two",readOnly:true});
return {released:released, first:a.context, second:b.context};
}})`, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case o := <-seen:
		if o.contexts != 1 || !o.retired {
			t.Fatalf("second agent ran with %d contexts, retired=%v; want the first retired already", o.contexts, o.retired)
		}
	default:
		t.Fatal("second agent never observed")
	}
	raw, _ := json.Marshal(report.Output)
	var output struct {
		Released struct {
			Released string `json:"released"`
			Retired  bool   `json:"retired"`
		} `json:"released"`
		First, Second string
	}
	if err := json.Unmarshal(raw, &output); err != nil {
		t.Fatal(err)
	}
	if output.Released.Released != output.First || !output.Released.Retired {
		t.Fatalf("release of a retired copy = %s", raw)
	}
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.Contexts[output.First] != nil || s.Contexts[output.Second] == nil {
		t.Fatalf("contexts after the workflow: first=%v second=%v", s.Contexts[output.First] != nil, s.Contexts[output.Second] != nil)
	}
	if out, err := execParentTool(t, r, "workflow_acknowledge", map[string]any{"id": report.ID}); err != nil || out != `"acknowledged; accepted 1 research result; retired 1 member"` {
		t.Fatalf("acknowledge = %s, %v", out, err)
	}
}

// A read-only copy that is not provably unchanged stays with its member;
// acceptance still stands and cleanup later names the edits.
func TestRetirementRetainsChangedReadOnlyCheckout(t *testing.T) {
	r := scratchRuntime(t, doneModel(), true)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := r.Agent(ctx, "", AgentRequest{Task: "look around", ReadOnly: true, Tools: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c := onlyContext(t, s)
	if err := os.WriteFile(filepath.Join(c.Root, "stray.txt"), []byte("edited outside the sandbox\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	task := s.Tasks[result.Task]
	out, err := execParentTool(t, r, "swarm_review", map[string]any{"task": task.ID, "revision": task.Revision, "accept": true})
	if err != nil {
		t.Fatal(err)
	}
	var reviewed map[string]any
	if err := json.Unmarshal([]byte(out), &reviewed); err != nil {
		t.Fatal(err)
	}
	if reviewed["status"] != "done" || reviewed["retired"] != nil || reviewed["retirement"] != nil {
		t.Fatalf("review result = %s", out)
	}
	if s, err = r.State(ctx); err != nil {
		t.Fatal(err)
	}
	if m := s.Members[result.Session]; m.Control != MemberControlEnabled || MemberState(s, m).Display != "idle · done" {
		t.Fatalf("member with a changed copy: control=%q display=%q", m.Control, MemberState(s, m).Display)
	}
	if kept := s.Contexts[c.ID]; kept == nil || kept.Retiring {
		t.Fatalf("changed copy not retained: %+v", kept)
	}
	if _, err := execParentTool(t, r, "swarm_control", map[string]any{"action": "cleanup", "id": c.ID}); err == nil || !strings.Contains(err.Error(), "unintegrated") {
		t.Fatalf("cleanup of the changed copy = %v", err)
	}
}

// A context left Retiring by an interrupted retirement is finished by the next
// sweep without re-verification.
func TestRetirementFinishesLeftoverRetiringContext(t *testing.T) {
	r := runtimeTest(t, doneModel(), 1, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := r.Agent(ctx, "", AgentRequest{Task: "look around", ReadOnly: true, Tools: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c := onlyContext(t, s)
	if err := r.update(ctx, func(s *State) error {
		s.Contexts[c.ID].Retiring = true
		s.Members[result.Session].Control = MemberControlRetired
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if n, err := r.RetireAcceptedResearch(ctx); err != nil || n != 1 {
		t.Fatalf("sweep = %d, %v; want the leftover finished", n, err)
	}
	if s, err = r.State(ctx); err != nil {
		t.Fatal(err)
	}
	if s.Contexts[c.ID] != nil {
		t.Fatal("leftover context survived the sweep")
	}
	if _, err := os.Stat(c.Scratch); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("leftover scratch survived the sweep: %v", err)
	}
	if n, err := r.RetireAcceptedResearch(ctx); err != nil || n != 0 {
		t.Fatalf("idle sweep = %d, %v", n, err)
	}
}

// countingSession counts committed coordination transactions. It wraps the
// root session before the runtime exists, so no runtime goroutine ever reads
// a parent field the test is swapping.
type countingSession struct {
	sessions.Session
	coord   sessions.CoordinationSession
	updates atomic.Int64
}

func newCountingSession(parent sessions.Session) *countingSession {
	return &countingSession{Session: parent, coord: parent.(sessions.CoordinationSession)}
}
func (s *countingSession) ViewID() string { return s.coord.ViewID() }
func (s *countingSession) ReadCoordination(ctx context.Context) (*sessions.CoordinationState, error) {
	return s.coord.ReadCoordination(ctx)
}
func (s *countingSession) OpenPublishedArtifact(ctx context.Context, id string) (io.ReadCloser, error) {
	return s.coord.OpenPublishedArtifact(ctx, id)
}
func (s *countingSession) UpdateCoordination(ctx context.Context, fn func(*sessions.CoordinationState) error) error {
	err := s.coord.UpdateCoordination(ctx, fn)
	if err == nil {
		s.updates.Add(1)
	}
	return err
}

// Retiring several researchers costs one accept, one mark and one delete
// transaction, however many members there are. The finishing workflow wakes
// its released members on their own goroutines, so the count is sampled
// around the acknowledge rather than installed after RunWorkflow returns.
func TestRetirementBatchesTransactions(t *testing.T) {
	var counter *countingSession
	r := runtimeTestWithParent(t, nilModel(), 1, 3, func(parent sessions.Session) sessions.Session {
		counter = newCountingSession(parent)
		return counter
	})
	ctx := context.Background()
	report, err := r.RunWorkflow(ctx, `polly.defineWorkflow({name:"three",inputSchema:polly.schema.object({}),async run(){for (const n of ["a","b","c"]) await polly.agent({task:n,readOnly:true});return true;}})`, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	before := counter.updates.Load()
	out, err := execParentTool(t, r, "workflow_acknowledge", map[string]any{"id": report.ID})
	if err != nil || out != `"acknowledged; accepted 3 research results; retired 3 members"` {
		t.Fatalf("acknowledge = %s, %v", out, err)
	}
	if n := counter.updates.Load() - before; n != 3 {
		t.Fatalf("acknowledging 3 researchers used %d transactions, want 3", n)
	}
}

// A researcher that still owns an unreviewed submission is not retired when
// another of its tasks is accepted; it retires once every task is settled.
func TestRetirementWaitsForEveryTaskOfTheMember(t *testing.T) {
	r := scratchRuntime(t, doneModel(), false)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	first, err := r.Agent(ctx, "", AgentRequest{Task: "look at a", ReadOnly: true, Tools: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.CreateTask(ctx, "look at b", "none", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Agent(ctx, "", AgentRequest{Session: first.Session, TaskID: second.ID, Task: "look at b"}); err != nil {
		t.Fatal(err)
	}
	accept := func(id string) map[string]any {
		t.Helper()
		s, err := r.State(ctx)
		if err != nil {
			t.Fatal(err)
		}
		task := s.Tasks[id]
		if task == nil || task.Owner != first.Session || task.Status != "awaiting_review" {
			t.Fatalf("task %s before review: %+v", id, task)
		}
		out, err := execParentTool(t, r, "swarm_review", map[string]any{"task": id, "revision": task.Revision, "accept": true})
		if err != nil {
			t.Fatal(err)
		}
		var reviewed map[string]any
		if err := json.Unmarshal([]byte(out), &reviewed); err != nil {
			t.Fatal(err)
		}
		return reviewed
	}
	if reviewed := accept(second.ID); reviewed["retired"] != nil || reviewed["retirement"] != nil {
		t.Fatalf("accepting the second task retired the member: %v", reviewed)
	}
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if m := s.Members[first.Session]; m.Control != MemberControlEnabled || s.Contexts[m.Context] == nil {
		t.Fatalf("member with an unreviewed task: control=%q context=%v", m.Control, s.Contexts[m.Context] != nil)
	}
	if reviewed := accept(first.Task); reviewed["retired"] != float64(1) {
		t.Fatalf("accepting the last task did not retire the member: %v", reviewed)
	}
	if s, err = r.State(ctx); err != nil {
		t.Fatal(err)
	}
	if m := s.Members[first.Session]; m.Control != MemberControlRetired || len(s.Contexts) != 0 {
		t.Fatalf("member after every task settled: control=%q contexts=%d", m.Control, len(s.Contexts))
	}
}

// Retiring a context inside a running workflow closes that workflow's tool
// binding for it before the directory goes, so no bound tool outlives it.
func TestRetirementClosesWorkflowBinding(t *testing.T) {
	type observed struct {
		hosts, bound int
	}
	skipIfWindows(t)
	seen := make(chan observed, 1)
	var calls atomic.Int32
	var r *Runtime
	r = runtimeTest(t, modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		if calls.Add(1) == 2 {
			r.mu.Lock()
			o := observed{hosts: len(r.workflowHosts)}
			for _, host := range r.workflowHosts {
				host.mu.Lock()
				o.bound += len(host.bound)
				host.mu.Unlock()
			}
			r.mu.Unlock()
			seen <- o
		}
		return answer("done")
	}), 1, 2)
	if _, err := r.config.Registry.LoadToolAuto("bash"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	report, err := r.RunWorkflow(ctx, `polly.defineWorkflow({name:"bound",inputSchema:polly.schema.object({}),async run(){
const a=await polly.agent({task:"one",readOnly:true});
await polly.scope({context:a.context}, w=>w.exec("true"));
const t=await polly.tasks.read(a.task);
await polly.tasks.review({task:t.id,revision:t.revision,accept:true});
const b=await polly.agent({task:"two",readOnly:true});
return {first:a.context};
}})`, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case o := <-seen:
		if o.hosts != 1 || o.bound != 0 {
			t.Fatalf("after retirement: %d hosts, %d bound contexts; want the retired binding closed", o.hosts, o.bound)
		}
	default:
		t.Fatal("second agent never observed")
	}
	r.mu.Lock()
	remaining := len(r.workflowHosts)
	r.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("workflow host retained after the run: %d", remaining)
	}
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(report.Output)
	if strings.Contains(string(raw), `"first":""`) || len(s.Contexts) != 1 {
		t.Fatalf("output %s, contexts %d", raw, len(s.Contexts))
	}
}
