package swarm

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/internal/scratch"
	"github.com/alexschlessinger/pollytool/sessions"
)

// markReleaseRecords commits exact provenance before touching files. The caller
// holds scheduler/context locks. A changed proof is either skipped (automatic
// release) or vetoes the whole requested batch (explicit cleanup).
func (r *Runtime) markReleaseRecords(ctx context.Context, proofs []*releaseProof, discard, strict bool, eligible func(*recordReader, *ExecutionContext) (bool, error)) ([]*releaseProof, error) {
	var marked []*releaseProof
	callbackSucceeded := false
	err := r.updateRecords(ctx, func(tx *recordTx) error {
		for _, proof := range proofs {
			c, err := tx.Context(proof.context.ID)
			if err != nil {
				return err
			}
			current, err := releaseProofRecords(tx.recordReader, c)
			if err != nil {
				return err
			}
			if !sameReleaseProof(proof, current) {
				if strict {
					return fail("workspace_changed", "workspace changed during release; retry explicitly")
				}
				continue
			}
			if eligible != nil {
				ok, err := eligible(tx.recordReader, c)
				if err != nil {
					return err
				}
				if !ok {
					continue
				}
			}
			c.Release, c.Reason = WorkspaceReleasing, ""
			c.Disposable = c.Disposable || discard
			if err := tx.PutContext(c); err != nil {
				return err
			}
			marked = append(marked, current)
		}
		callbackSucceeded = true
		return nil
	})
	if err != nil {
		if callbackSucceeded {
			// The backend may have committed. Do not automatically replay this
			// mutation, remove files, or publish tentative completion effects.
			r.quarantineRelease(proofs)
			err = &releaseCommitUncertain{cause: err}
		}
		return nil, err
	}
	return marked, nil
}

// finishRelease closes bindings and processes before deletion, then records all
// successful removals in one transaction. The caller holds each context lock.
// Both the pre-removal read and receipt transaction recheck the marked identity.
func (r *Runtime) finishRelease(ctx context.Context, proofs []*releaseProof, trees map[string]string) (removed []string, err error) {
	defer func() {
		for _, p := range proofs {
			r.notifyRelease(p.context.ID)
		}
	}()
	// Caller cancellation cannot strand a known committed release. Parent lease
	// loss still cancels this bounded finishing phase and fences record writes.
	finishCtx, cancel := context.WithTimeout(r.config.Parent.Context(), 2*time.Minute+10*time.Second*time.Duration(len(proofs)))
	defer cancel()
	var errs []error
	var finished []*releaseProof
	for _, p := range proofs {
		c := p.context
		current, e := r.readReleaseProof(finishCtx, c.ID)
		if e != nil {
			errs = append(errs, e)
			continue
		}
		if c.Release != WorkspaceReleasing || !sameReleaseProof(p, current) {
			errs = append(errs, fail("workspace_changed", "workspace changed before removal; retry explicitly"))
			continue
		}
		r.unbindContext(c.ID)
		r.reapContextProcesses(c)
		if e := r.removeContextFiles(finishCtx, c, trees[c.ID]); e != nil {
			errs = append(errs, fmt.Errorf("context %s: %w", c.ID, e))
			continue
		}
		finished = append(finished, p)
	}
	var wake []string
	if len(finished) > 0 {
		callbackSucceeded := false
		e := r.updateRecords(finishCtx, func(tx *recordTx) error {
			// Several receipts can share an owner (its current context and an
			// older releasing one). Validate every proof against the initial
			// transaction view before our own unlink changes that owner's proof.
			var contexts []*ExecutionContext
			for _, p := range finished {
				c, err := tx.Context(p.context.ID)
				if err != nil {
					return err
				}
				current, err := releaseProofRecords(tx.recordReader, c)
				if err != nil {
					return err
				}
				if !sameReleaseProof(p, current) {
					return fail("workspace_changed", "workspace changed before removal receipt; retry explicitly")
				}
				contexts = append(contexts, c)
			}
			for _, c := range contexts {
				members, err := followupRecordValues(tx.Members, sessions.CoordinationIndexOverviewDependency, sessions.CoordinationIndexKey("context", c.ID))
				if err != nil {
					return err
				}
				for _, m := range members {
					if m.Context != c.ID {
						continue
					}
					m.SandboxPresets = sandboxPresets(m.SandboxPresets, c.SandboxPresets...)
					m.Context = ""
					if err := tx.PutMember(m); err != nil {
						return err
					}
					wake = append(wake, m.ID)
				}
				if err := tx.Delete(sessions.RecordKey{Kind: "context", ID: c.ID}); err != nil {
					return err
				}
			}
			callbackSucceeded = true
			return nil
		})
		if e != nil {
			if callbackSucceeded {
				r.quarantineRelease(finished)
				e = &releaseCommitUncertain{cause: e}
			}
			return nil, errors.Join(append(errs, e)...)
		}
	}
	for _, p := range finished {
		removed = append(removed, p.context.ID)
	}
	for _, id := range wake {
		r.wake(id)
	}
	return removed, errors.Join(errs...)
}

func (r *Runtime) removeContextFiles(ctx context.Context, c *ExecutionContext, tree string) error {
	if c.Checkout != nil {
		checkout := *c.Checkout
		// Older checkout manifests recomputed scratch from the active root;
		// the execution context retained the actual path across restarts.
		if c.Scratch != "" {
			checkout.Scratch = c.Scratch
		}
		if c.Disposable {
			return r.worktrees.FinishDiscard(ctx, checkout)
		}
		return r.worktrees.FinishCleanup(ctx, checkout, tree)
	}
	if c.Scratch == "" {
		return nil
	}
	dir, err := r.runtimeDirectory()
	if err != nil {
		return err
	}
	// The retained root need not be the active one, and its slot may exceed
	// today's allocation capacity. Derive a canonical logical slot, then
	// require both the exact scratch name and its protected owner record.
	name := filepath.Base(c.Scratch)
	index, _, _ := strings.Cut(strings.TrimPrefix(name, "live-"), "-")
	if n, err := strconv.Atoi(index); err == nil && n >= 0 {
		slot := filepath.Join(dir, fmt.Sprintf("live-%04d", n))
		if name == filepath.Base(scratch.DirFor(slot)) {
			return scratch.ReleaseOwned(c.Scratch, slot)
		}
	}
	// Before the separate root, live scratches were immediate children of
	// runtime/scratch. A saved path is not authority to delete other data.
	if filepath.IsAbs(c.Scratch) && filepath.Clean(c.Scratch) == c.Scratch && filepath.Dir(c.Scratch) == filepath.Join(dir, "scratch") && strings.HasPrefix(filepath.Base(c.Scratch), "live-") {
		return scratch.RemoveAll(c.Scratch)
	}
	return fmt.Errorf("not a runtime-owned live scratch: %s", c.Scratch)
}
