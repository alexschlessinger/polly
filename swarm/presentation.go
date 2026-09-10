package swarm

import (
	"fmt"
	"strings"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/workflow"
)

// DecisionItem is one thing only the parent can settle, with its next action.
type DecisionItem struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Label  string `json:"label"`
	Why    string `json:"why"`
	Action string `json:"action"`
	// Member is the task owner or the mail sender; State is its display.
	Member string `json:"member,omitempty"`
	State  string `json:"state,omitempty"`
}

// WorkingItem is work that can still change the answer without the parent.
type WorkingItem struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Label  string `json:"label"`
	State  string `json:"state"`
	Task   string `json:"task,omitempty"`
	Agents int    `json:"agents,omitempty"`
}

// Counts are totals over the whole swarm, never over a truncated page.
type Counts struct {
	NeedsDecision int `json:"needsDecision"`
	Working       int `json:"working"`
	Done          int `json:"done"`
	Dormant       int `json:"dormant"`
	Delivering    int `json:"delivering,omitempty"`
	Retained      int `json:"retained,omitempty"`
	Retired       int `json:"retired,omitempty"`
	Deferred      int `json:"deferred,omitempty"`
}

// Budget is the allowance that bounds the view's actor: the run's logical
// starts for the parent, the execution's model calls for a member.
type Budget struct {
	Unit      string `json:"unit"`
	Used      int    `json:"used"`
	Limit     int    `json:"limit"`
	Exhausted bool   `json:"exhausted"`
}

// Presentation is the swarm as its parent or a member should act on it:
// decisions in settlement order, each with one action; work that is still
// moving; totals; and the first thing to do.
type Presentation struct {
	Decisions []DecisionItem
	Working   []WorkingItem
	Counts    Counts
	Budget    *Budget
	Next      string
}

const presentationLabelBytes = 160

// Present derives the actor's view. The parent sees every decision; a member
// sees the requests awaiting its reply and its teammates.
func Present(s *State, actor, parent string) Presentation {
	return presentation(deriveFacts(s, actor), parent)
}

// StatusCounts are the parent view's totals without building its lists.
func StatusCounts(s *State, parent string) Counts {
	return Present(s, parent, parent).Counts
}

// DecisionCounts keys the parent's decisions by the member they concern and
// by workflow, for surfaces that annotate rows and headings.
func DecisionCounts(s *State, parent string) (byMember, byWorkflow map[string]int) {
	byMember, byWorkflow = map[string]int{}, map[string]int{}
	for _, item := range Present(s, parent, parent).Decisions {
		switch item.Kind {
		case KindWorkflow:
			byWorkflow[item.ID]++
		case KindTask, KindMail:
			if item.Member != "" {
				byMember[item.Member]++
			}
		}
	}
	return byMember, byWorkflow
}

// FirstDecision is the decision the parent should take first, when any.
func FirstDecision(s *State, parent string) (DecisionItem, bool) {
	items := Present(s, parent, parent).Decisions
	if len(items) == 0 {
		return DecisionItem{}, false
	}
	return items[0], true
}

