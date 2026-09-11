package swarm

import (
	"context"
	"errors"

	"github.com/alexschlessinger/pollytool/tools"
)

// statusSummary is the swarm_status result and the parent's swarm_wait result:
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

// status answers swarm_status for actor: the summary, or one paged list when
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
