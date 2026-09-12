package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/sessions"
)

// Observes entry to the cancelable wait without startup sleeps.
type maintenanceWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *maintenanceWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestReleaseMaintenanceDoesNotCountAsActive(t *testing.T) {
	r := runtimeTest(t, doneModel(), 1, 2)
	unlock, err := r.lockMaintenance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	r.scheduleRelease() // Worker is running but cannot enter its pass yet.
	if r.HasActive() {
		t.Fatal("release worker counted as an active member")
	}
	r.mu.Lock()
	r.active["member"] = &invocation{}
	r.mu.Unlock()
	if !r.HasActive() {
		t.Fatal("executing member did not count as active")
	}
	r.mu.Lock()
	delete(r.active, "member")
	r.workflowCancels["workflow"] = func() {}
	r.mu.Unlock()
	if !r.HasActive() {
		t.Fatal("workflow did not count as active")
	}
	r.mu.Lock()
	delete(r.workflowCancels, "workflow")
	r.mu.Unlock()
	closed := make(chan error, 1)
	go func() { closed <- r.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not cancel and join the queued release worker")
	}
	r.releaseMu.Lock()
	defer r.releaseMu.Unlock()
	if r.releaseRunning {
		t.Fatal("Close returned before joining the release worker")
	}
}

func TestReleaseMaintenanceWaitIsCancelableAndOutsideSchedulerLocks(t *testing.T) {
	for _, op := range []string{"cleanup", "forget", "release"} {
		t.Run(op, func(t *testing.T) {
			r := runtimeTest(t, doneModel(), 1, 2)
			unlock, err := r.lockMaintenance(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			base, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			ctx := &maintenanceWaitContext{Context: base, waiting: make(chan struct{})}
			result := make(chan error, 1)
			go func() {
				switch op {
				case "cleanup":
					result <- r.Cleanup(ctx, "")
				case "forget":
					result <- r.Forget(ctx)
				case "release":
					_, err := r.releaseWorkspace(ctx, "missing")
					result <- err
				}
			}()
			select {
			case <-ctx.waiting:
			case <-base.Done():
				t.Fatal("operation never waited for maintenance")
			}
			if !r.launchMu.TryLock() {
				t.Fatal("maintenance wait held launchMu")
			}
			r.launchMu.Unlock()
			if !r.parentTools.TryLock() {
				t.Fatal("maintenance wait held parentTools")
			}
			r.parentTools.Unlock()
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation = %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("maintenance wait ignored cancellation")
			}
		})
	}
}

func TestCleanupWaitsForAutomaticRelease(t *testing.T) {
	for _, wholeFamily := range []bool{false, true} {
		t.Run(map[bool]string{false: "context", true: "family"}[wholeFamily], func(t *testing.T) {
			r := runtimeTest(t, doneModel(), 1, 2)
			a, err := r.Agent(context.Background(), "", AgentRequest{Label: "Test agent", Task: "inspect", ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			unlock, err := r.lockMaintenance(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			var once sync.Once
			release := func() { once.Do(unlock) }
			defer release()
			admitParent(t, r)
			base, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			ctx := &maintenanceWaitContext{Context: base, waiting: make(chan struct{})}
			id := a.Context
			if wholeFamily {
				id = ""
			}
			result := make(chan error, 1)
			go func() { result <- r.Cleanup(ctx, id) }()
			select {
			case <-ctx.waiting:
			case <-base.Done():
				t.Fatal("cleanup never queued")
			}
			release()
			select {
			case err := <-result:
				if err != nil {
					t.Fatal(err)
				}
			case <-base.Done():
				t.Fatal("cleanup did not finish after release")
			}
			s, err := r.read(base)
			if err != nil || s.Contexts[a.Context] != nil || s.Members[a.Session].Context != "" {
				t.Fatalf("cleanup left workspace: %v", err)
			}
			if err := r.Cleanup(base, a.Context); err != nil {
				t.Fatalf("already released context: %v", err)
			}
		})
	}
}

func TestTargetedReleaseReportsOutcomeAndLeavesOtherWorkspaces(t *testing.T) {
	r := runtimeTest(t, doneModel(), 2, 4)
	suspendAutoRelease(t, r)
	ctx := context.Background()
	var agents []AgentResult
	for range 2 {
		a, err := r.Agent(ctx, "", AgentRequest{Label: "Test agent", Task: "inspect", ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		agents = append(agents, a)
	}
	id := agents[0].Context
	check := func(want string) {
		t.Helper()
		out, err := execParentTool(t, r, "swarm_control", map[string]any{"action": "release", "id": id})
		if err != nil {
			t.Fatal(err)
		}
		var result map[string]string
		if err := json.Unmarshal([]byte(out), &result); err != nil {
			t.Fatal(err)
		}
		if result["context"] != id || result["status"] != want || want != "released" && result["reason"] == "" {
			t.Fatalf("release %s: %s", want, out)
		}
		s, err := r.read(ctx)
		if err != nil || s.Contexts[agents[1].Context] == nil {
			t.Fatalf("targeted release touched another workspace: %v", err)
		}
	}
	check("ineligible") // Result not delivered yet.
	admitParent(t, r)
	if err := r.update(ctx, func(s *State) error {
		s.Contexts[id].Release, s.Contexts[id].Reason = WorkspaceRetained, "inspect changed files"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	check("retained")
	if err := r.update(ctx, func(s *State) error {
		s.Contexts[id].Release, s.Contexts[id].Reason = "", ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	lock := r.contextMutex(id)
	lock.Lock()
	func() { defer lock.Unlock(); check("busy") }()
	check("released")
	check("released")
}

type releaseReceiptGate struct {
	sessions.CoordinationSession
	calls   atomic.Int32
	entered chan struct{}
	resume  chan struct{}
}

func (s *releaseReceiptGate) UpdateCoordination(ctx context.Context, fn func(*sessions.CoordinationState) error) error {
	if s.calls.Add(1) == 2 { // Mark has committed; pause the removal receipt.
		close(s.entered)
		select {
		case <-s.resume:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.CoordinationSession.UpdateCoordination(ctx, fn)
}

func TestCloseJoinsTargetedReleaseReceipt(t *testing.T) {
	r := runtimeTest(t, doneModel(), 1, 2)
	suspendAutoRelease(t, r)
	a, err := r.Agent(context.Background(), "", AgentRequest{Label: "Test agent", Task: "inspect", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	admitParent(t, r)
	gate := &releaseReceiptGate{CoordinationSession: r.parent, entered: make(chan struct{}), resume: make(chan struct{})}
	r.parent = gate
	var once sync.Once
	unblock := func() { once.Do(func() { close(gate.resume) }) }
	defer unblock()
	done := make(chan error, 1)
	go func() { _, err := r.releaseWorkspace(context.Background(), a.Context); done <- err }()
	select {
	case <-gate.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("release never reached its removal receipt")
	}
	closed := make(chan error, 1)
	go func() { closed <- r.Close() }()
	select {
	case <-r.ctx.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("Close never started")
	}
	select {
	case <-closed:
		t.Fatal("Close returned before the removal receipt")
	default:
	}
	unblock()
	for _, ch := range []<-chan error{done, closed} {
		select {
		case err := <-ch:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("release or shutdown did not complete")
		}
	}
	s, err := r.read(context.Background())
	if err != nil || s.Contexts[a.Context] != nil {
		t.Fatalf("removal receipt missing: %v", err)
	}
}
