package swarm

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/alexschlessinger/pollytool/workflow"
)

// releaseEligible only judges saved obligations and runtime reservations.
// Filesystem evidence is checked separately while holding the context lock.
func releaseEligible(s *State, c *ExecutionContext, active map[string]*invocation, reserved func(string) bool) (bool, string) {
	if c == nil || c.Release != "" {
		return false, "workspace is already releasing or retained"
	}
	m := s.Members[c.Owner]
	if m == nil || m.Context != c.ID || c.Owner != m.ID {
		return false, "workspace is not the member's current workspace"
	}
	if ok, why := memberReleaseEligible(s, m, active); !ok {
		return false, why
	}
	if m.Controller != "" && reserved(m.Controller) {
		return false, "member is reserved by a running workflow"
	}
	return true, ""
}

func memberReleaseEligible(s *State, m *Member, active map[string]*invocation) (bool, string) {
	if active[m.ID] != nil {
		return false, "member is executing"
	}
	if e := s.Executions[m.Execution]; e != nil {
		switch e.Status {
		case "queued", "running", "waiting", "paused":
			return false, "member has an active or paused execution"
		}
	}
	for _, t := range s.Tasks {
		if t.Owner == m.ID && t.Status != "done" && t.Status != "canceled" {
			return false, "member has an open task"
		}
	}
	for _, a := range s.Applies {
		if a.Status != "applying" && a.Status != "recovery_required" {
			continue
		}
		for _, ref := range a.Tasks {
			if t := s.Tasks[ref.Task]; t != nil && t.Owner == m.ID {
				return false, "member has an unresolved integration"
			}
		}
	}
	return true, ""
}

func explicitReleaseEligible(s *State, c *ExecutionContext, controller string, active map[string]*invocation) (bool, string) {
	if c == nil || controller == "" {
		return false, "unknown workspace"
	}
	if c.Owner == controller {
		return true, ""
	}
	m := s.Members[c.Owner]
	if m == nil || m.Context != c.ID || m.Controller != controller {
		return false, "workspace belongs to another controller"
	}
	return memberReleaseEligible(s, m, active)
}

func (r *Runtime) contextMutex(id string) *sync.Mutex {
	r.mu.Lock()
	defer r.mu.Unlock()
	lock := r.contextLocks[id]
	if lock == nil {
		lock = &sync.Mutex{}
		r.contextLocks[id] = lock
	}
	return lock
}

func (r *Runtime) releaseSignal(id string) <-chan struct{} {
	r.releaseMu.Lock()
	defer r.releaseMu.Unlock()
	if r.releaseSignals == nil {
		r.releaseSignals = map[string]chan struct{}{}
	}
	ch := r.releaseSignals[id]
	if ch == nil {
		ch = make(chan struct{})
		r.releaseSignals[id] = ch
	}
	return ch
}
func (r *Runtime) notifyRelease(id string) {
	r.releaseMu.Lock()
	defer r.releaseMu.Unlock()
	if ch := r.releaseSignals[id]; ch != nil {
		close(ch)
		delete(r.releaseSignals, id)
	}
}

// scheduleRelease coalesces requests; it never waits for global scheduler locks.
func (r *Runtime) scheduleRelease() {
	r.releaseMu.Lock()
	defer r.releaseMu.Unlock()
	if r.releaseStopped {
		return
	}
	r.releasePending = true
	if r.releaseRunning {
		return
	}
	r.releaseRunning = true
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		for {
			r.releaseMu.Lock()
			if r.releaseStopped || !r.releasePending {
				r.releaseRunning = false
				r.releaseMu.Unlock()
				r.changed()
				return
			}
			r.releasePending = false
			r.releaseMu.Unlock()
			if _, err := r.releaseWorkspaces(r.ctx); err != nil && r.ctx.Err() == nil {
				r.event("release_incomplete", "", err.Error())
			}
		}
	}()
}

