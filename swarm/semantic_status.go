package swarm

import (
	"context"
	"errors"

	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
)

// semanticStatus selects only a capability on the supplied callback-local
// reader. Missing capability or ambiguous historical display ownership uses the
// original full-state implementation; read failures never select a fallback.
func (r *Runtime) semanticStatus(ctx context.Context, actor string, a tools.Args) (value any, native bool, err error) {
	section := a.String("section")
	if section != "" && section != "decisions" && section != "working" {
		return nil, true, errors.New("status section must be decisions or working")
	}
	err = r.readRecords(ctx, func(rd *recordReader) error {
		reader, ok := rd.raw.(sessions.CoordinationStatusReader)
		if !ok {
			return nil
		}
		header, err := reader.SemanticStatus(actor, section == "")
		if err != nil {
			return err
		}
		if header.ColdCompatibility {
			return nil
		}
		native = true
		if section != "" {
			offset, limit := a.Int("offset", 1), a.Int("limit", 50)
			if offset < 1 || limit < 1 || limit > 100 {
				return errors.New("offset must be positive and limit between 1 and 100")
			}
			value, err = narrowStatusPage(reader, actor, section, offset, limit, inspectionBytes-2048)
			return err
		}
		decisions, err := narrowStatusPage(reader, actor, "decisions", 1, 50, statusDecisionsRoom)
		if err != nil {
			return err
		}
		working, err := narrowStatusPage(reader, actor, "working", 1, 50, statusWorkingRoom)
		if err != nil {
			return err
		}
		out := statusSummary{Counts: Counts(header.Counts), Next: header.Next, NeedsDecision: []DecisionItem{}, NeedsDecisionNext: decisions.Next, Working: []WorkingItem{}, WorkingNext: working.Next}
		if header.Budget != nil {
			budget := Budget(*header.Budget)
			out.Budget = &budget
		}
		for _, item := range decisions.Items {
			out.NeedsDecision = append(out.NeedsDecision, item.(DecisionItem))
		}
		for _, item := range working.Items {
			out.Working = append(out.Working, item.(WorkingItem))
		}
		value = out
		return nil
	})
	return value, native, err
}
func narrowStatusPage(reader sessions.CoordinationStatusReader, actor, section string, offset, limit, room int) (inspectionPage, error) {
	// Compact JSON is no larger than the indented caller envelope. Selecting at
	// most room bytes plus its one oversized-item exception bounds hydration;
	// pageItems retains the existing exact caller byte/offset behavior.
	selected, err := reader.SemanticStatusPage(sessions.SemanticStatusQuery{Actor: actor, Section: section, Offset: offset - 1, Limit: limit, MaxBytes: room})
	if err != nil {
		return inspectionPage{}, err
	}
	var items []any
	if section == "decisions" {
		for _, item := range selected.Decisions {
			items = append(items, DecisionItem(item))
		}
	} else {
		for _, item := range selected.Working {
			items = append(items, WorkingItem(item))
		}
	}
	page, err := pageItems(items, 1, limit, room)
	if err != nil {
		return page, err
	}
	page.Total = selected.Total
	page.Next = 0
	if next := offset + len(page.Items); next <= selected.Total {
		page.Next = next
	}
	return page, nil
}
func (r *Runtime) semanticTimeout(ctx context.Context) (text string, native bool, err error) {
	err = r.readRecords(ctx, func(rd *recordReader) error {
		reader, ok := rd.raw.(sessions.CoordinationStatusReader)
		if !ok {
			return nil
		}
		header, err := reader.SemanticStatus(r.ID, false)
		if err != nil {
			return err
		}
		if header.ColdCompatibility {
			return nil
		}
		native = true
		page, err := reader.SemanticStatusPage(sessions.SemanticStatusQuery{Actor: r.ID, Section: "working", Limit: waitTextItems, MaxBytes: waitTextBytes})
		if err != nil {
			return err
		}
		p := Presentation{Counts: Counts(header.Counts)}
		for _, item := range page.Working {
			p.Working = append(p.Working, WorkingItem(item))
		}
		text = waitTextTotal(p, header.Counts.Working)
		return nil
	})
	return text, native, err
}
