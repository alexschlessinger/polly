package coordsem

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Display values are owned, derived data, never mutation or settlement authority.
type DecisionItem struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Label  string `json:"label"`
	Why    string `json:"why"`
	Action string `json:"action"`
	Member string `json:"member,omitempty"`
	State  string `json:"state,omitempty"`
}
type WorkingItem struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Label  string `json:"label"`
	State  string `json:"state"`
	Task   string `json:"task,omitempty"`
	Agents int    `json:"agents,omitempty"`
}
type Counts struct {
	NeedsDecision int `json:"needsDecision"`
	Working       int `json:"working"`
	Done          int `json:"done"`
	Dormant       int `json:"dormant"`
	Delivering    int `json:"delivering,omitempty"`
	Retained      int `json:"retained,omitempty"`
	Deferred      int `json:"deferred,omitempty"`
	Publications  int `json:"publications,omitempty"`
}
type Budget struct {
	Unit      string `json:"unit"`
	Used      int    `json:"used"`
	Limit     int    `json:"limit"`
	Exhausted bool   `json:"exhausted"`
}
type TaskReference struct {
	Task     string `json:"task"`
	Revision int    `json:"revision"`
}

func Clip(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	end := limit
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end] + "…"
}
func IntegrateTasks(refs []TaskReference) string {
	b, _ := json.Marshal(struct {
		Tasks []TaskReference `json:"tasks,omitempty"`
	}{refs})
	return "swarm_integrate(" + string(b) + ")"
}
func IntegrateCandidate(id string) string {
	b, _ := json.Marshal(struct {
		Candidate string `json:"candidate,omitempty"`
	}{id})
	return "swarm_integrate(" + string(b) + fmt.Sprintf("); if the parent changed, first refresh through workflow_run with polly.integration.refresh(%q)", id)
}
func CompletionGuidance(requirement string) string {
	switch requirement {
	case "delivered":
		return "End your turn with the result; it completes when durably delivered to the parent or workflow."
	case "reviewed":
		return "Return your result for explicit parent review of this task's submitted revision."
	default:
		return "Return your changes and result; the parent completes the task by integrating the candidate."
	}
}
func WorkflowNext(w Workflow) string {
	switch {
	case w.Status == "running":
		return "Running: continue your own work if you have any; results reach you at each step. Park with wait_agent when you have nothing else to do; do not poll. It wakes you once when the workflow finishes or when mail addresses you."
	case w.Acknowledged:
		return ""
	case w.Status == "completed":
		return "Completed; its output is delivered with the notice. swarm_read({view: \"workflows\", id: \"" + w.ID + "\", section: \"output\"}) shows it again; no acknowledgment is needed."
	default:
		return "Terminal " + w.Status + ": inspect steps and agents, then either recover its work or report the failure and swarm_control({action: \"acknowledge_workflow\", id: \"" + w.ID + "\", defer: true, note: \"...\"}) to retain unresolved work for later. Deferral does not accept, apply, or cancel work."
	}
}
func DeliveryNext(n int) string {
	if n == 1 {
		return "1 result awaits delivery; its completion notice accompanies this prompt: read it, then answer"
	}
	text := fmt.Sprintf("%d results await delivery; their completion notices accompany this prompt: read them, then answer", n)
	if n > 16 {
		text += "; more follow: park with wait_agent until none remain"
	}
	return text
}

const WorkingNext = "Continue work you already have — results reach you automatically at each step. Park with wait_agent when you have nothing else to do; inspect decisions with swarm_read."

func ParentNext(first *DecisionItem, refs []TaskReference, deliveries, working int, hasRuns bool) string {
	if first != nil {
		if first.Kind == "task" && first.Why == "candidate ready" && len(refs) > 1 {
			return "candidates ready: integrate them in one call: " + IntegrateTasks(refs) + " or request changes with swarm_review"
		}
		return first.Label + ": " + first.Action
	}
	if deliveries > 0 {
		return DeliveryNext(deliveries)
	}
	if working > 0 {
		return WorkingNext
	}
	if hasRuns {
		return "Nothing is outstanding; answer the user."
	}
	return "No swarm work yet."
}

