package swarm

import (
	"context"
	"errors"
	"strings"
)

// TaskDeferral records an explicit decision to retain unresolved work without
// blocking the parent. It neither changes the outcome nor accepts a candidate.
type TaskDeferral struct {
	Workflow         string `json:"workflow"`
	Execution        string `json:"execution"`
	Owner            string `json:"owner"`
	Revision         int    `json:"revision"`
	Generation       int    `json:"generation"`
	AcceptedRevision int    `json:"acceptedRevision"`
	Status           string `json:"status"`
	Note             string `json:"note"`
}

func TaskDeferred(s *State, t *Task) bool {
	if t == nil || t.Deferral == nil || t.Status == "done" || t.Status == "canceled" {
		return false
	}
	d := t.Deferral
	e, w := s.Executions[t.Execution], s.Workflows[d.Workflow]
	return e != nil && w != nil && w.Acknowledged && (w.Status == "failed" || w.Status == "canceled" || w.Status == "interrupted") &&
		e.Workflow == d.Workflow && e.Run == w.Run && e.Run == t.Run && e.Member == t.Owner &&
		e.ID == d.Execution && e.Generation == d.Generation &&
		t.Owner == d.Owner && t.Revision == d.Revision && t.Status == d.Status &&
		t.AcceptedRevision == d.AcceptedRevision && e.Status != "running" && e.Status != "queued" && e.Status != "waiting"
}

func DeferredCount(s *State, workflowID string) int {
	n := 0
	for _, t := range s.Tasks {
		if TaskDeferred(s, t) && t.Deferral.Workflow == workflowID {
			n++
		}
	}
	return n
}

// DeferWorkflow atomically acknowledges a terminal failure and records the
// exact unresolved submissions owned by that attempt. Display provenance is
// deliberately insufficient authority to defer historical tasks.
func (r *Runtime) DeferWorkflow(ctx context.Context, id, note string) error {
	if strings.TrimSpace(note) == "" {
		return errors.New("deferral requires a nonblank explanation")
	}
	r.parentTools.Lock()
	defer r.parentTools.Unlock()
	return r.update(ctx, func(s *State) error {
		w := s.Workflows[id]
		if w == nil {
			return errors.New("unknown workflow")
		}
		if w.Status != "failed" && w.Status != "canceled" && w.Status != "interrupted" {
			return errors.New("only a terminal failed, canceled or interrupted workflow can be deferred")
		}
		for _, e := range s.Executions {
			if e.Workflow == id && (e.Status == "running" || e.Status == "queued" || e.Status == "waiting") {
				return fail("session_busy", "workflow agents are still settling; wait before deferring")
			}
		}
		w.Acknowledged = true
		for _, t := range s.Tasks {
			e := s.Executions[t.Execution]
			if e == nil || e.Workflow != id || e.Run != w.Run || t.Run != e.Run || t.Owner != e.Member || t.Status == "done" || t.Status == "canceled" {
				continue
			}
			t.Deferral = &TaskDeferral{Workflow: id, Execution: e.ID, Owner: t.Owner, Revision: t.Revision, Generation: e.Generation, AcceptedRevision: t.AcceptedRevision, Status: t.Status, Note: strings.TrimSpace(note)}
		}
		return nil
	})
}

// Reactivating retained work restores its original accounting, never a fresh
// budget. One workspace still has one active coordination run. Called inside
// the same transaction as the successful mutation, so refusals change nothing.
func reactivateTask(s *State, t *Task) error {
	if t == nil || t.Deferral == nil {
		return nil
	}
	for _, run := range s.Runs {
		if run.ID != t.Run && (run.Status == "running" || run.Status == "paused") {
			return fail("session_busy", "deferred work belongs to an earlier run; wait for the current run to settle before recovering it")
		}
	}
	run := s.Runs[t.Run]
	if run == nil {
		return errors.New("deferred task's original run is unavailable")
	}
	if run.Status == "completed" {
		run.Status = "running"
	}
	t.Deferral = nil
	return nil
}

func runDeferred(s *State, run string) bool {
	found := false
	for _, t := range s.Tasks {
		if t.Run != run || t.Status == "done" || t.Status == "canceled" {
			continue
		}
		if !TaskDeferred(s, t) {
			return false
		}
		found = true
	}
	return found
}