func presentation(f *coordinationFacts, parent string) Presentation {
	if f.actor != parent {
		return memberPresentation(f)
	}
	s := f.s
	var p Presentation
	for _, a := range f.applies {
		p.Decisions = append(p.Decisions, DecisionItem{Kind: KindIntegration, ID: a.ID, Label: "integration " + a.ID, Why: "outcome unconfirmed", Action: fmt.Sprintf("reconcile it with swarm_integration {op: %q, id: %q}", "reconcile", a.ID)})
	}
	for _, m := range f.requests {
		p.Decisions = append(p.Decisions, DecisionItem{Kind: KindMail, ID: m.ID, Label: "request " + m.ID + " from " + m.From, Why: clipInspection(m.Text, presentationLabelBytes), Action: fmt.Sprintf("send_message {to: %q, kind: %q, reply_to: %q}", m.From, "reply", m.ID), Member: m.From, State: memberDisplay(s, m.From)})
	}
	for _, m := range f.replies {
		p.Decisions = append(p.Decisions, DecisionItem{Kind: KindMail, ID: m.ID, Label: "reply " + m.ID + " from " + m.From, Why: "unread answer to your request", Action: "read_messages", Member: m.From, State: memberDisplay(s, m.From)})
	}
	for _, research := range f.research {
		w := research.report
		p.Decisions = append(p.Decisions, DecisionItem{Kind: KindWorkflow, ID: w.ID, Label: "workflow " + w.ID, Why: fmt.Sprintf("completed with %s awaiting review", countNoun(len(research.tasks), "research result")), Action: workflowNext(s, w)})
	}
	if f.budget != nil {
		p.Decisions = append(p.Decisions, DecisionItem{Kind: KindBudget, ID: f.budget.ID, Label: "run " + f.budget.ID, Why: fmt.Sprintf("execution budget exhausted (%d/%d)", f.budget.Starts, f.budget.Limit), Action: "tell the user; only a user-directed /swarm grant N extends it"})
	}
	c := newClassifier(f)
	decisions, taskWork, repaired := c.items()
	p.Decisions = append(p.Decisions, decisions...)
	for _, w := range f.failed {
		p.Decisions = append(p.Decisions, DecisionItem{Kind: KindWorkflow, ID: w.ID, Label: "workflow " + w.ID, Why: w.Status, Action: workflowNext(s, w)})
	}
	p.Working = append(p.Working, workflowWork(s)...)
	p.Working = append(p.Working, memberWork(f, "", false)...)
	p.Working = append(p.Working, taskWork...)
	p.Counts = counts(f, len(p.Decisions), len(p.Working), repaired)
	if f.run != nil {
		p.Budget = &Budget{Unit: "starts", Used: f.run.Starts, Limit: f.run.Limit, Exhausted: f.run.Starts >= f.run.Limit}
	}
	switch {
	case len(p.Decisions) > 0:
		p.Next = p.Decisions[0].Label + ": " + p.Decisions[0].Action
	case len(p.Working) > 0:
		p.Next = "Park with swarm_wait; it returns this status when a decision is due or nothing is active."
	case len(s.Runs) > 0:
		p.Next = "Nothing is outstanding; answer the user."
	default:
		p.Next = "No swarm work yet."
	}
	return p
}

func memberPresentation(f *coordinationFacts) Presentation {
	s := f.s
	var p Presentation
	for _, m := range f.requests {
		p.Decisions = append(p.Decisions, DecisionItem{Kind: KindMail, ID: m.ID, Label: "request " + m.ID + " from " + m.From, Why: clipInspection(m.Text, presentationLabelBytes), Action: fmt.Sprintf("send_message {to: %q, kind: %q, reply_to: %q}", m.From, "reply", m.ID), Member: m.From, State: memberDisplay(s, m.From)})
	}
	p.Working = memberWork(f, f.actor, true)
	c := newClassifier(f)
	_, _, repaired := c.items()
	p.Counts = counts(f, len(p.Decisions), len(p.Working), repaired)
	if m := s.Members[f.actor]; m != nil {
		if e := s.Executions[m.Execution]; e != nil {
			p.Budget = &Budget{Unit: "iterations", Used: e.Iterations, Limit: e.Request.MaxIterations, Exhausted: e.Request.MaxIterations > 0 && e.Iterations >= e.Request.MaxIterations}
		}
	}
	if len(p.Decisions) > 0 {
		p.Next = "Reply to the requests above, then continue your task."
	} else {
		p.Next = "Continue your task; swarm_submit when done, swarm_wait to park until addressed input."
	}
	return p
}

func memberDisplay(s *State, id string) string {
	if m := s.Members[id]; m != nil {
		return MemberState(s, m).Display
	}
	return ""
}

func memberLabel(m *Member) string {
	if m.Label != "" {
		return clipInspection(m.Label, presentationLabelBytes)
	}
	return m.Name
}

