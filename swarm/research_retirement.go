package swarm

import (
	"context"
	"errors"
	"sync"

	"github.com/alexschlessinger/pollytool/workflow"
)

// RetireAcceptedResearch retires every read-only member whose research has
// been accepted: the member reads idle · retired, its copy is removed, and its
// context record is deleted. Acceptance itself stays a transaction (Review,
// AcknowledgeWorkflow); the tools, scripts and /swarm run this sweep after it,
// so a library caller that only reviews keeps its members. A copy that is not
// provably unchanged is retained with its member, a context left Retiring by
// an interrupted retirement is finished, and editing members are never
// touched. It returns how many contexts it removed.
func (r *Runtime) RetireAcceptedResearch(ctx context.Context) (int, error) {
	s, err := r.read(ctx)
	if err != nil {
		return 0, err
	}
	var candidates, leftovers []*ExecutionContext
	for _, id := range sortedInspectionIDs(s.Contexts) {
		c := s.Contexts[id]
		owner := s.Members[c.Owner]
		switch {
		case c.Retiring:
			if owner != nil && owner.Control == MemberControlRetired {
				leftovers = append(leftovers, c)
			}
		case acceptedResearch(s, c, owner):
			candidates = append(candidates, c)
		}
	}
	if len(candidates)+len(leftovers) == 0 {
		return 0, nil
	}
	// Context locks are tried, never awaited, and held until the files are
	// gone; a context a workflow tool is using waits for the next sweep.
	var unlocks []func()
	defer func() {
		for _, unlock := range unlocks {
			unlock()
		}
	}()
	hold := func(c *ExecutionContext) bool {
		r.mu.Lock()
		active := r.active[c.Owner] != nil
		lock := r.contextLocks[c.ID]
		if lock == nil {
			lock = &sync.Mutex{}
			r.contextLocks[c.ID] = lock
		}
		r.mu.Unlock()
		if active || !lock.TryLock() {
			return false
		}
		unlocks = append(unlocks, lock.Unlock)
		return true
	}
	var verified, finishing []*ExecutionContext
	trees := map[string]string{}
	for _, c := range candidates {
		if !hold(c) {
			continue
		}
		tree, err := r.contextCleanupTree(ctx, s, c)
		var refusal *workflow.Error
		if errors.As(err, &refusal) && refusal.Code == "unintegrated_changes" {
			continue // retained with its member; cleanup names it later
		}
		if err != nil {
			return 0, err
		}
		verified = append(verified, c)
		trees[c.ID] = tree
	}
	for _, c := range leftovers {
		if hold(c) {
			finishing = append(finishing, c)
		}
	}
	if len(verified) > 0 {
		marked, err := r.markAcceptedResearch(ctx, verified)
		if err != nil {
			return 0, err
		}
		finishing = append(finishing, marked...)
	}
	if len(finishing) == 0 {
		return 0, nil
	}
	for _, c := range finishing {
		if c.Checkout != nil {
			if _, err := r.manager(ctx); err != nil {
				return 0, err
			}
			break
		}
	}
	removed, err := r.finishRetirement(ctx, finishing, trees)
	return len(removed), err
}

// markAcceptedResearch records retirement, in one transaction, for the
// verified contexts that are still eligible under the launch and
// coordination locks, so no launch or resume can slip in between.
func (r *Runtime) markAcceptedResearch(ctx context.Context, verified []*ExecutionContext) ([]*ExecutionContext, error) {
	r.launchMu.Lock()
	defer r.launchMu.Unlock()
	r.parentTools.Lock()
	defer r.parentTools.Unlock()
	s, err := r.read(ctx)
	if err != nil {
		return nil, err
	}
	var marking []*ExecutionContext
	r.mu.Lock()
	for _, c := range verified {
		stored := s.Contexts[c.ID]
		if stored != nil && r.active[stored.Owner] == nil && acceptedResearch(s, stored, s.Members[stored.Owner]) {
			marking = append(marking, stored)
		}
	}
	r.mu.Unlock()
	if len(marking) == 0 {
		return nil, nil
	}
	if err := r.markRetiring(ctx, marking); err != nil {
		return nil, err
	}
	return marking, nil
}

// acceptedResearch reports whether a context belongs to an enabled read-only
// member whose last execution completed and whose every task is settled:
// at least one accepted, none still running, submitted, or sent back. A
// member that still owns an unreviewed submission must stay resumable.
func acceptedResearch(s *State, c *ExecutionContext, m *Member) bool {
	if c == nil || m == nil || c.Retiring || !c.ReadOnly || !m.ReadOnly || m.Control != MemberControlEnabled || m.Context != c.ID || c.Owner != m.ID {
		return false
	}
	if e := s.Executions[m.Execution]; e != nil && e.Status != "completed" {
		return false
	}
	accepted := false
	for _, t := range s.Tasks {
		if t.Owner != m.ID {
			continue
		}
		switch t.Status {
		case "done":
			accepted = true
		case "canceled":
		default:
			return false
		}
	}
	return accepted
}
