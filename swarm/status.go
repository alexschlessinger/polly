package swarm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/tools"
)

// Room for the wait notice: enough to name what is running without turning a
// park into a second swarm_read.
const (
	waitTextItems = 5
	waitTextBytes = 1 << 10
	// waitSummaryFloor is the shortest park that earns a summary when it
	// expires. timeout_ms bottoms out at ten seconds, so summarising every
	// expiry would make wait_agent a cheaper swarm_read — the polling this
	// tool exists to replace. A park long enough to reach this is not polling.
	waitSummaryFloor = 10 * time.Minute
)

const waitNoUpdate = "No update before timeout."

// timeoutMessage is the notice for an expired park. A park that expired is not
// a failure, so a state read that fails keeps the bare notice rather than
// turning the whole call into an error.
func (r *Runtime) timeoutMessage(ctx context.Context, waited time.Duration) string {
	if waited < waitSummaryFloor {
		return waitNoUpdate
	}
	s, err := r.read(ctx)
	if err != nil {
		return waitNoUpdate
	}
	return waitText(Present(s, r.ID, r.ID))
}

// waitText says what is still in flight when a park expires, so the parent can
// decide without a second swarm_read. Bounded like nudgeText; labels and states
// arrive pre-clipped from Present.
func waitText(p Presentation) string {
	var b strings.Builder
	b.WriteString("No update before timeout.")
	if len(p.Working) == 0 {
		b.WriteString(" Nothing is running.")
	} else {
		shown, parts := 0, make([]string, 0, waitTextItems)
		used := b.Len() + len(" Still running: ")
		for _, item := range p.Working {
			part := item.Label + " (" + item.State + ")"
			if item.Agents > 0 {
				part = fmt.Sprintf("%s (%s, %d agents)", item.Label, item.State, item.Agents)
			}
			// The parts are joined only after the loop, so the builder's
			// length does not count them; used does.
			if shown == waitTextItems || used+len(part)+len(", ") > waitTextBytes {
				break
			}
			parts = append(parts, part)
			used += len(part) + len(", ")
			shown++
		}
		b.WriteString(" Still running: " + strings.Join(parts, ", "))
		if rest := len(p.Working) - shown; rest > 0 {
			fmt.Fprintf(&b, ", and %d more", rest)
		}
		b.WriteString(".")
	}
	if p.Counts.NeedsDecision == 0 {
		b.WriteString(" No decisions are waiting.")
	} else {
		fmt.Fprintf(&b, " %d need your decision; swarm_read lists them.", p.Counts.NeedsDecision)
	}
	b.WriteString(" Continue your own work or park again.")
	return b.String()
}

// wakeText names why the park ended. News and an empty swarm are independent,
// so a wake that is both says both rather than picking one.
func wakeText(wake parentWake) string {
	switch {
	case wake.News && wake.Idle:
		return "An agent or workflow update is available, and no workers remain active. Read addressed delivery and use swarm_read for decisions."
	case wake.News:
		return "An agent or workflow update is available. Read addressed delivery and use swarm_read for decisions."
	default:
		return "No workers remain active."
	}
}

// statusSummary is the swarm_read result:
// totals, the actor's budget, the first thing to do, and the first page of
// each list with the offset that continues it. Counts and next never read a
// truncated page.
type statusSummary struct {
	Counts            Counts         `json:"counts"`
	Budget            *Budget        `json:"budget,omitempty"`
	Next              string         `json:"next"`
	NeedsDecision     []DecisionItem `json:"needs_decision"`
	NeedsDecisionNext int            `json:"needsDecisionNext,omitempty"`
	Working           []WorkingItem  `json:"working"`
	WorkingNext       int            `json:"workingNext,omitempty"`
}

// Room for the summary's two first pages, leaving the envelope, counts and
// next well inside the inspection budget.
const (
	statusDecisionsRoom = 9 << 10
	statusWorkingRoom   = 4 << 10
)

// status answers swarm_read for actor: the summary, or one paged list when
// section names it.
func (r *Runtime) status(ctx context.Context, actor string, a tools.Args) (any, error) {
	s, err := r.read(ctx)
	if err != nil {
		return nil, err
	}
	p := Present(s, actor, r.ID)
	switch a.String("section") {
	case "":
		return summarize(p)
	case "decisions":
		return pageInspection(anyItems(p.Decisions), a)
	case "working":
		return pageInspection(anyItems(p.Working), a)
	}
	return nil, errors.New("status section must be decisions or working")
}

func summarize(p Presentation) (statusSummary, error) {
	decisions, err := pageItems(anyItems(p.Decisions), 1, 50, statusDecisionsRoom)
	if err != nil {
		return statusSummary{}, err
	}
	working, err := pageItems(anyItems(p.Working), 1, 50, statusWorkingRoom)
	if err != nil {
		return statusSummary{}, err
	}
	out := statusSummary{Counts: p.Counts, Budget: p.Budget, Next: p.Next, NeedsDecision: []DecisionItem{}, NeedsDecisionNext: decisions.Next, Working: []WorkingItem{}, WorkingNext: working.Next}
	for _, item := range decisions.Items {
		out.NeedsDecision = append(out.NeedsDecision, item.(DecisionItem))
	}
	for _, item := range working.Items {
		out.Working = append(out.Working, item.(WorkingItem))
	}
	return out, nil
}

func anyItems[T any](items []T) []any {
	out := make([]any, 0, len(items))
	for _, item := range items {
		out = append(out, item)
	}
	return out
}
