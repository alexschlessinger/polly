package swarm

import (
	"context"
	"errors"

	"github.com/alexschlessinger/pollytool/sessions"
)

const releaseBatchSize = 32

type releasePartition struct {
	key, after, cutoff string
	remaining          int
	resume             bool // count exhausted before the captured cutoff/end
}

type releaseSweep struct {
	partitions [2]releasePartition
	next       int
}

type releaseScanState struct {
	// sweep is serialized by maintenance; retry/uncertain by releaseMu.
	sweep     *releaseSweep
	retry     bool
	uncertain map[string]bool
}

func (r *Runtime) quarantineRelease(proofs []*releaseProof) {
	r.releaseMu.Lock()
	defer r.releaseMu.Unlock()
	if r.releaseScan.uncertain == nil {
		r.releaseScan.uncertain = map[string]bool{}
	}
	for _, p := range proofs {
		r.releaseScan.uncertain[p.context.ID] = true
	}
}

func (r *Runtime) releaseQuarantined(id string) bool {
	r.releaseMu.Lock()
	defer r.releaseMu.Unlock()
	return r.releaseScan.uncertain[id]
}

// nextReleaseBatch advances over every selected row, including locked and
// ineligible ones. Count and last-key bounds make a sweep finite under inserts;
// alternating partitions prevents new unreleased work starving old receipts.
func (r *Runtime) nextReleaseBatch(ctx context.Context) (contexts []*ExecutionContext, done bool, err error) {
	sweep := r.releaseScan.sweep
	if sweep != nil {
		copy := *sweep
		sweep = &copy
	}
	err = r.readReleaseRecords(ctx, func(rd *recordReader) error {
		if rd.journalOnly {
			done = true
			return nil
		}
		if sweep == nil {
			sweep = &releaseSweep{partitions: [2]releasePartition{{key: ""}, {key: WorkspaceReleasing}}}
			for i := range sweep.partitions {
				p := &sweep.partitions[i]
				extent, err := rd.raw.Query(sessions.RecordQuery{Kind: "context", Index: sessions.CoordinationIndexContextRelease, Key: p.key, CountOnly: true, IncludeTotal: true, IncludeLast: true})
				if err != nil {
					return err
				}
				p.remaining = extent.Total
				if extent.Last != nil {
					p.cutoff = extent.Last.ID
				}
			}
		}
		start := sweep.next
		sweep.next = 1 - sweep.next
		for n := 0; n < 2 && len(contexts) < releaseBatchSize; n++ {
			p := &sweep.partitions[(start+n)%2]
			if p.remaining <= 0 {
				continue
			}
			limit := min(releaseBatchSize-len(contexts), p.remaining)
			page, err := queryRecords[ExecutionContext](rd, "context", sessions.RecordQuery{Index: sessions.CoordinationIndexContextRelease, Key: p.key, After: p.after, Limit: limit, MaxBytes: 256 << 10})
			if err != nil {
				return err
			}
			reachedCutoff := false
			for _, record := range page.Records {
				p.remaining--
				if record.Key.ID >= p.cutoff {
					reachedCutoff = true
				}
				if record.Key.ID > p.cutoff {
					break
				}
				contexts = append(contexts, record.Value)
			}
			if page.Next == "" || reachedCutoff {
				p.remaining = 0
			} else if page.Next == p.after {
				return sessions.ErrCoordinationCursor
			}
			// Inserts can consume the captured count before an original tail
			// row is reached. Keep its cursor for a bounded follow-on sweep,
			// rather than stranding that tail or restarting from the front.
			p.resume = p.remaining <= 0 && page.Next != "" && !reachedCutoff
			p.after = page.Next
		}
		done = sweep.partitions[0].remaining <= 0 && sweep.partitions[1].remaining <= 0
		return nil
	})
	if err == nil {
		r.releaseScan.sweep = sweep
		if done {
			r.releaseScan.sweep = nil
			if sweep != nil {
				continuing := false
				for i := range sweep.partitions {
					p := &sweep.partitions[i]
					if p.resume {
						p.remaining, p.resume = releaseBatchSize, false
						continuing = true
					}
				}
				if continuing {
					r.releaseScan.sweep = sweep
					r.releaseMu.Lock()
					r.releaseScan.retry = true
					r.releaseMu.Unlock()
				}
			}
		}
	}
	return
}