// workflowWork lists running workflows, folding the agents they own.
func workflowWork(s *State) []WorkingItem {
	var out []WorkingItem
	for _, id := range sortedInspectionIDs(s.Workflows) {
		w := s.Workflows[id]
		if w.Status != "running" {
			continue
		}
		agents, waiting := 0, 0
		for _, e := range s.Executions {
			if ExecutionWorkflow(s, e) != w.ID {
				continue
			}
			switch e.Status {
			case "queued", "running":
				agents++
			case "waiting":
				agents++
				waiting++
			}
		}
		state := "running"
		if agents > 0 && waiting == agents && !workflowStepRunning(w) {
			state = "parked"
		}
		label := clipInspection(w.Name, presentationLabelBytes)
		if label == "" {
			label = w.ID
		}
		out = append(out, WorkingItem{Kind: KindWorkflow, ID: w.ID, Label: label, State: state, Agents: agents})
	}
	return out
}

// workflowStepRunning reports a running step that is not an agent await:
// the same host work Settle's gate waits for.
func workflowStepRunning(w *workflow.Report) bool {
	for _, step := range w.Steps {
		if step.Status == "running" && step.Kind != "agent" {
			return true
		}
	}
	return false
}

// memberWork lists busy members. The parent view skips members a running
// workflow owns (its entry covers them), members whose parked execution waits
// on the parent's own reply (the request item covers them), and members
// parked while nothing else moves (their task is a decision); a member's view
// lists every busy teammate except itself.
func memberWork(f *coordinationFacts, except string, teammates bool) []WorkingItem {
	s := f.s
	asking := map[string]bool{}
	for _, m := range f.requests {
		asking[m.From] = true
	}
	var out []WorkingItem
	for _, id := range sortedInspectionIDs(s.Members) {
		m := s.Members[id]
		if id == except {
			continue
		}
		state := MemberState(s, m)
		if !state.Busy {
			continue
		}
		if !teammates && (memberControlled(s, m) || state.Lifecycle == LifecycleWaiting && (asking[m.ID] || !f.running)) {
			continue
		}
		out = append(out, WorkingItem{Kind: "member", ID: m.ID, Label: memberLabel(m), State: state.Display, Task: m.Task})
	}
	return out
}

func counts(f *coordinationFacts, decisions, working, repaired int) Counts {
	s := f.s
	c := Counts{NeedsDecision: decisions, Working: working, Done: repaired}
	run := ""
	if f.run != nil {
		run = f.run.ID
	}
	for _, t := range s.Tasks {
		switch {
		case t.Status == "done" && t.Run == run:
			c.Done++
		case TaskDeferred(s, t):
			c.Deferred++
		}
	}
	for _, m := range s.Members {
		if m.Control == MemberControlRetired {
			c.Retired++
			continue
		}
		if p := MemberState(s, m); m.Control == MemberControlEnabled && !p.Busy && !p.Attention {
			c.Dormant++
		}
	}
	return c
}

// taskBucket is where presentation files one unsettled task.
type taskBucket int

const (
	bucketDecision       taskBucket = iota // listed as its own decision
	bucketFoldedDecision                   // covered by another decision item
	bucketWorking                          // listed as task-only working entry
	bucketFoldedWorking                    // covered by a member or workflow entry
	bucketDone                             // settlement will complete it
)

type classifier struct {
	f        *coordinationFacts
	byID     map[string]taskFact
	memo     map[string]taskBucket
	reason   map[string]string
	visiting map[string]bool
	asking   map[string]bool
}

func newClassifier(f *coordinationFacts) *classifier {
	c := &classifier{f: f, byID: map[string]taskFact{}, memo: map[string]taskBucket{}, reason: map[string]string{}, visiting: map[string]bool{}, asking: map[string]bool{}}
	for _, tf := range f.tasks {
		c.byID[tf.task.ID] = tf
	}
	for _, m := range f.requests {
		c.asking[m.From] = true
	}
	return c
}

