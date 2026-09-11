package swarm

import (
	"context"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/workflow"
)

type stateSeed struct{ s *State }

func seedState() *stateSeed {
	return &stateSeed{s: &State{
		Runs:       map[string]*Run{"run": {ID: "run", Status: "running", Starts: 1, Limit: 8}},
		Members:    map[string]*Member{},
		Tasks:      map[string]*Task{},
		Executions: map[string]*Execution{},
		Messages:   map[string]*Mail{},
		Workflows:  map[string]*workflow.Report{},
		Applies:    map[string]*ApplyRecord{},
	}}
}

func (x *stateSeed) member(id string, readOnly bool, task, execution string) *Member {
	m := &Member{ID: id, Name: id, Label: id, ReadOnly: readOnly, Task: task, Execution: execution}
	x.s.Members[id] = m
	return m
}

func (x *stateSeed) exec(id, member, status, wf string) *Execution {
	e := &Execution{ID: id, Run: "run", Member: member, Status: status, Workflow: wf, Request: AgentRequest{MaxIterations: 5}}
	x.s.Executions[id] = e
	return e
}

func (x *stateSeed) task(id, owner, execution, status string, deps ...string) *Task {
	t := &Task{ID: id, Run: "run", Owner: owner, Execution: execution, Status: status, Revision: 1, Dependencies: deps}
	x.s.Tasks[id] = t
	return t
}

func (x *stateSeed) present() Presentation { return Present(x.s, "parent", "parent") }

func decisionIDs(p Presentation) string {
	var ids []string
	for _, d := range p.Decisions {
		ids = append(ids, d.Kind+":"+d.ID)
	}
	return strings.Join(ids, " ")
}

func workingIDs(p Presentation) string {
	var ids []string
	for _, w := range p.Working {
		ids = append(ids, w.Kind+":"+w.ID+"="+w.State)
	}
	return strings.Join(ids, " ")
}

func expectLists(t *testing.T, p Presentation, decisions, working string) {
	t.Helper()
	if got := decisionIDs(p); got != decisions {
		t.Fatalf("decisions = %q, want %q", got, decisions)
	}
	if got := workingIDs(p); got != working {
		t.Fatalf("working = %q, want %q", got, working)
	}
}

