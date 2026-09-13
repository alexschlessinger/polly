package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/alexschlessinger/pollytool/workflow"
)

// IntegrateRequest selects exact submitted revisions or a retained candidate.
// Drift applies only when preparing tasks; a candidate keeps its recorded policy.
type IntegrateRequest struct {
	Tasks     []TaskReference `json:"tasks,omitempty"`
	Candidate string          `json:"candidate,omitempty"`
	Drift     string          `json:"drift,omitempty"`
}

type IntegrationOutcome struct {
	Status    string          `json:"status"`
	Candidate string          `json:"candidate,omitempty"`
	Tasks     []TaskReference `json:"tasks"`
	Unchanged []string        `json:"unchanged,omitempty"`
	Receipt   *ApplyRecord    `json:"receipt,omitempty"`
	Next      string          `json:"next"`
}

const integrationDone = "Tasks are done; their workspaces are released automatically. Answer the user or continue."

// Integrate accepts and applies under one gate and task lock. Durable candidates
// and receipts retain the recovery boundaries of the stepwise integration API.
func (r *Runtime) Integrate(ctx context.Context, req IntegrateRequest) (*IntegrationOutcome, error) {
	if (len(req.Tasks) == 0) == (req.Candidate == "") {
		return nil, fail("invalid_args", "name task revisions or a candidate")
	}
	if req.Candidate != "" {
		if req.Drift != "" {
			return nil, fail("invalid_args", "drift belongs to task preparation; omit it when naming a candidate")
		}
	} else {
		if req.Drift == "" {
			req.Drift = "paths"
		}
		if req.Drift != "paths" && req.Drift != "tree" {
			return nil, fail("invalid_args", "drift must be paths or tree")
		}
		seen := map[string]bool{}
		for _, ref := range req.Tasks {
			if strings.TrimSpace(ref.Task) == "" || ref.Revision <= 0 || seen[ref.Task] {
				return nil, fail("invalid_args", "task references must be unique, nonempty, and name positive revisions")
			}
			seen[ref.Task] = true
		}
	}
	var result *IntegrationOutcome
	err := r.withApplyLock(ctx, func(ctx context.Context) error {
		s, err := r.read(ctx)
		if err != nil {
			return err
		}
		if req.Candidate != "" {
			result, err = r.integrateCandidateLocked(ctx, s, req.Candidate)
		} else {
			result, err = r.integrateTasksLocked(ctx, s, req)
		}
		return err
	})
	return result, err
}

func editingTask(s *State, t *Task) bool {
	return requirementOf(s, t) == RequirementApplied || s.Members[t.Owner] != nil && !s.Members[t.Owner].ReadOnly
}

func integrateRefusal(s *State, ref TaskReference) error {
	t := s.Tasks[ref.Task]
	if t == nil {
		return fail("unknown_task", "unknown task "+ref.Task)
	}
	switch requirementOf(s, t) {
	case RequirementDelivered:
		return fail("invalid_args", fmt.Sprintf("task %s completes on delivery; nothing to integrate", t.ID))
	case RequirementReviewed:
		return fail("invalid_args", fmt.Sprintf("task %s is research under review; accept it with swarm_review or request changes", t.ID))
	}
	if t.Revision != ref.Revision || t.Status != "awaiting_review" && t.Status != "done" {
		return fail("stale_task", fmt.Sprintf("task %s is %s at revision %d; integrate the current submitted revision", t.ID, TaskStatus(t), t.Revision))
	}
	if t.Status == "done" {
		if acceptedTaskRevision(t) && unchangedTask(s, t) {
			return nil
		}
		return fail("stale_task", fmt.Sprintf("task %s is already done at revision %d; read or retry its applied candidate for the receipt", t.ID, t.Revision))
	}
	if t.Snapshot == "" {
		return fail("stale_task", fmt.Sprintf("task %s has no submitted commit; use followup_task with its member or cancel it", t.ID))
	}
	return nil
}