// items classifies every unsettled task, in settlement order, into decision
// items, task-only working entries and the count settlement will complete.
func (c *classifier) items() (decisions []DecisionItem, working []WorkingItem, repaired int) {
	for _, tf := range c.f.tasks {
		switch c.bucket(tf.task.ID) {
		case bucketDecision:
			decisions = append(decisions, c.decision(tf))
		case bucketWorking:
			working = append(working, WorkingItem{Kind: KindTask, ID: tf.task.ID, Label: "task " + tf.task.ID, State: c.reason[tf.task.ID], Task: tf.task.ID})
		case bucketDone:
			repaired++
		}
	}
	return decisions, working, repaired
}

func (c *classifier) bucket(id string) taskBucket {
	if b, ok := c.memo[id]; ok {
		return b
	}
	if c.visiting[id] {
		return bucketDecision
	}
	c.visiting[id] = true
	defer delete(c.visiting, id)
	b := c.compute(c.byID[id])
	c.memo[id] = b
	return b
}

func (c *classifier) compute(tf taskFact) taskBucket {
	f := c.f
	switch {
	case tf.covered != "":
		return bucketFoldedDecision
	case tf.controlled != "":
		return bucketFoldedWorking
	case tf.unchanged && f.uncertainApply:
		c.reason[tf.task.ID] = "accepted · settles after integration " + f.applies[0].ID + " is reconciled"
		return bucketWorking
	case tf.unchanged:
		return bucketDone
	case tf.advancing:
		return bucketFoldedWorking
	case tf.parked && c.asking[tf.owner.ID]:
		return bucketFoldedDecision
	case tf.parked && f.running:
		return bucketFoldedWorking
	case tf.parked:
		return bucketDecision
	case tf.task.Status == "pending":
		for _, dep := range tf.deps {
			if dep.canceled {
				c.reason[tf.task.ID] = dep.id
				return bucketDecision
			}
		}
		if len(tf.deps) == 0 {
			return bucketDecision
		}
		var ids []string
		for _, dep := range tf.deps {
			if _, open := c.byID[dep.id]; !open {
				// Deferred, or outside the run: nothing is moving it.
				return bucketDecision
			}
			c.bucket(dep.id)
			ids = append(ids, dep.id)
		}
		c.reason[tf.task.ID] = "pending · waiting on " + strings.Join(ids, ", ")
		return bucketWorking
	}
	return bucketDecision
}

func (c *classifier) decision(tf taskFact) DecisionItem {
	s := c.f.s
	task := tf.task
	item := DecisionItem{Kind: KindTask, ID: task.ID, Label: fmt.Sprintf("task %s revision %d", task.ID, task.Revision), Member: task.Owner, State: memberDisplay(s, task.Owner)}
	switch {
	case tf.execution != nil && tf.execution.Status == "paused" && tf.execution.StopReason == messages.StopReasonMaxIterations:
		item.Why = fmt.Sprintf("paused at its iteration limit (%d/%d model calls)", tf.execution.Iterations, tf.execution.Request.MaxIterations)
		item.Action = fmt.Sprintf("tell the user; only a user-directed /swarm resume %s N grants more calls", tf.execution.Member)
	case task.Status == "pending" && c.reason[task.ID] != "":
		item.Why = "pending"
		item.Action = fmt.Sprintf("dependency %s is canceled; update its dependencies or cancel it", c.reason[task.ID])
	case task.Status == "pending" && len(tf.deps) == 0:
		item.Why = "pending"
		item.Action = "assign and run the task or cancel it"
	default:
		item.Why, item.Action = taskDisposition(s, task)
	}
	switch {
	case task.Owner != "" && tf.owner == nil, tf.owner != nil && tf.owner.Control == MemberControlRetired:
		item.Action = "reassign it with swarm_update_task or cancel it"
	case tf.owner != nil && tf.owner.Control == MemberControlStopped:
		item.Action += ", or swarm_control resume " + tf.owner.ID
	}
	return item
}