func TestPresentationBuckets(t *testing.T) {
	t.Run("pending waits on a running dependency", func(t *testing.T) {
		x := seedState()
		x.member("m", false, "D", "e")
		x.exec("e", "m", "running", "")
		x.task("D", "m", "e", "running")
		x.task("P", "", "", "pending", "D")
		p := x.present()
		expectLists(t, p, "", "member:m=active task:P=pending · waiting on D")
		if len(settlementBlockers(deriveFacts(x.s, "parent"))) != 2 || p.Counts.Working != 2 || p.Counts.NeedsDecision != 0 {
			t.Fatalf("blocker set or counts changed: %+v", p.Counts)
		}
		if !strings.HasPrefix(p.Next, "Park with swarm_wait") {
			t.Fatalf("next = %q", p.Next)
		}
	})
	t.Run("pending with a canceled dependency", func(t *testing.T) {
		x := seedState()
		x.task("C", "", "", "canceled")
		x.task("P", "", "", "pending", "C")
		p := x.present()
		expectLists(t, p, "task:P", "")
		if d := p.Decisions[0]; d.Why != "pending" || d.Action != "dependency C is canceled; update its dependencies or cancel it" {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("pending with dependencies done", func(t *testing.T) {
		x := seedState()
		x.task("D", "", "", "done")
		x.task("P", "", "", "pending", "D")
		p := x.present()
		expectLists(t, p, "task:P", "")
		if d := p.Decisions[0]; d.Why != "pending" || d.Action != "assign and run the task or cancel it" || p.Next != "task P revision 1: assign and run the task or cancel it" {
			t.Fatalf("decision = %+v next = %q", d, p.Next)
		}
		if p.Counts.Done != 1 {
			t.Fatalf("counts = %+v", p.Counts)
		}
	})
	t.Run("pending waits on a decision", func(t *testing.T) {
		x := seedState()
		x.member("m", true, "R", "e")
		x.exec("e", "m", "completed", "")
		x.task("R", "m", "e", "awaiting_review")
		x.task("P", "", "", "pending", "R")
		expectLists(t, x.present(), "task:R", "task:P=pending · waiting on R")
	})
	t.Run("submitted one task then claimed another", func(t *testing.T) {
		x := seedState()
		x.member("m", false, "T2", "e2")
		x.exec("e1", "m", "completed", "")
		x.exec("e2", "m", "running", "")
		x.task("T1", "m", "e1", "awaiting_review")
		x.task("T2", "m", "e2", "running")
		p := x.present()
		expectLists(t, p, "task:T1", "member:m=active")
		if d := p.Decisions[0]; d.Why != "candidate ready" || !strings.Contains(d.Action, "swarm_integrate") || d.Member != "m" || d.State != "active" {
			t.Fatalf("decision = %+v", d)
		}
		if p.Working[0].Task != "T2" {
			t.Fatalf("working = %+v", p.Working[0])
		}
	})
	t.Run("changes requested with a stopped owner", func(t *testing.T) {
		x := seedState()
		m := x.member("m", false, "T", "e")
		m.Control = MemberControlStopped
		x.exec("e", "m", "paused", "")
		x.task("T", "m", "e", "changes_requested")
		p := x.present()
		expectLists(t, p, "task:T", "")
		if d := p.Decisions[0]; d.Why != "changes requested" || d.Action != "deliver the feedback and resume the member or cancel the task, or swarm_control resume m" {
			t.Fatalf("decision = %+v", d)
		}
	})
	t.Run("missing owner of an open task", func(t *testing.T) {
		x := seedState()
		m := x.member("m", false, "T", "e")
		m.Context = ""
		delete(x.s.Members, "m")
		x.exec("e", "m", "completed", "")
		x.task("T", "m", "e", "awaiting_review")
		p := x.present()
		expectLists(t, p, "task:T", "")
		if d := p.Decisions[0]; d.Action != "reassign it with swarm_update_task or cancel it" || p.Counts.Dormant != 0 {
			t.Fatalf("decision = %+v counts = %+v", d, p.Counts)
		}
	})
	t.Run("running task whose execution failed", func(t *testing.T) {
		x := seedState()
		x.member("m", false, "T", "e")
		x.exec("e", "m", "failed", "")
		x.task("T", "m", "e", "running")
		p := x.present()
		expectLists(t, p, "task:T", "")
		if d := p.Decisions[0]; d.Why != "execution e is failed" || d.Action != "inspect its saved result and explicitly resume member m or cancel the task" {
			t.Fatalf("decision = %+v", d)
		}
		x.s.Tasks["T"].Status = "blocked"
		if d := x.present().Decisions[0]; d.Why != "blocked" || d.Action != "update the task to resolve its blocker or cancel it" {
			t.Fatalf("blocked decision = %+v", d)
		}
	})
	t.Run("request admitted but unanswered", func(t *testing.T) {
		x := seedState()
		x.member("m", false, "T", "e")
		x.exec("e", "m", "waiting", "")
		x.task("T", "m", "e", "running")
		x.s.Messages["ask"] = &Mail{ID: "ask", From: "m", To: "parent", Kind: "request", Text: "which branch?", Delivered: true}
		p := x.present()
		expectLists(t, p, "mail:ask", "")
		if d := p.Decisions[0]; d.Why != "which branch?" || d.Action != `send_message {to: "m", kind: "reply", reply_to: "ask"}` || d.Member != "m" || d.State != "waiting" {
			t.Fatalf("decision = %+v", d)
		}
		if !strings.HasPrefix(p.Next, "request ask from m: send_message") {
			t.Fatalf("next = %q", p.Next)
		}
		f := deriveFacts(x.s, "parent")
		if f.wakeMail || len(settlementBlockers(f)) != 1 || settlementBlockers(f)[0].kind != KindTask {
			t.Fatal("settlement changed: delivered mail no longer wakes, the parked task blocks")
		}
		x.s.Messages["ask"].ReplyID = "answer"
		expectLists(t, x.present(), "task:T", "")
	})
	t.Run("unread reply", func(t *testing.T) {
		x := seedState()
		x.member("m", false, "", "")
		x.s.Messages["rep"] = &Mail{ID: "rep", From: "m", To: "parent", Kind: "reply", ReplyTo: "q", Text: "yes"}
		p := x.present()
		expectLists(t, p, "mail:rep", "")
		if d := p.Decisions[0]; d.Why != "unread answer to your request" || d.Action != "read_messages" {
			t.Fatalf("decision = %+v", d)
		}
		x.s.Messages["rep"].Delivered = true
		expectLists(t, x.present(), "", "")
	})
	t.Run("parked member alone and with a peer running", func(t *testing.T) {
		x := seedState()
		x.member("m", false, "T", "e")
		x.exec("e", "m", "waiting", "")
		x.task("T", "m", "e", "running")
		p := x.present()
		expectLists(t, p, "task:T", "")
		if d := p.Decisions[0]; d.Why != "execution e is waiting" {
			t.Fatalf("decision = %+v", d)
		}
		x.member("n", false, "U", "f")
		x.exec("f", "n", "running", "")
		x.task("U", "n", "f", "running")
		expectLists(t, x.present(), "", "member:m=waiting member:n=active")
	})
	t.Run("research inside a running workflow", func(t *testing.T) {
		x := seedState()
		x.s.Workflows["wf"] = &workflow.Report{ID: "wf", Name: "judges", Run: "run", Status: "running", Steps: []workflow.Step{{Operation: workflow.Operation{ID: "wf/b", Kind: "agent"}, Status: "running"}}}
		a := x.member("a", true, "ta", "ea")
		a.Controller = "wf"
		x.exec("ea", "a", "completed", "wf")
		x.task("ta", "a", "ea", "awaiting_review")
		b := x.member("b", true, "tb", "eb")
		b.Controller = "wf"
		x.exec("eb", "b", "running", "wf")
		x.task("tb", "b", "eb", "running")
		p := x.present()
		expectLists(t, p, "", "workflow:wf=running")
		if p.Working[0].Agents != 1 || p.Working[0].Label != "judges" {
			t.Fatalf("workflow entry = %+v", p.Working[0])
		}
	})
	t.Run("workflow parked versus a running tool step", func(t *testing.T) {
		x := seedState()
		x.s.Workflows["wf"] = &workflow.Report{ID: "wf", Run: "run", Status: "running", Steps: []workflow.Step{{Operation: workflow.Operation{ID: "wf/a", Kind: "agent"}, Status: "running"}}}
		a := x.member("a", true, "ta", "ea")
		a.Controller = "wf"
		x.exec("ea", "a", "waiting", "wf")
		x.task("ta", "a", "ea", "running")
		if got := workingIDs(x.present()); got != "workflow:wf=parked" {
			t.Fatalf("working = %q", got)
		}
		x.s.Workflows["wf"].Steps = append(x.s.Workflows["wf"].Steps, workflow.Step{Operation: workflow.Operation{ID: "wf/tool", Kind: "tool"}, Status: "running"})
		if got := workingIDs(x.present()); got != "workflow:wf=running" {
			t.Fatalf("working with a tool step = %q", got)
		}
	})
	t.Run("completed workflow with research and an editing task", func(t *testing.T) {
		x := seedState()
		x.s.Workflows["wf"] = &workflow.Report{ID: "wf", Run: "run", Status: "completed"}
		for _, id := range []string{"r1", "r2"} {
			x.member(id, true, "t"+id, "e"+id)
			x.exec("e"+id, id, "completed", "wf")
			x.task("t"+id, id, "e"+id, "awaiting_review")
		}
		x.member("ed", false, "te", "ee")
		x.exec("ee", "ed", "completed", "wf")
		x.task("te", "ed", "ee", "awaiting_review").Snapshot = "snap"
		p := x.present()
		expectLists(t, p, "task:te task:tr1 task:tr2", "")

	})
	t.Run("accepted unchanged task with and without an uncertain apply", func(t *testing.T) {
		s, task := unchangedTaskState()
		if !acceptedUnchangedTask(s, task) {
			t.Fatal("fixture is not an accepted unchanged task")
		}
		p := Present(s, "parent", "parent")
		expectLists(t, p, "", "")
		if p.Counts.Done != 1 {
			t.Fatalf("repairable task not counted as done: %+v", p.Counts)
		}
		s.Applies = map[string]*ApplyRecord{"apply": {ID: "apply", Status: "applying"}}
		p = Present(s, "parent", "parent")
		expectLists(t, p, "integration:apply", "task:task=accepted · settles after integration apply is reconciled")
		if p.Counts.Done != 0 || !strings.HasPrefix(p.Next, "integration apply: reconcile it with swarm_integration") {
			t.Fatalf("counts = %+v next = %q", p.Counts, p.Next)
		}
	})
	t.Run("budget exhausted with everything done", func(t *testing.T) {
		x := seedState()
		x.s.Runs["run"].Status, x.s.Runs["run"].Starts, x.s.Runs["run"].Limit = "paused", 1, 1
		x.task("T", "", "", "done")
		p := x.present()
		expectLists(t, p, "budget:run", "")
		if d := p.Decisions[0]; d.Why != "execution budget exhausted (1/1)" || !strings.Contains(d.Action, "/swarm grant") || p.Budget == nil || !p.Budget.Exhausted || p.Counts.Done != 1 {
			t.Fatalf("decision = %+v budget = %+v counts = %+v", d, p.Budget, p.Counts)
		}
	})
	t.Run("uncertain apply during a running workflow", func(t *testing.T) {
		x := seedState()
		x.s.Applies["apply"] = &ApplyRecord{ID: "apply", Status: "recovery_required"}
		x.s.Workflows["wf"] = &workflow.Report{ID: "wf", Run: "run", Status: "running"}
		p := x.present()
		expectLists(t, p, "integration:apply", "workflow:wf=running")
		if p.Next != `integration apply: reconcile it with swarm_integration {op: "reconcile", id: "apply"}` {
			t.Fatalf("next = %q", p.Next)
		}
	})
	t.Run("failed workflow after the tasks", func(t *testing.T) {
		x := seedState()
		x.s.Workflows["wf"] = &workflow.Report{ID: "wf", Run: "run", Status: "failed"}
		x.task("P", "", "", "pending")
		p := x.present()
		expectLists(t, p, "task:P workflow:wf", "")
		if d := p.Decisions[1]; d.Why != "failed" || !strings.Contains(d.Action, "defer: true") {
			t.Fatalf("failure decision = %+v", d)
		}
	})
	t.Run("counts and dormant members", func(t *testing.T) {
		x := seedState()
		x.member("idle", false, "T", "e")
		x.exec("e", "idle", "completed", "")
		x.task("T", "idle", "e", "done")
		x.member("fresh", false, "", "")
		p := x.present()
		if p.Counts.Dormant != 2 || p.Counts.Done != 1 || p.Counts.NeedsDecision != 0 || p.Next != "Nothing is outstanding; answer the user." {
			t.Fatalf("counts = %+v next = %q", p.Counts, p.Next)
		}
		if p := Present(&State{}, "parent", "parent"); p.Next != "No swarm work yet." || p.Budget != nil {
			t.Fatalf("empty swarm: %+v", p)
		}
	})
	t.Run("member view", func(t *testing.T) {
		x := seedState()
		x.member("self", false, "T", "e")
		x.exec("e", "self", "running", "").Iterations = 2
		x.task("T", "self", "e", "running")
		x.member("peer", false, "U", "f")
		x.exec("f", "peer", "waiting", "")
		x.task("U", "peer", "f", "running")
		x.member("done", false, "V", "g")
		x.exec("g", "done", "completed", "")
		x.task("V", "done", "g", "done")
		x.s.Messages["q1"] = &Mail{ID: "q1", From: "parent", To: "self", Kind: "request", Text: "status?", Delivered: true}
		x.s.Messages["q2"] = &Mail{ID: "q2", From: "peer", To: "self", Kind: "request", Text: "answered", ReplyID: "a2"}
		x.s.Messages["q3"] = &Mail{ID: "q3", From: "peer", To: "parent", Kind: "request", Text: "not mine"}
		p := Present(x.s, "self", "parent")
		expectLists(t, p, "mail:q1", "member:peer=waiting")
		if d := p.Decisions[0]; d.Action != `send_message {to: "parent", kind: "reply", reply_to: "q1"}` || p.Next != "Reply to the requests above, then continue your task." {
			t.Fatalf("decision = %+v next = %q", d, p.Next)
		}
		if p.Budget == nil || p.Budget.Unit != "iterations" || p.Budget.Used != 2 || p.Budget.Limit != 5 || p.Budget.Exhausted {
			t.Fatalf("budget = %+v", p.Budget)
		}
		x.s.Messages["q1"].ReplyID = "a1"
		if p := Present(x.s, "self", "parent"); len(p.Decisions) != 0 || !strings.HasPrefix(p.Next, "Continue your task") {
			t.Fatalf("answered request still listed: %+v", p)
		}
	})
	t.Run("deferred work is counted only", func(t *testing.T) {
		r, id, _ := failedResearchWorkflow(t)
		ctx := context.Background()
		if err := r.DeferWorkflow(ctx, id, "later"); err != nil {
			t.Fatal(err)
		}
		s, err := r.read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		p := Present(s, r.ID, r.ID)
		expectLists(t, p, "", "")
		if p.Counts.Deferred != 1 {
			t.Fatalf("counts = %+v", p.Counts)
		}
	})
	t.Run("iteration limit item", func(t *testing.T) {
		x := seedState()
		x.member("m", false, "T", "e")
		e := x.exec("e", "m", "paused", "")
		e.StopReason, e.Iterations = messages.StopReasonMaxIterations, 5
		x.task("T", "m", "e", "running")
		p := x.present()
		expectLists(t, p, "task:T", "")
		if d := p.Decisions[0]; d.Why != "paused at its iteration limit (5/5 model calls)" || d.Action != "tell the user; only a user-directed /swarm resume m N grants more calls" {
			t.Fatalf("decision = %+v", d)
		}
	})
}

func TestDecisionCountsAndFirstDecision(t *testing.T) {
	x := seedState()
	x.s.Workflows["wf"] = &workflow.Report{ID: "wf", Run: "run", Status: "completed"}
	x.member("r", true, "tr", "er")
	x.exec("er", "r", "completed", "wf")
	x.task("tr", "r", "er", "awaiting_review")
	x.member("m", false, "T", "e")
	x.exec("e", "m", "completed", "")
	x.task("T", "m", "e", "awaiting_review")
	x.s.Messages["ask"] = &Mail{ID: "ask", From: "m", To: "parent", Kind: "request", Text: "?"}
	byMember, byWorkflow := DecisionCounts(x.s, "parent")
	if byMember["m"] != 2 || byMember["r"] != 1 || byWorkflow["wf"] != 0 {
		t.Fatalf("byMember = %v byWorkflow = %v", byMember, byWorkflow)
	}
	first, ok := FirstDecision(x.s, "parent")
	if !ok || first.Kind != KindMail || first.Member != "m" {
		t.Fatalf("first = %+v", first)
	}
	if c := StatusCounts(x.s, "parent"); c.NeedsDecision != 3 {
		t.Fatalf("counts = %+v", c)
	}
}
