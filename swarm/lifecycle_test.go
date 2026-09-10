package swarm

import (
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/workflow"
)

// memberFixture builds the smallest state that exercises one presentation.
func memberFixture(control, execution, task string, stop messages.StopReason) (*State, *Member) {
	s := &State{Members: map[string]*Member{}, Executions: map[string]*Execution{}, Tasks: map[string]*Task{}}
	m := &Member{ID: "m", Name: "worker", Control: MemberControl(control)}
	if task != "" {
		s.Tasks["t"] = &Task{ID: "t", Status: task, Revision: 1}
		m.Task = "t"
	}
	if execution != "" {
		s.Executions["e"] = &Execution{ID: "e", Member: "m", Status: execution, StopReason: stop, Iterations: 3, Request: AgentRequest{MaxIterations: 5}, Generation: 1}
		m.Execution = "e"
	}
	s.Members["m"] = m
	return s, m
}

func TestMemberStateLifecycleTable(t *testing.T) {
	for _, tc := range []struct {
		name, control, execution, task string
		stop                           messages.StopReason
		lifecycle                      Lifecycle
		display                        string
		busy, attention                bool
	}{
		{name: "never ran", lifecycle: LifecycleIdle, display: "idle"},
		{name: "assigned only", task: "pending", lifecycle: LifecycleIdle, display: "idle · pending", attention: true},
		{name: "queued", execution: "queued", task: "running", lifecycle: LifecycleActive, display: "active · queued", busy: true},
		{name: "running", execution: "running", task: "running", lifecycle: LifecycleActive, display: "active", busy: true},
		{name: "parked", execution: "waiting", task: "running", lifecycle: LifecycleWaiting, display: "waiting", busy: true},
		{name: "submitted", execution: "completed", task: "awaiting_review", lifecycle: LifecycleIdle, display: "idle · awaiting review", attention: true},
		{name: "accepted", execution: "completed", task: "done", lifecycle: LifecycleIdle, display: "idle · done"},
		{name: "iteration limit", execution: "paused", task: "blocked", stop: messages.StopReasonMaxIterations, lifecycle: LifecyclePaused, display: "paused · iteration limit (3/5)", attention: true},
		{name: "interrupted", execution: "paused", task: "blocked", lifecycle: LifecyclePaused, display: "paused · interrupted", attention: true},
		{name: "failed", execution: "failed", task: "blocked", lifecycle: LifecyclePaused, display: "paused · failed", attention: true},
		{name: "stopped with open work", control: "stopped", execution: "paused", task: "blocked", lifecycle: LifecyclePaused, display: "paused · stopped · blocked", attention: true},
		{name: "stopped after acceptance", control: "stopped", execution: "completed", task: "done", lifecycle: LifecyclePaused, display: "paused · stopped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, m := memberFixture(tc.control, tc.execution, tc.task, tc.stop)
			p := MemberState(s, m)
			if p.Lifecycle != tc.lifecycle || p.Display != tc.display || p.Busy != tc.busy || p.Attention != tc.attention {
				t.Fatalf("presentation %+v", p)
			}
			if p.Outcome != tc.execution || string(p.Control) != tc.control {
				t.Fatalf("raw facts changed: %+v", p)
			}
			if tc.execution != "" && (p.Iterations != 3 || p.MaxIterations != 5) {
				t.Fatalf("iteration facts missing: %+v", p)
			}
		})
	}
	if got := DisplayLabel(LifecycleIdle, "awaiting review", true); got != "idle · awaiting review · deferred" {
		t.Fatalf("deferred label %q", got)
	}
}

func TestCompactRosterCountsAndWorking(t *testing.T) {
	s, m := memberFixture("", "completed", "done", "")
	m.Label = "worker label"
	if roster := compactRoster(s); !strings.Contains(roster, "1 members: 0 working, 1 idle, 0 paused.\nWorking now: none.") || strings.Contains(roster, "worker label") || strings.Contains(roster, "omitted") {
		t.Fatalf("roster %q", roster)
	}
	s.Members["gone"] = &Member{ID: "gone", Label: "released researcher", Context: "", Task: "tg"}
	if roster := compactRoster(s); strings.Contains(roster, "released researcher") || !strings.Contains(roster, "2 members: 0 working, 2 idle, 0 paused.") {
		t.Fatalf("roster with a released member %q", roster)
	}
	s, m = memberFixture("", "running", "running", "")
	m.Label = "worker label"
	s.Members["stuck"] = &Member{ID: "stuck", Label: "stuck", Control: MemberControlStopped}
	if roster := compactRoster(s); !strings.Contains(roster, "2 members: 1 working, 0 idle, 1 paused.\nWorking now:\nm · worker label · active · task t\n") {
		t.Fatalf("roster with a working member %q", roster)
	}
}

