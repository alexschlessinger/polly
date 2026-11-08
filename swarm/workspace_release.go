package swarm

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/alexschlessinger/pollytool/workflow"
)

// State equivalents remain useful for cold callers and eligibility contract
// tests. Runtime release uses the exact-record equivalents in record_release.go.
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

// scheduleRelease coalesces hints without resetting a running sweep's cursors.
func (r *Runtime) scheduleRelease() {
	if r.initializing.Load() {
		return
	}
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
			if r.releaseStopped || !r.releasePending && !r.releaseScan.retry {
				r.releaseRunning = false
				r.releaseMu.Unlock()
				r.signalCompatibilityChange()
				return
			}
			if r.releaseScan.retry {
				r.releaseScan.retry = false
			} else {
				r.releasePending = false
			}
			r.releaseMu.Unlock()
			if _, err := r.releaseWorkspaces(r.ctx); err != nil && r.ctx.Err() == nil {
				r.event("release_incomplete", "", err.Error())
			}
			// A context operation need not emit a release hint. Retry locked
			// eligible contexts with a bounded backoff, not a busy loop at the
			// front of a partition. No maintenance/scheduler lock is held here.
			r.releaseMu.Lock()
			retry := r.releaseScan.retry
			// Keep external hints separate from retries: a continuation consumes
			// its cursor, not the fresh sweep requested by an arriving hint.
			r.releaseMu.Unlock()
			if retry {
				timer := time.NewTimer(100 * time.Millisecond)
				select {
				case <-r.ctx.Done():
					timer.Stop()
				case <-timer.C:
				}
			}
		}
	}()
}

// Acquire maintenance before context or scheduler locks. Automatic release
// yields this lock between bounded batches; explicit cleanup selects its set
// once and validates that whole requested set before any file removal.
func (r *Runtime) lockMaintenance(ctx context.Context) (func(), error) {
	r.releaseMu.Lock()
	if r.maintenance == nil {
		r.maintenance = make(chan struct{}, 1)
	}
	ch := r.maintenance
	r.releaseMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := r.ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-r.ctx.Done():
		return nil, r.ctx.Err()
	}
}

// Targeted release never schedules a family pass or turns a refusal into success.
func (r *Runtime) releaseWorkspace(ctx context.Context, id string) (any, error) {
	r.launchMu.Lock()
	if r.closing {
		r.launchMu.Unlock()
		return nil, context.Canceled
	}
	r.wg.Add(1)
	r.launchMu.Unlock()
	defer r.wg.Done()
	unlock, err := r.lockMaintenance(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if id == "" {
		return nil, fmt.Errorf("unknown context %q; use items[].context from list_agents({details:true})", id)
	}
	p, err := r.readReleaseProof(ctx, id)
	if err != nil {
		return nil, err
	}
	if p.context != nil {
		if _, err := r.releaseBatchLocked(ctx, []*ExecutionContext{p.context}, false); err != nil {
			return nil, err
		}
	}
	var result any
	err = r.readReleaseRecords(ctx, func(rd *recordReader) error {
		c, err := rd.Context(id)
		if err != nil {
			return err
		}
		if c == nil {
			released, err := releasedWorkspaceRecords(rd, id, "")
			if err != nil {
				return err
			}
			if p.context == nil && !released {
				return fmt.Errorf("unknown context %q; use items[].context from list_agents({details:true})", id)
			}
			result = map[string]any{"context": id, "status": "released"}
			return nil
		}
		if c.Release == WorkspaceRetained {
			result = map[string]any{"context": id, "status": "retained", "reason": c.Reason}
			return nil
		}
		ok, why, err := r.releaseEligibleRecords(rd, c, "")
		if err != nil {
			return err
		}
		status := "ineligible"
		if ok {
			status, why = "busy", "workspace is in use by another operation; retry when it finishes"
		}
		result = map[string]any{"context": id, "status": status, "reason": why}
		return nil
	})
	return result, err
}

func (r *Runtime) releaseFailure(ctx context.Context, c *ExecutionContext, err error) error {
	if ctx.Err() != nil || r.releaseQuarantined(c.ID) {
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
		r.releaseScan.retry = true
	}
	r.releaseMu.Unlock()
	if retain {
		callbackSucceeded := false
		saveErr := r.updateRecords(ctx, func(tx *recordTx) error {
			saved, e := tx.Context(c.ID)
			if e != nil || !reflect.DeepEqual(saved, c) {
				return e
			}
			saved.Release = WorkspaceRetained
			saved.Reason = fmt.Sprintf("%v; inspect the workspace or /swarm cleanup %s", err, c.ID)
			e = tx.PutContext(saved)
			callbackSucceeded = e == nil
			return e
		})
		if saveErr != nil && callbackSucceeded {
			r.quarantineRelease([]*releaseProof{{context: c}})
		}
		return errors.Join(err, saveErr)
	}
	return err
}
