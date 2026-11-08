package swarm

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/worktree"
)

// Cleanup releases inactive execution contexts after proving their current
// files contain no unintegrated changes. An empty ID means the whole family.
// Snapshot references and task provenance survive cleanup; Forget ends their
// restoration lifetime. Findings and transcript history remain in SQLite.
func (r *Runtime) Cleanup(ctx context.Context, contextID string) error {
	unlock, err := r.lockMaintenance(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	r.launchMu.Lock()
	defer r.launchMu.Unlock()
	r.parentTools.Lock()
	defer r.parentTools.Unlock()
	return r.cleanupLocked(ctx, contextID, false)
}

// Discard is the user's explicit answer to a copy cleanup cannot prove. No
// tool or workflow operation exposes it; cleanup's other refusals still apply.
func (r *Runtime) Discard(ctx context.Context, contextID string) error {
	if contextID == "" {
		return errors.New("discard requires a context ID")
	}
	unlock, err := r.lockMaintenance(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	r.launchMu.Lock()
	defer r.launchMu.Unlock()
	r.parentTools.Lock()
	defer r.parentTools.Unlock()
	return r.cleanupLocked(ctx, contextID, true)
}

func (r *Runtime) cleanupLocked(ctx context.Context, contextID string, discard bool) error {
	if r.closing {
		return context.Canceled
	}
	if r.HasActive() {
		return errors.New("stop active members and workflows before cleanup")
	}
	var proofs []*releaseProof
	var previews []*worktree.Preview
	err := r.readReleaseRecords(ctx, func(rd *recordReader) error {
		if err := cleanupIntegrationsRecords(rd); err != nil {
			return err
		}
		var contexts []*ExecutionContext
		if contextID != "" {
			c, err := rd.Context(contextID)
			if err != nil {
				return err
			}
			if c == nil {
				released, err := releasedWorkspaceRecords(rd, contextID, "")
				if err != nil || released {
					return err
				}
				return fmt.Errorf("unknown execution context %q; cleanup requires items[].context from list_agents({details:true})", contextID)
			}
			contexts = append(contexts, c)
		} else {
			if rd.journalOnly {
				return nil
			}
			var err error
			contexts, err = followupRecordValues(func(q sessions.RecordQuery) (recordPage[ExecutionContext], error) {
				return queryRecords[ExecutionContext](rd, "context", q)
			}, sessions.CoordinationIndexKind, "context")
			if err != nil {
				return err
			}
			previews, err = followupRecordValues(func(q sessions.RecordQuery) (recordPage[worktree.Preview], error) {
				return queryRecords[worktree.Preview](rd, "preview", q)
			}, sessions.CoordinationIndexKind, "preview")
			if err != nil {
				return err
			}
		}
		for _, c := range contexts {
			p, err := releaseProofRecords(rd, c)
			if err != nil {
				return err
			}
			if err := cleanupPausedRecords(rd, p); err != nil {
				return err
			}
			proofs = append(proofs, p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	// Try every selected context before proving or removing any of them.
	var held []func()
	defer func() {
		for _, unlock := range held {
			unlock()
		}
		for _, p := range proofs {
			r.notifyRelease(p.context.ID)
		}
	}()
	for _, p := range proofs {
		lock := r.contextMutex(p.context.ID)
		if !lock.TryLock() {
			return errors.New("release in progress; retry")
		}
		held = append(held, lock.Unlock)
	}
	trees := map[string]string{}
	for _, p := range proofs {
		c := *p.context
		// Discard is persisted only by the mark transaction, not by changing
		// the proof's original identity before its transaction recheck.
		c.Disposable = c.Disposable || discard
		tree, err := r.contextCleanupTreeRecords(ctx, &c)
		if err != nil {
			return fmt.Errorf("context %s: %w; /swarm discard %s removes it and everything in it", c.ID, err, c.ID)
		}
		trees[c.ID] = tree
	}
	if len(previews) > 0 {
		manager, err := r.manager(ctx)
		if err != nil {
			return err
		}
		for _, preview := range previews {
			current, err := manager.Capture(ctx, preview.Checkout.Path)
			if err != nil {
				return err
			}
			if current.Tree != preview.Checkout.Base.Tree {
				return errors.New("cleanup refuses unintegrated preview edits in " + preview.ID)
			}
		}
	}
	if len(proofs) > 0 {
		marked, err := r.markReleaseRecords(ctx, proofs, discard, true, func(rd *recordReader, c *ExecutionContext) (bool, error) {
			if err := cleanupIntegrationsRecords(rd); err != nil {
				return false, err
			}
			p, err := releaseProofRecords(rd, c)
			if err == nil {
				err = cleanupPausedRecords(rd, p)
			}
			return err == nil, err
		})
		if err != nil {
			return err
		}
		if _, err := r.finishRelease(ctx, marked, trees); err != nil {
			return err
		}
	}
	for _, p := range previews {
		if err := r.worktrees.Cleanup(ctx, p.Checkout, ""); err != nil {
			return err
		}
		if err := r.updateRecords(ctx, func(tx *recordTx) error {
			current, err := getRecord[worktree.Preview](tx.recordReader, "preview", p.ID)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(current, p) {
				return fail("workspace_changed", "preview changed during cleanup")
			}
			return tx.Delete(sessions.RecordKey{Kind: "preview", ID: p.ID})
		}); err != nil {
			return err
		}
	}
	return nil
}

// Forget explicitly selects all pinned snapshots, but not transcripts, closed
// tasks/executions, workflow journals, or unrelated coordination history.
func (r *Runtime) Forget(ctx context.Context) error {
	unlock, err := r.lockMaintenance(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	r.launchMu.Lock()
	defer r.launchMu.Unlock()
	r.parentTools.Lock()
	defer r.parentTools.Unlock()
	var snapshots []*worktree.Snapshot
	err = r.readReleaseRecords(ctx, func(rd *recordReader) error {
		if rd.journalOnly {
			return nil
		}
		if err := forgetObligationsRecords(rd); err != nil {
			return err
		}
		var err error
		snapshots, err = followupRecordValues(func(q sessions.RecordQuery) (recordPage[worktree.Snapshot], error) {
			return queryRecords[worktree.Snapshot](rd, "snapshot", q)
		}, sessions.CoordinationIndexKind, "snapshot")
		return err
	})
	if err != nil {
		return err
	}
	if err := r.cleanupLocked(ctx, "", false); err != nil {
		return err
	}
	// A failed state write can leave a manifest-owned commit pin without a
	// snapshot record. Explicit forget must retire those pins too.
	m, err := r.manager(ctx)
	if err != nil {
		if len(snapshots) == 0 && errors.Is(err, worktree.ErrNotRepository) {
			return nil
		}
		return err
	}
	if err := m.CleanupSnapshotRefs(ctx); err != nil {
		return err
	}
	if len(snapshots) == 0 {
		return nil
	}
	return r.updateRecords(ctx, func(tx *recordTx) error {
		if err := forgetObligationsRecords(tx.recordReader); err != nil {
			return err
		}
		for _, snapshot := range snapshots {
			current, err := tx.Snapshot(snapshot.ID)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(current, snapshot) {
				return fail("workspace_changed", "snapshot changed during forget")
			}
			if err := tx.Delete(sessions.RecordKey{Kind: "snapshot", ID: snapshot.ID}); err != nil {
				return err
			}
		}
		return nil
	})
}
