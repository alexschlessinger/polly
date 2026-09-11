package swarm

import (
	"github.com/alexschlessinger/pollytool/workflow"
)

// Kinds of things only the parent can settle.
const (
	KindIntegration = "integration"
	KindMail        = "mail"
	KindDelivery    = "delivery"
	KindWorkflow    = "workflow"
	KindBudget      = "budget"
	KindTask        = "task"
)

// coordinationFacts is everything settlement and the decision presentation
// read, derived once per state. Settlement consumes the complete obligation sets
// in Settle's order; presentation groups and folds them. Neither feeds the
// other, so a display rule can never change a settlement outcome.
type coordinationFacts struct {
	s     *State
	actor string
	// run is the running or paused run, lowest ID first; nil when none.
	run *Run
	// applies are uncertain integration receipts, sorted by ID.
	applies []*ApplyRecord
	// requests are addressed requests the actor has not answered, delivered
	// or not; replies are answers the actor has not read yet.
	requests []*Mail
	replies  []*Mail
	outputs  []*workflow.Report
	// failed lists the run's terminal, unacknowledged failures, sorted by ID.
	failed []*workflow.Report
	// budget is a paused run whose work is not entirely deferred.
	budget *Run
	// tasks is unsettledTasks in its order, complete.
	tasks []taskFact
	// running is Settle's gate: model or host work that can still change the
	// answer. uncertainApply is settlementState's repair guard.
	running        bool
	uncertainApply bool
}

// taskFact is one unsettled task with the facts presentation classifies on.
type taskFact struct {
	task      *Task
	owner     *Member
	execution *Execution
	// assigned is the exact current assignment: the owner's current execution
	// is the one recorded on the task. A member that submitted one task and
	// claimed another advances only the second.
	assigned bool
	// advancing: an assigned running task whose execution is queued or running.
	advancing bool
	// parked: an assigned task whose execution waits for addressed input.
	parked bool
	// controlled names the running workflow that owns the execution.
	controlled string
	delivering bool
	// unchanged: an accepted, unchanged submission settlement repairs to done.
	unchanged bool
	// deps are the unfinished dependencies.
	deps []depFact
}

type depFact struct {
	id string
	// canceled or missing: the dependency can never become done.
	canceled bool
}

// activeRun is the run settlement judges: running or paused, lowest ID.
func activeRun(s *State) *Run {
	var run *Run
	for _, candidate := range s.Runs {
		if candidate.Status != "running" && candidate.Status != "paused" {
			continue
		}
		if run == nil || candidate.ID < run.ID {
			run = candidate
		}
	}
	return run
}

// workRunning is Settle's gate. Agent host calls can be parked awaiting the
// parent; other host operations and startup still have work to settle.
func workRunning(s *State) bool {
	for _, w := range s.Workflows {
		if w.Status != "running" {
			continue
		}
		if len(w.Steps) == 0 {
			return true
		}
		for _, step := range w.Steps {
			if step.Status == "running" && step.Kind != "agent" && step.Kind != "followup" {
				return true
			}
		}
	}
	for _, e := range s.Executions {
		if e.Status == "running" || e.Status == "queued" {
			return true
		}
	}
	return false
}

