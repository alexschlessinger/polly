package sessions

import "github.com/alexschlessinger/pollytool/internal/coordsem"

type SemanticCounts = coordsem.Counts
type SemanticBudget = coordsem.Budget
type SemanticDecision = coordsem.DecisionItem
type SemanticWorking = coordsem.WorkingItem

// SemanticStatusHeader contains totals over the entire visible family, never a
// page. ColdCompatibility identifies ambiguous pre-binding workflow ownership;
// callers must use their full-state compatibility path, not guess an owner.
type SemanticStatusHeader struct {
	Counts                                  SemanticCounts
	Budget                                  *SemanticBudget
	Next, Run, ParentDisposition            string
	HasRuns, WorkRunning, ColdCompatibility bool
	Deliveries                              int
}
type SemanticStatusQuery struct {
	Actor, Section, After   string
	Offset, Limit, MaxBytes int
}
type SemanticStatusPage struct {
	Decisions []SemanticDecision
	Working   []SemanticWorking
	Total     int
	Next      string
	Oversized bool
}

// CoordinationStatusReader is optional independently of indexed records and
// semantic news history. Values are owned and reads are callback-local.
type CoordinationStatusReader interface {
	SemanticStatus(actor string, includeNext bool) (SemanticStatusHeader, error)
	SemanticStatusPage(SemanticStatusQuery) (SemanticStatusPage, error)
}
