package swarm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/workflow"
)

func TestWorkspaceReleaseEligibilityHolds(t *testing.T) {
	for _, hold := range []string{"none", "no_tasks", "canceled", "active", "queued", "running", "waiting", "paused", "open_task", "reserved", "applying", "recovery_required", "unbound", "retained"} {
		t.Run(hold, func(t *testing.T) {
			s := seedState().s
			c := &ExecutionContext{ID: "copy", Owner: "member"}
			m := &Member{ID: "member", Context: c.ID, Execution: "execution", Controller: "workflow"}
			s.Members[m.ID] = m
			s.Executions[m.Execution] = &Execution{ID: m.Execution, Member: m.ID, Status: "completed"}
			s.Tasks["task"] = &Task{ID: "task", Owner: m.ID, Status: "done", Requirement: RequirementDelivered, Execution: m.Execution}
			active := map[string]*invocation{}
			reserved := false
			switch hold {
			case "active":
				active[m.ID] = &invocation{}
			case "queued", "running", "waiting", "paused":
				s.Executions[m.Execution].Status = hold
			case "no_tasks":
				delete(s.Tasks, "task")
			case "canceled":
				s.Tasks["task"].Status = "canceled"
			case "open_task":
				s.Tasks["other"] = &Task{ID: "other", Owner: m.ID, Status: "pending"}
			case "reserved":
				reserved = true
			case "applying", "recovery_required":
				s.Applies["apply"] = &ApplyRecord{Status: hold, Tasks: []TaskReference{{Task: "task"}}}
			case "unbound":
				m.Context = "other"
			case "retained":
				c.Release = WorkspaceRetained
			}
			ok, why := releaseEligible(s, c, active, func(string) bool { return reserved })
			want := hold == "none" || hold == "no_tasks" || hold == "canceled"
			if ok != want || !ok && why == "" {
				t.Fatalf("eligible=%v reason=%q", ok, why)
			}
			if hold == "reserved" {
				if ok, _ := explicitReleaseEligible(s, c, "workflow", active); !ok {
					t.Fatal("own reservation blocked explicit release")
				}
			}
			if ok, _ := explicitReleaseEligible(s, c, "another", active); ok {
				t.Fatal("another workflow gained release authority")
			}
		})
	}
}

func TestReviewedResearchAndUnchangedEditorReleaseOnlyAfterAcceptance(t *testing.T) {
	for _, readOnly := range []bool{true, false} {
		t.Run(map[bool]string{true: "research", false: "editor"}[readOnly], func(t *testing.T) {
			r, result, ref := noEditResult(t, readOnly)
			ctx := context.Background()
			if n, err := r.releaseWorkspaces(ctx); err != nil || n != 0 {
				t.Fatalf("unaccepted release: %d %v", n, err)
			}
			if readOnly {
				if err := r.Review(ctx, ref.Task, ref.Revision, true, ""); err != nil {
					t.Fatal(err)
				}
			} else {
				integrateOK(t, r, IntegrateRequest{Tasks: []TaskReference{ref}})
			}
			s := awaitReleased(t, r, result.Session)
			if s.Tasks[ref.Task].AcceptedRevision != ref.Revision || s.Tasks[ref.Task].Status != "done" {
				t.Fatal("release changed accepted result")
			}
		})
	}
}