func integrationWriteGuard(s *State) error {
	// Stable selection also makes the repair guidance agree across retries.
	var uncertain []string
	for id, receipt := range s.Applies {
		if receipt.Status == "applying" || receipt.Status == "recovery_required" {
			uncertain = append(uncertain, id)
		}
	}
	if len(uncertain) == 0 {
		return nil
	}
	sort.Strings(uncertain)
	id := uncertain[0]
	return fail("recovery_required", fmt.Sprintf("integration %s has an unconfirmed outcome; use workflow_run with polly.integration.reconcile(%q) to observe its outcome first", id, id))
}

func (r *Runtime) integrateTasksLocked(ctx context.Context, s *State, req IntegrateRequest) (*IntegrationOutcome, error) {
	allDone := true
	unchanged := []string{}
	for _, ref := range req.Tasks {
		if err := integrateRefusal(s, ref); err != nil {
			return nil, err
		}
		t := s.Tasks[ref.Task]
		allDone = allDone && t.Status == "done"
		if unchangedTask(s, t) {
			unchanged = append(unchanged, ref.Task)
		}
	}
	out := &IntegrationOutcome{Status: "done", Tasks: req.Tasks, Unchanged: unchanged, Next: integrationDone}
	// Completed unchanged tasks can be replayed after their run and resources
	// end. This is read-only, including while an unrelated write is uncertain.
	if allDone {
		return out, nil
	}
	if err := integrationWriteGuard(s); err != nil {
		return nil, err
	}
	inputs, run, err := integrationInputs(s, req.Tasks)
	if err != nil {
		return nil, err
	}
	if len(unchanged) == len(req.Tasks) {
		err = r.update(ctx, func(current *State) error {
			for _, ref := range req.Tasks {
				if err := integrateRefusal(current, ref); err != nil {
					return err
				}
				if !unchangedTask(current, current.Tasks[ref.Task]) {
					return fail("stale_task", "unchanged task provenance is unavailable")
				}
			}
			for _, ref := range req.Tasks {
				t := current.Tasks[ref.Task]
				if t.Status != "done" {
					if err := acceptTask(current, t); err != nil {
						return err
					}
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		r.scheduleRelease()
		return out, nil
	}
	c, err := r.prepareCandidateLocked(ctx, inputs, run, req.Drift)
	if err != nil {
		return nil, err
	}
	if err = r.saveAcceptedCandidate(ctx, c); err != nil {
		return nil, err
	}
	r.scheduleRelease()
	if c.Status != "ready" {
		return nil, integrationHalt(c)
	}
	return r.integrateApplyLocked(ctx, c)
}

func (r *Runtime) saveAcceptedCandidate(ctx context.Context, c *IntegrationCandidate) error {
	return r.update(ctx, func(s *State) error {
		if err := validCandidate(s, c); err != nil {
			return err
		}
		for _, ref := range c.references() {
			if err := acceptTask(s, s.Tasks[ref.Task]); err != nil {
				return err
			}
		}
		c.Accepted = c.Status == "ready"
		return storeCandidate(s, c)
	})
}

func (r *Runtime) integrateCandidateLocked(ctx context.Context, s *State, id string) (*IntegrationOutcome, error) {
	c := s.Integrations[id]
	if c == nil {
		return nil, fail("unknown_candidate", "prepare a candidate with task revision provenance first")
	}
	if c.Successor != "" || c.Status == "superseded" {
		return nil, fail("superseded", "candidate was superseded by "+c.Successor)
	}
	// validCandidate intentionally refuses applied work. Return the authoritative
	// receipt first, even if task snapshots have since been forgotten.
	if receipt := s.Applies[id]; receipt != nil && receipt.Status == "applied" {
		return appliedOutcome(c, receipt), nil
	}
	if err := validCandidate(s, c); err != nil {
		return nil, err
	}
	out := &IntegrationOutcome{Status: "done", Candidate: id, Tasks: c.references(), Unchanged: candidateUnchanged(c), Next: integrationDone}
	if completedUnchangedCandidate(s, c) {
		return out, nil
	}
	if err := integrationWriteGuard(s); err != nil {
		return nil, err
	}
	if c.Status != "ready" || len(c.Pending) > 0 || len(c.Conflicts) > 0 {
		return nil, integrationHalt(c)
	}
	completed := false
	err := r.update(ctx, func(current *State) error {
		c = current.Integrations[id]
		if err := validCandidate(current, c); err != nil {
			return err
		}
		for _, ref := range c.references() {
			if err := acceptTask(current, current.Tasks[ref.Task]); err != nil {
				return err
			}
		}
		c.Accepted = true
		completed = completedUnchangedCandidate(current, c)
		return nil
	})
	if err != nil {
		return nil, err
	}
	r.scheduleRelease()
	if completed {
		return out, nil
	}
	return r.integrateApplyLocked(ctx, c)
}

func (r *Runtime) integrateApplyLocked(ctx context.Context, c *IntegrationCandidate) (*IntegrationOutcome, error) {
	receipt, err := r.applyLocked(ctx, c)
	var failure *workflow.Error
	if errors.As(err, &failure) && failure.Code == "parent_changed" {
		return nil, fail("parent_changed", fmt.Sprintf("%s; parent changed since preparation; refresh candidate %s through workflow_run with polly.integration.refresh(%q), then swarm_integrate({candidate: <new id>})", failure.Message, c.ID, c.ID))
	}
	if err != nil {
		return nil, err
	}
	return appliedOutcome(c, receipt), nil
}

func appliedOutcome(c *IntegrationCandidate, receipt *ApplyRecord) *IntegrationOutcome {
	return &IntegrationOutcome{Status: "applied", Candidate: c.ID, Tasks: receipt.Tasks, Unchanged: candidateUnchanged(c), Receipt: receipt, Next: integrationDone}
}

func candidateUnchanged(c *IntegrationCandidate) []string {
	var unchanged []string
	for _, input := range append(append([]IntegrationInput{}, c.Inputs...), c.Repairs...) {
		if input.Base.Tree != "" && input.Base.Tree == input.Submitted.Tree {
			unchanged = append(unchanged, input.Task)
		}
	}
	return unchanged
}

// A valid candidate can remain inspectable without being outstanding work.
// Immutable proofs, rather than status alone, permit forgetting a no-op.
func completedUnchangedCandidate(s *State, c *IntegrationCandidate) bool {
	if validCandidate(s, c) != nil || c.Status != "ready" || len(c.Pending) != 0 || len(c.Conflicts) != 0 || c.Parent.Tree == "" || c.Parent.Tree != c.Merged.Tree {
		return false
	}
	refs := c.references()
	if len(refs) == 0 {
		return false
	}
	for _, ref := range refs {
		t := s.Tasks[ref.Task]
		if t == nil || t.Revision != ref.Revision || t.Status != "done" || !acceptedTaskRevision(t) || !unchangedTask(s, t) {
			return false
		}
	}
	return true
}

func integrationHalt(c *IntegrationCandidate) *workflow.Error {
	seen := map[string]bool{}
	var paths []string
	for _, conflict := range c.Conflicts {
		for _, path := range conflict.Paths {
			if !seen[path] {
				paths = append(paths, path)
				seen[path] = true
			}
		}
	}
	sort.Strings(paths)
	message := fmt.Sprintf("integration %s halted: conflicts in %s (inspect through workflow_run with polly.integration.read(%q)); resolve them with an editing repair task from commit %s using workflow_run and polly.agent({label: \"Resolve conflicts\", commit: %q, task: <repair brief>}), then polly.integration.revise(%q, {task, revision}) in that workflow, or request changes on a task with swarm_review", c.ID, clipInspection(strings.Join(paths, ", "), presentationLabelBytes), c.ID, c.Merged.Commit, c.Merged.Commit, c.ID)
	return &workflow.Error{Code: "conflicts", Message: message, Result: c}
}

func integrateTasksAction(refs []TaskReference) string {
	data, _ := json.Marshal(IntegrateRequest{Tasks: refs})
	return "swarm_integrate(" + string(data) + ")"
}

func integrateCandidateAction(id string) string {
	data, _ := json.Marshal(IntegrateRequest{Candidate: id})
	return "swarm_integrate(" + string(data) + fmt.Sprintf("); if the parent changed, first refresh through workflow_run with polly.integration.refresh(%q)", id)
}