// The parent's wait ends on lifecycle, control and execution identity, never
// on a label or on the queued→running hop of the same execution.
func TestFingerprintStableAcrossQueuedToRunning(t *testing.T) {
	s, m := memberFixture("", "queued", "running", "")
	e := s.Executions["e"]
	queued := coordinationFingerprint(s)
	e.Status = "running"
	if coordinationFingerprint(s) != queued {
		t.Fatal("queued→running changed the fingerprint")
	}
	e.Status = "waiting"
	waiting := coordinationFingerprint(s)
	if waiting == queued {
		t.Fatal("parking did not change the fingerprint")
	}
	e.Generation++
	resumed := coordinationFingerprint(s)
	if resumed == waiting {
		t.Fatal("a new generation did not change the fingerprint")
	}
	m.Control = MemberControlStopped
	if coordinationFingerprint(s) == resumed {
		t.Fatal("a control change did not change the fingerprint")
	}
}

func TestWorkflowControlledFollowsWorkflowStatus(t *testing.T) {
	s := &State{Workflows: map[string]*workflow.Report{}}
	for _, status := range []string{"running", "completed", "failed", "interrupted", "canceled"} {
		s.Workflows[status] = &workflow.Report{ID: status, Status: status}
	}
	for _, tc := range []struct {
		name string
		e    *Execution
		want bool
	}{
		{name: "nil execution"},
		{name: "direct spawn", e: &Execution{ID: "e"}},
		{name: "running workflow", e: &Execution{ID: "e", Workflow: "running"}, want: true},
		{name: "completed workflow", e: &Execution{ID: "e", Workflow: "completed"}},
		{name: "failed workflow", e: &Execution{ID: "e", Workflow: "failed"}},
		{name: "interrupted workflow", e: &Execution{ID: "e", Workflow: "interrupted"}},
		{name: "canceled workflow", e: &Execution{ID: "e", Workflow: "canceled"}},
		{name: "missing workflow", e: &Execution{ID: "e", Workflow: "missing"}},
	} {
		if got := workflowControlled(s, tc.e); got != tc.want {
			t.Errorf("%s: workflowControlled = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A parked parent sleeps through what a running workflow controls; the full
// fingerprint that drives the settlement nudge still moves on every change.
func TestFingerprintIgnoresWorkflowInternalTransitions(t *testing.T) {
	s := &State{Members: map[string]*Member{}, Executions: map[string]*Execution{}, Tasks: map[string]*Task{}, Workflows: map[string]*workflow.Report{}}
	s.Workflows["w"] = &workflow.Report{ID: "w", Status: "running"}
	s.Executions["ea"] = &Execution{ID: "ea", Member: "a", Workflow: "w", Status: "running", Generation: 1}
	s.Members["a"] = &Member{ID: "a", Execution: "ea", Task: "ta"}
	s.Tasks["ta"] = &Task{ID: "ta", Execution: "ea", Owner: "a", Status: "running", Revision: 1}
	s.Executions["ed"] = &Execution{ID: "ed", Member: "d", Status: "running", Generation: 1}
	s.Members["d"] = &Member{ID: "d", Execution: "ed", Task: "td"}
	s.Tasks["td"] = &Task{ID: "td", Execution: "ed", Owner: "d", Status: "running", Revision: 1}
	s.Tasks["tp"] = &Task{ID: "tp", Status: "pending", Revision: 1}
	before, _ := coordinationEntries(s)
	full := coordinationFingerprint(s)
	expect := func(step string, want bool) {
		t.Helper()
		if got := parentWaitChanged(before, s); got != want {
			t.Fatalf("%s: parentWaitChanged = %v, want %v", step, got, want)
		}
		if want {
			before, _ = coordinationEntries(s)
		}
	}
	s.Executions["ea"].Status = "waiting"
	expect("workflow member parks", false)
	s.Executions["ea"].Status = "completed"
	s.Tasks["ta"].Status = "awaiting_review"
	s.Tasks["ta"].Revision++
	expect("workflow member completes", false)
	s.Executions["eb"] = &Execution{ID: "eb", Member: "b", Workflow: "w", Status: "running", Generation: 1}
	s.Members["b"] = &Member{ID: "b", Execution: "eb", Task: "tp"}
	s.Tasks["tp"].Owner, s.Tasks["tp"].Status, s.Tasks["tp"].Execution = "b", "running", "eb"
	expect("workflow takes over a pending task", false)
	s.Members["c"] = &Member{ID: "c", Controller: "w"}
	expect("workflow member recorded before its execution", false)
	s.Executions["ec"] = &Execution{ID: "ec", Member: "c", Workflow: "w", Status: "queued", Generation: 1}
	s.Members["c"].Execution = "ec"
	expect("workflow member's execution recorded", false)
	if coordinationFingerprint(s) == full {
		t.Fatal("the full fingerprint ignored workflow-internal transitions")
	}
	s.Executions["ed"].Status = "completed"
	expect("direct child completes", true)
	s.Workflows["w"].Status = "completed"
	expect("workflow completes", true)
	s.Workflows["w"].Acknowledged = true
	expect("workflow acknowledged", true)
	s.Executions["ea"].Generation++
	expect("resume after the workflow ended", true)
}