func (r *Runtime) releaseWorkspaces(ctx context.Context) (int, error) {
	var removed int
	var errs []error
	for {
		unlock, err := r.lockMaintenance(ctx)
		if err != nil {
			return removed, errors.Join(append(errs, err)...)
		}
		contexts, done, selectErr := r.nextReleaseBatch(ctx)
		err = selectErr
		if selectErr == nil {
			var n int
			n, err = r.releaseBatchLocked(ctx, contexts, true)
			removed += n
		}
		unlock()
		if err != nil {
			errs = append(errs, err)
		}
		// A failed row must not strand later pages. A selection failure has
		// not advanced the cursor and is left for a subsequent external hint.
		if done || selectErr != nil || ctx.Err() != nil {
			return removed, errors.Join(errs...)
		}
	}
}

// releaseBatchLocked owns maintenance, but never waits on a context operation.
// Proof/mark/removal cost is bounded by the selected batch and its exact live
// obligations, not closed task/apply/snapshot/workflow history.
func (r *Runtime) releaseBatchLocked(ctx context.Context, contexts []*ExecutionContext, automatic bool) (int, error) {
	var held []func()
	var heldIDs []string
	var verified []*releaseProof
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
	for _, candidate := range contexts {
		if automatic && r.releaseQuarantined(candidate.ID) {
			continue
		}
		var proof *releaseProof
		var eligible bool
		err := r.readReleaseRecords(ctx, func(rd *recordReader) error {
			c, err := rd.Context(candidate.ID)
			if err != nil {
				return err
			}
			eligible, _, err = r.releaseEligibleRecords(rd, c, "")
			if err == nil && eligible {
				proof, err = releaseProofRecords(rd, c)
			}
			return err
		})
		if err != nil {
			return 0, errors.Join(append(errs, err)...)
		}
		if !eligible {
			continue
		}
		c := proof.context
		lock := r.contextMutex(c.ID)
		if !lock.TryLock() {
			if automatic {
				r.releaseMu.Lock()
				r.releaseScan.retry = true
				r.releaseMu.Unlock()
			}
			continue
		}
		held = append(held, lock.Unlock)
		heldIDs = append(heldIDs, c.ID)
		tree, err := r.contextCleanupTreeRecords(ctx, c)
		if err != nil {
			errs = append(errs, r.releaseFailure(ctx, c, err))
			continue
		}
		verified = append(verified, proof)
		trees[c.ID] = tree
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
	marked, err := r.markReleaseRecords(ctx, verified, false, false, func(rd *recordReader, c *ExecutionContext) (bool, error) {
		ok, _, err := r.releaseEligibleRecords(rd, c, "")
		return ok, err
	})
	r.parentTools.Unlock()
	r.launchMu.Unlock()
	if err != nil {
		for _, p := range verified {
			errs = append(errs, r.releaseFailure(ctx, p.context, err))
		}
		return 0, errors.Join(errs...)
	}
	removed, err := r.finishRelease(ctx, marked, trees)
	if err != nil {
		errs = append(errs, err)
		for _, p := range marked {
			current, readErr := r.readReleaseProof(ctx, p.context.ID)
			if readErr != nil {
				errs = append(errs, readErr)
			} else if sameReleaseProof(p, current) {
				errs = append(errs, r.releaseFailure(ctx, p.context, err))
			}
		}
	}
	r.releaseMu.Lock()
	for _, id := range removed {
		delete(r.releaseFailures, id)
		delete(r.releaseScan.uncertain, id)
	}
	r.releaseMu.Unlock()
	return len(removed), errors.Join(errs...)
}
