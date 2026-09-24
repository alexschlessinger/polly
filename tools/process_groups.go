package tools

import (
	"slices"
	"sync"

	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

// processGroups remembers the private process groups a bound registry's
// commands ran in. Background children deliberately outlive a call (see
// finite_command.go); they do not outlive the binding unless detached first.
// A swarm member's runtime detaches them at the end of every execution slice
// and reaps them when the workspace is released, so a server a member starts
// serves its later turns and dies with its workspace instead of with the
// host. A group ID can be reused once its last process exits; the sets kept
// here are bounded to groups this binding created, and kills happen only at
// release and shutdown, so that window is accepted.
type processGroups struct {
	mu  sync.Mutex
	ids map[int]struct{}
}

// remember records a command's group, forgetting groups already gone.
func (g *processGroups) remember(pgid int) {
	if g == nil || pgid <= 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ids == nil {
		g.ids = map[int]struct{}{}
	}
	for id := range g.ids {
		if !sandbox.ProcessGroupAlive(id) {
			delete(g.ids, id)
		}
	}
	g.ids[pgid] = struct{}{}
}

// detach hands back the groups still alive, sorted, and forgets them all.
func (g *processGroups) detach() []int {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	var alive []int
	for id := range g.ids {
		if sandbox.ProcessGroupAlive(id) {
			alive = append(alive, id)
		}
	}
	g.ids = nil
	slices.Sort(alive)
	return alive
}

// kill ends every surviving group and reports the ones it killed.
func (g *processGroups) kill() []int { return KillProcessGroups(g.detach()) }

// KillProcessGroups kills every listed group still alive and reports the
// ones it killed. Callers pass groups detached from a bound registry.
func KillProcessGroups(pgids []int) []int {
	var killed []int
	for _, id := range pgids {
		if !sandbox.ProcessGroupAlive(id) {
			continue
		}
		if sandbox.KillProcessGroup(id) == nil {
			killed = append(killed, id)
		}
	}
	return killed
}

// DetachProcessGroups hands back the process groups the registry's commands
// left running and forgets them: the caller now owns their reaping, and
// Close no longer kills them. An unbound registry tracks none.
func (r *ToolRegistry) DetachProcessGroups() []int {
	if r == nil {
		return nil
	}
	return r.processGroups.detach()
}