func deriveFacts(s *State, actor string) *coordinationFacts {
	f := &coordinationFacts{s: s, actor: actor, run: activeRun(s), running: workRunning(s)}
	for _, id := range sortedInspectionIDs(s.Applies) {
		if a := s.Applies[id]; a.Status == "applying" || a.Status == "recovery_required" {
			f.applies = append(f.applies, a)
		}
	}
	f.uncertainApply = len(f.applies) > 0
	for _, m := range inbox(s, actor, false) {
		switch {
		case m.Kind == "request" && m.ReplyID == "":
			f.requests = append(f.requests, m)
		case m.Kind == "reply" && !m.Delivered:
			f.replies = append(f.replies, m)
		}
	}
	run := ""
	if f.run != nil {
		run = f.run.ID
	}
	for _, id := range sortedInspectionIDs(s.Workflows) {
		w := s.Workflows[id]
		if w.Status == "completed" && !w.Acknowledged {
			for _, mail := range inbox(s, actor, true) {
				if mail.Workflow == id {
					f.outputs = append(f.outputs, w)
					break
				}
			}
		}
		if w := s.Workflows[id]; w.Run == run && w.Status != "running" && w.Status != "completed" && !w.Acknowledged {
			f.failed = append(f.failed, w)
		}
	}
	for _, id := range sortedInspectionIDs(s.Runs) {
		if candidate := s.Runs[id]; candidate.Status == "paused" && !runDeferred(s, candidate.ID) {
			f.budget = candidate
			break
		}
	}
	for _, task := range unsettledTasks(s, run) {
		f.tasks = append(f.tasks, taskFactOf(s, task))
	}
	return f
}

func taskFactOf(s *State, task *Task) taskFact {
	tf := taskFact{task: task, delivering: deliveringTask(s, task) && resultNotice(s, task) != nil && !TaskDeferred(s, task), owner: s.Members[task.Owner], execution: s.Executions[task.Execution]}
	tf.assigned = tf.owner != nil && tf.execution != nil && task.Execution != "" && tf.owner.Task == task.ID && tf.owner.Execution == task.Execution
	if tf.assigned {
		switch tf.execution.Status {
		case "queued", "running":
			tf.advancing = task.Status == "running"
		case "waiting":
			tf.parked = true
		}
	}
	if workflowControlled(s, tf.execution) {
		tf.controlled = tf.execution.Workflow
	}
	tf.unchanged = acceptedUnchangedTask(s, task)
	for _, id := range task.Dependencies {
		dep := s.Tasks[id]
		if dep != nil && dep.Status == "done" {
			continue
		}
		tf.deps = append(tf.deps, depFact{id: id, canceled: dep == nil || dep.Status == "canceled"})
	}
	return tf
}

// blocker is one settlement obstacle with the exact error Settle reports.
type blocker struct {
	kind string
	err  error
	task *Task
}

// settlementBlockers is the complete blocker set in Settle's order: uncertain
// integrations, unanswered parent requests or unread replies, pending delivery,
// an exhausted budget, every unsettled task, then
// unacknowledged failures. No presentation rule applies here.
func settlementBlockers(f *coordinationFacts) []blocker {
	var out []blocker
	for _, a := range f.applies {
		out = append(out, blocker{kind: KindIntegration, err: fail("recovery_required", "integration "+a.ID+" has an unconfirmed outcome")})
	}
	if len(f.requests) > 0 || len(f.replies) > 0 {
		out = append(out, blocker{kind: KindMail, err: fail("blocked", "a member is waiting for a parent reply")})
	}
	if n := deliveryCount(f); n > 0 {
		out = append(out, blocker{kind: KindDelivery, err: fail("blocked", deliveryNext(n))})
	}
	if f.budget != nil {
		out = append(out, blocker{kind: KindBudget, err: ErrBudget})
	}
	for _, tf := range f.tasks {
		if tf.delivering {
			continue
		}
		out = append(out, blocker{kind: KindTask, err: taskSettlementError(f.s, tf.task), task: tf.task})
	}
	for _, w := range f.failed {
		out = append(out, blocker{kind: KindWorkflow, err: fail("blocked", "workflow "+w.ID+" "+w.Status+"; inspect its report and explicitly acknowledge the failure after arranging recovery or reporting the blocker")})
	}
	return out
}

// blockerError is Settle's error for a blocker set: the first blocker, or
// for tasks the first task's blocker carrying the open-task count.
func blockerError(f *coordinationFacts, blockers []blocker) error {
	if len(blockers) == 0 {
		return nil
	}
	if blockers[0].kind != KindTask {
		return blockers[0].err
	}
	var tasks []*Task
	for _, b := range blockers {
		if b.kind == KindTask {
			tasks = append(tasks, b.task)
		}
	}
	return unsettledTasksError(f.s, tasks)
}