func (r *Runtime) releaseWorkspaces(ctx context.Context) (int, error) {
	s, err := r.read(ctx)
	if err != nil {
		return 0, err
	}
	var held []func()
	var heldIDs []string
	var verified []*ExecutionContext
	trees := map[string]string{}
	var errs []error
	defer func() {
		for _, unlock := range held {
			unlock()
		}
		for _, id := range heldIDs {
			r.notifyRelease(id)
		}
	}()
	for _, id := range sortedInspectionIDs(s.Contexts) {
		c := s.Contexts[id]
		r.mu.Lock()
		ok, _ := releaseEligible(s, c, r.active, func(id string) bool { return r.workflowCancels[id] != nil })
		r.mu.Unlock()
		if !ok && c.Release != WorkspaceReleasing {
			continue
		}
		lock := r.contextMutex(id)
		if !lock.TryLock() {
			continue
		}
		held = append(held, lock.Unlock)
		heldIDs = append(heldIDs, id)
		tree, err := r.contextCleanupTree(ctx, s, c)
		if err != nil {
			errs = append(errs, r.releaseFailure(ctx, c, err))
			r.notifyRelease(id)
			continue
		}
		verified = append(verified, c)
		trees[id] = tree
	}
	if len(verified) == 0 {
		return 0, errors.Join(errs...)
	}
	r.launchMu.Lock()
	r.parentTools.Lock()
	if r.closing {
		r.parentTools.Unlock()
		r.launchMu.Unlock()
		return 0, context.Canceled
	}
	var marked []*ExecutionContext
	err = r.update(ctx, func(s *State) error {
		for _, proof := range verified {
			c := s.Contexts[proof.ID]
			if c == nil {
				continue
			}
			r.mu.Lock()
			ok, _ := releaseEligible(s, c, r.active, func(id string) bool { return r.workflowCancels[id] != nil })
			r.mu.Unlock()
			if c.Release == WorkspaceReleasing || ok {
				c.Release = WorkspaceReleasing
				c.Reason = ""
				marked = append(marked, c)
			}
		}
		return nil
	})
	r.parentTools.Unlock()
	r.launchMu.Unlock()
	if err != nil {
		for _, c := range verified {
			errs = append(errs, r.releaseFailure(ctx, c, err))
		}
		return 0, errors.Join(errs...)
	}
	removed, err := r.finishRelease(ctx, marked, trees)
	if err != nil {
		remaining, readErr := r.read(ctx)
		errs = append(errs, err, readErr)
		if readErr == nil {
			for _, c := range marked {
				if remaining.Contexts[c.ID] != nil {
					errs = append(errs, r.releaseFailure(ctx, c, err))
				}
			}
		}
	}
	r.releaseMu.Lock()
	for _, id := range removed {
		delete(r.releaseFailures, id)
	}
	r.releaseMu.Unlock()
	return len(removed), errors.Join(errs...)
}

func (r *Runtime) releaseFailure(ctx context.Context, c *ExecutionContext, err error) error {
	if ctx.Err() != nil {
		return err
	}
	var refusal *workflow.Error
	retain := errors.As(err, &refusal) && refusal.Code == "unintegrated_changes"
	r.releaseMu.Lock()
	if r.releaseFailures == nil {
		r.releaseFailures = map[string]int{}
	}
	r.releaseFailures[c.ID]++
	retain = retain || r.releaseFailures[c.ID] >= 3
	if !retain && !r.releaseStopped {
		r.releasePending = true
	}
	r.releaseMu.Unlock()
	if retain {
		saveErr := r.update(ctx, func(s *State) error {
			if saved := s.Contexts[c.ID]; saved != nil {
				saved.Release = WorkspaceRetained
				saved.Reason = fmt.Sprintf("%v; inspect the workspace or /swarm cleanup %s", err, c.ID)
			}
			return nil
		})
		return errors.Join(err, saveErr)
	}
	return err
}
