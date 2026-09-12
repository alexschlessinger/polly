package swarm

import (
	"context"
	"errors"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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

func awaitReleased(t *testing.T, r *Runtime, member string) *State {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return awaitState(t, r, ctx, func(s *State) bool {
		r.releaseMu.Lock()
		releasing := r.releaseRunning
		r.releaseMu.Unlock()
		return s.Members[member].Context == "" && !r.HasActive() && !releasing
	})
}

func TestDeliveredResearchReleasesAndFollowupRestoresSameMember(t *testing.T) {
	for _, git := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "snapshot"}[git], func(t *testing.T) {
			r := scratchRuntime(t, doneModel(), git)
			ctx := context.Background()
			first, err := r.Agent(ctx, "", AgentRequest{Label: "Test agent", Task: "inspect", ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			initial, _ := r.read(ctx)
			source := initial.Tasks[first.Task].StartingSnapshot
			admitParent(t, r)
			s := awaitReleased(t, r, first.Session)
			if s.Members[first.Session].Control != MemberControlEnabled || s.Tasks[first.Task].Status != "done" {
				t.Fatal("release changed identity or completion")
			}
			task, err := r.Followup(ctx, "", FollowupRequest{Task: first.Task, Question: "explain"})
			if err != nil {
				t.Fatal(err)
			}
			next, err := r.Agent(ctx, "", AgentRequest{Session: task.Owner, TaskID: task.ID, Task: "explain"})
			if err != nil {
				t.Fatal(err)
			}
			if next.Session != first.Session || next.Context == first.Context || next.Execution == first.Execution {
				t.Fatalf("follow-up identity: first=%+v next=%+v", first, next)
			}
			s, _ = r.read(ctx)
			if s.Tasks[next.Task].StartingSnapshot != source || s.Tasks[next.Task].Follows != first.Task || s.Tasks[first.Task].Revision != first.Revision {
				t.Fatal("follow-up changed the original or its source")
			}
			admitParent(t, r)
			awaitReleased(t, r, first.Session)
		})
	}
}

func TestWorkspaceReleaseRetainsUnintegratedEditor(t *testing.T) {
	r, result, _ := noEditResult(t, false)
	ctx := context.Background()
	s, _ := r.read(ctx)
	c := s.Contexts[result.Context]
	if err := os.WriteFile(filepath.Join(c.Root, "source.txt"), []byte("unintegrated edit"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := r.CancelTask(ctx, result.Task); err != nil {
		t.Fatal(err)
	}
	wait, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	s = awaitState(t, r, wait, func(s *State) bool {
		c := s.Contexts[result.Context]
		return c != nil && c.Release == WorkspaceRetained
	})
	if !strings.Contains(s.Contexts[result.Context].Reason, "unintegrated") || s.Members[result.Session].Context != result.Context {
		t.Fatal("editor edits were not visibly retained")
	}
	if _, err := os.Stat(c.Root); err != nil {
		t.Fatal("retained files were removed")
	}
}

func TestWorkflowExplicitReleaseAndFollowup(t *testing.T) {
	r := runtimeTest(t, doneModel(), 1, 5)
	report, err := r.RunWorkflow(context.Background(), `polly.defineWorkflow({name:"followup",inputSchema:polly.schema.object({}),async run(){const a=await polly.agent({label:"Test agent",task:"inspect",readOnly:true});await polly.release(a.context);const again=await polly.release(a.context);if(!again.dormant)throw Error("missing historical receipt");const b=await polly.followup({task:a.task,question:"explain"});const task=await polly.tasks.read(b.task);if(task.status!=="done"||task.follows!==a.task||a.context===b.context||a.session!==b.session)throw Error("bad followup");return b;}})`, map[string]any{})
	if err != nil {
		t.Fatalf("report=%+v err=%v", report, err)
	}
}

func TestForgetRefusesImplicitSnapshotRefresh(t *testing.T) {
	r := scratchRuntime(t, doneModel(), true)
	ctx := context.Background()
	result, err := r.Agent(ctx, "", AgentRequest{Label: "Test agent", Task: "inspect", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	admitParent(t, r)
	awaitReleased(t, r, result.Session)
	if err := r.Forget(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Followup(ctx, "", FollowupRequest{Task: result.Task, Question: "explain"}); err == nil || !strings.Contains(err.Error(), "snapshot is unavailable") {
		t.Fatalf("forgotten followup: %v", err)
	}
	if _, err := r.Agent(ctx, "", AgentRequest{Session: result.Session, Task: "explain"}); err == nil || !strings.Contains(err.Error(), "snapshot is unavailable") {
		t.Fatalf("forgotten continuation: %v", err)
	}
}

// Manual cleanup tests hold reclamation so they can inject changes between proof
// and removal. Production behavior is exercised by the automatic release tests.
func suspendAutoRelease(t *testing.T, r *Runtime) {
	t.Helper()
	r.releaseMu.Lock()
	r.releaseStopped = true
	r.releasePending = false
	r.releaseMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	awaitIdle(t, r, ctx)
	awaitState(t, r, ctx, func(*State) bool {
		r.releaseMu.Lock()
		defer r.releaseMu.Unlock()
		return !r.releaseRunning
	})
}

func TestReleaseRecoversAfterFilesGoneAndProtectsReusedSlot(t *testing.T) {
	r, result, _ := noEditResult(t, false)
	ctx := context.Background()
	suspendAutoRelease(t, r)
	s, _ := r.read(ctx)
	old := s.Contexts[result.Context]
	if err := r.update(ctx, func(s *State) error { s.Contexts[old.ID].Release = WorkspaceReleasing; return nil }); err != nil {
		t.Fatal(err)
	}
	manager, err := r.manager(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Cleanup(ctx, *old.Checkout, old.Checkout.Base.Tree); err != nil {
		t.Fatal(err)
	}
	// Another checkout occupies the freed slot before the old deletion receipt.
	fresh, err := r.makeContext(ctx, r.ID, AgentRequest{Snapshot: old.Checkout.Base.ID, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Root != old.Root {
		t.Fatal("fixture did not reuse the freed slot")
	}
	if n, err := r.releaseWorkspaces(ctx); err != nil || n != 1 {
		t.Fatalf("recovery: removed=%d err=%v", n, err)
	}
	s, _ = r.read(ctx)
	if s.Contexts[old.ID] != nil || s.Members[result.Session].Context != "" || s.Contexts[fresh.ID] == nil {
		t.Fatal("recovery lost current workspace identity")
	}
	if _, err := os.Stat(filepath.Join(fresh.Root, "source.txt")); err != nil {
		t.Fatal("recovery removed the replacement's files")
	}
}

func TestExplicitReleaseWaitIsCancelable(t *testing.T) {
	r := runtimeTest(t, doneModel(), 1, 2)
	ctx := context.Background()
	suspendAutoRelease(t, r)
	first, err := r.Agent(ctx, "", AgentRequest{Label: "Test agent", Task: "inspect", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.update(ctx, func(s *State) error {
		s.Members[first.Session].Controller = "owner"
		s.Contexts[first.Context].Release = WorkspaceReleasing
		s.Executions[first.Execution].Workflow = "owner"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	lock := r.contextMutex(first.Context)
	lock.Lock()
	wait, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := (&workflowHost{runtime: r, controller: "owner"}).release(wait, first.Context)
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("release wait ignored cancellation")
	}
	lock.Unlock()
	r.notifyRelease(first.Context)
}