// TaskDecisionInputs separates relationship resolution from wording and
// precedence. CandidateAction is the selected candidate's pure halt/ready text.
type TaskDecisionInputs struct {
	Task                                                                              Task
	Execution                                                                         *Execution
	Owner                                                                             *Member
	OwnerDisplay                                                                      string
	Delivering, Editing, Provenance                                                   bool
	CandidateID, CandidateStatus, CandidateAction, DependencyReason, RetryDescription string
	UnfinishedDependencies                                                            int
}

func TaskReason(f TaskDecisionInputs) (string, string) {
	t := f.Task
	if f.Delivering {
		return "result awaiting delivery", "the runtime re-posts its notice at settlement"
	}
	switch t.Status {
	case "awaiting_review":
		if f.Editing {
			if f.CandidateID != "" {
				if f.CandidateStatus == "conflicted" {
					return "integration " + f.CandidateID + " halted · conflicted", f.CandidateAction
				}
				return "candidate " + f.CandidateID + " ready", f.CandidateAction
			}
			if Accepted(&t) {
				if !f.Provenance {
					return "accepted, but captured commit provenance is unavailable", "cancel the task with swarm_control and start a new assignment"
				}
				return "accepted", IntegrateTasks([]TaskReference{{t.ID, t.Revision}})
			}
			return "candidate ready", IntegrateTasks([]TaskReference{{t.ID, t.Revision}}) + " or request changes with swarm_review"
		}
		return "awaiting parent review", "accept this revision or request changes"
	case "pending":
		return "pending", "resolve its dependencies, then assign and run the task or cancel it"
	case "blocked":
		return "blocked", "resolve its blocker and use followup_task with the owner target, or cancel it with swarm_control"
	case "changes_requested":
		return "changes requested", "use followup_task with the owner target and revision instructions, reassign through workflow_run, or cancel the task"
	}
	if e := f.Execution; e != nil {
		return fmt.Sprintf("execution %s is %s", e.ID, e.Status), fmt.Sprintf("inspect its saved result and use followup_task with target %q, or cancel the task with swarm_control", e.Member)
	}
	return "", "inspect the task and use followup_task with its owner target, or cancel it with swarm_control"
}
func TaskDecision(f TaskDecisionInputs) DecisionItem {
	t, e := f.Task, f.Execution
	item := DecisionItem{Kind: "task", ID: t.ID, Label: fmt.Sprintf("task %s revision %d", t.ID, t.Revision), Member: t.Owner, State: f.OwnerDisplay}
	switch {
	case e != nil && e.Status == "paused" && e.RetryPause:
		item.Why = f.RetryDescription
		item.Action = fmt.Sprintf("continue explicitly with followup_task %s once eligible; do not replay the original request", e.Member)
	case e != nil && e.Status == "paused" && e.StopReason == "max_iterations":
		item.Why = fmt.Sprintf("paused at its iteration limit (%d/%d model calls)", e.Iterations, e.MaxIterations)
		item.Action = fmt.Sprintf("tell the user; only a user-directed /swarm resume %s N grants more calls", e.Member)
	case t.Status == "pending" && f.DependencyReason != "":
		item.Why = "pending"
		item.Action = fmt.Sprintf("dependency %s is canceled; update its dependencies or cancel it", f.DependencyReason)
	case t.Status == "pending" && f.UnfinishedDependencies == 0:
		item.Why, item.Action = "pending", "assign and run the task or cancel it"
	default:
		item.Why, item.Action = TaskReason(f)
	}
	switch {
	case t.Owner != "" && f.Owner == nil:
		item.Action = "reassign it through workflow_run with polly.tasks.update or cancel it with swarm_control"
	case f.Owner != nil && f.Owner.Control == "stopped":
		if e != nil && e.RetryPause {
			item.Action = "tell the user; only an explicit user resume can restart this stopped member"
		} else {
			item.Action += ", or followup_task " + f.Owner.ID
		}
	}
	return item
}
func IntegrationHalt(id, commit string, paths []string) string {
	return fmt.Sprintf("integration %s halted: conflicts in %s (inspect through workflow_run with polly.integration.read(%q)); resolve them with an editing repair task from commit %s using workflow_run and polly.agent({label: \"Resolve conflicts\", commit: %q, task: <repair brief>}), then polly.integration.revise(%q, {task, revision}) in that workflow, or request changes on a task with swarm_review", id, Clip(strings.Join(paths, ", "), 160), id, commit, commit, id)
}