func TestReleaseBatchUsesTwoTransactionsAndSkipsLockedWorkspace(t *testing.T) {
	var counter *countingSession
	r := runtimeTestWithParent(t, doneModel(), 2, 4, func(s sessions.Session) sessions.Session { counter = newCountingSession(s); return counter })
	ctx := context.Background()
	suspendAutoRelease(t, r)
	var results []AgentResult
	for range 2 {
		a, err := r.Agent(ctx, "", AgentRequest{Label: "Test agent", Task: "inspect", ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, a)
	}
	admitParent(t, r)
	lock := r.contextMutex(results[0].Context)
	lock.Lock()
	before := counter.updates.Load()
	if n, err := r.releaseWorkspaces(ctx); err != nil || n != 1 {
		t.Fatalf("locked pass: %d %v", n, err)
	}
	if got := counter.updates.Load() - before; got != 2 {
		t.Fatalf("one release used %d transactions", got)
	}
	lock.Unlock()
	before = counter.updates.Load()
	if n, err := r.releaseWorkspaces(ctx); err != nil || n != 1 {
		t.Fatalf("unlocked pass: %d %v", n, err)
	}
	if got := counter.updates.Load() - before; got != 2 {
		t.Fatalf("release used %d transactions", got)
	}
	// Another pair demonstrates both records share the same mark/delete commits.
	for range 2 {
		if _, err := r.Agent(ctx, "", AgentRequest{Label: "Test agent", Task: "inspect", ReadOnly: true}); err != nil {
			t.Fatal(err)
		}
	}
	admitParent(t, r)
	before = counter.updates.Load()
	if n, err := r.releaseWorkspaces(ctx); err != nil || n != 2 {
		t.Fatalf("batch: %d %v", n, err)
	}
	if got := counter.updates.Load() - before; got != 2 {
		t.Fatalf("batch used %d transactions", got)
	}
}

type failCoordination struct {
	sessions.CoordinationSession
	remaining int
	calls     int
}

func (s *failCoordination) UpdateCoordination(ctx context.Context, fn func(*sessions.CoordinationState) error) error {
	s.calls++
	if s.remaining > 0 {
		s.remaining--
		return errors.New("injected storage failure")
	}
	return s.CoordinationSession.UpdateCoordination(ctx, fn)
}

func TestReleaseMarkFailureRetriesThenRetains(t *testing.T) {
	r := runtimeTest(t, doneModel(), 1, 2)
	ctx := context.Background()
	suspendAutoRelease(t, r)
	a, err := r.Agent(ctx, "", AgentRequest{Label: "Test agent", Task: "inspect", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	admitParent(t, r)
	failed := &failCoordination{CoordinationSession: r.parent, remaining: 3}
	r.parent = failed
	for range 3 {
		if _, err := r.releaseWorkspaces(ctx); err == nil || !strings.Contains(err.Error(), "injected") {
			t.Fatalf("failure=%v", err)
		}
	}
	s, _ := r.read(ctx)
	c := s.Contexts[a.Context]
	if c == nil || c.Release != WorkspaceRetained || !strings.Contains(c.Reason, "injected storage failure") {
		t.Fatalf("failure not retained: %+v", c)
	}
	if s.Tasks[a.Task].Status != "done" {
		t.Fatal("cleanup failure reopened delivered work")
	}
	if n, err := r.releaseWorkspaces(ctx); err != nil || n != 0 {
		t.Fatalf("retained context retried forever: %d %v", n, err)
	}
}

func TestReleaseWaitAllowsUnrelatedLaunchAndReceivesCompletion(t *testing.T) {
	r := runtimeTest(t, doneModel(), 2, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	suspendAutoRelease(t, r)
	a, err := r.Agent(ctx, "", AgentRequest{Label: "Test agent", Task: "inspect", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	admitParent(t, r)
	if err := r.update(ctx, func(s *State) error {
		s.Contexts[a.Context].Release = WorkspaceReleasing
		s.Executions[a.Execution].Workflow = "owner"
		s.Members[a.Session].Controller = "owner"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	lock := r.contextMutex(a.Context)
	lock.Lock()
	finished := make(chan error, 1)
	go func() {
		_, err := (&workflowHost{runtime: r, controller: "owner"}).release(ctx, a.Context)
		finished <- err
	}()
	// The waiter must not acquire either global lock while removal owns the context.
	b, err := r.Agent(ctx, "", AgentRequest{Label: "Test agent", Task: "unrelated", ReadOnly: true})
	if err != nil {
		lock.Unlock()
		t.Fatal(err)
	}
	if b.Session == a.Session {
		lock.Unlock()
		t.Fatal("fixture reused the member")
	}
	lock.Unlock()
	r.notifyRelease(a.Context)
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("missed release notification")
	}
	value, err := (&workflowHost{runtime: r, controller: "owner"}).release(ctx, a.Context)
	if err != nil || value.(map[string]any)["dormant"] != true {
		t.Fatalf("historical release: %+v %v", value, err)
	}
	if _, err := (&workflowHost{runtime: r, controller: "other"}).release(ctx, a.Context); err == nil {
		t.Fatal("historical receipt leaked authority")
	}
}

func TestReleaseLeftoverFinishesOnOpen(t *testing.T) {
	r, a, _ := noEditResult(t, false)
	ctx := context.Background()
	suspendAutoRelease(t, r)
	if err := r.update(ctx, func(s *State) error { s.Contexts[a.Context].Release = WorkspaceReleasing; return nil }); err != nil {
		t.Fatal(err)
	}
	before, _ := r.read(ctx)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(r.config)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	after := awaitReleased(t, reopened, a.Session)
	if !reflect.DeepEqual(after.Tasks[a.Task], before.Tasks[a.Task]) {
		t.Fatal("reopening a release changed the task")
	}
}

func TestReleaseClosesBindingsBeforeFilesDisappear(t *testing.T) {
	skipIfWindows(t)
	r := runtimeTest(t, doneModel(), 1, 3)
	if _, err := r.config.Registry.LoadToolAuto("bash"); err != nil {
		t.Fatal(err)
	}
	report, err := r.RunWorkflow(context.Background(), `polly.defineWorkflow({name:"bindings",inputSchema:polly.schema.object({}),async run(){const c=await polly.context({readOnly:true});await polly.scope({context:c},w=>w.exec("true"));await polly.release(c);return await polly.release(c)}})`, map[string]any{})
	if err != nil {
		t.Fatalf("%+v %v", report, err)
	}
	s, _ := r.read(context.Background())
	if len(s.Contexts) != 0 {
		t.Fatal("workflow context remained")
	}
}

func TestReleaseWorkerShutdownFence(t *testing.T) {
	r := runtimeTest(t, doneModel(), 1, 2)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				r.scheduleRelease()
			}
		}()
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	r.scheduleRelease()
	r.releaseMu.Lock()
	defer r.releaseMu.Unlock()
	if !r.releaseStopped || r.releaseRunning {
		t.Fatal("worker restarted after close")
	}
}

func TestReleaseProofFailureRetainsFiles(t *testing.T) {
	r, a, _ := noEditResult(t, false)
	ctx := context.Background()
	suspendAutoRelease(t, r)
	if err := r.CancelTask(ctx, a.Task); err != nil {
		t.Fatal(err)
	}
	s, _ := r.read(ctx)
	c := s.Contexts[a.Context]
	path := filepath.Join(c.Root, "source.txt")
	if err := os.WriteFile(path, []byte("never integrated"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := r.releaseWorkspaces(ctx)
	var failure *workflow.Error
	if !errors.As(err, &failure) || failure.Code != "unintegrated_changes" {
		t.Fatalf("proof=%v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "never integrated" {
		t.Fatal("unintegrated content lost")
	}
}

func TestContextCreationSaveFailureDoesNotLeakCheckout(t *testing.T) {
	r := scratchRuntime(t, doneModel(), true)
	ctx := context.Background()
	suspendAutoRelease(t, r)
	// Prepare and initialize the manager before injecting the creation save failure.
	if err := r.prepare(ctx); err != nil {
		t.Fatal(err)
	}
	manager, err := r.manager(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r.parent = &failCoordination{CoordinationSession: r.parent, remaining: 1}
	if _, err := r.makeContext(ctx, r.ID, AgentRequest{ReadOnly: true}); err == nil {
		t.Fatal("injected creation failure succeeded")
	}
	for _, slot := range manager.Slots {
		if _, err := os.Stat(filepath.Join(slot, "owner")); !os.IsNotExist(err) {
			t.Fatalf("failed creation leaked a slot: %s %v", slot, err)
		}
	}
	if n, err := r.releaseWorkspaces(ctx); err != nil || n != 0 {
		t.Fatalf("orphan records: %d %v", n, err)
	}
}

func TestEligibleWorkspaceReleaseResumesOnOpenBeforeMark(t *testing.T) {
	r := runtimeTest(t, doneModel(), 1, 2)
	ctx := context.Background()
	suspendAutoRelease(t, r)
	a, err := r.Agent(ctx, "", AgentRequest{Label: "Test agent", Task: "inspect", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	admitParent(t, r)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(r.config)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	after := awaitReleased(t, reopened, a.Session)
	if after.Tasks[a.Task].Status != "done" || after.Tasks[a.Task].Delivery == nil {
		t.Fatal("open changed completion evidence")
	}
}
