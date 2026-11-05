package main

import (
	"unicode"

	ui "github.com/metaspartan/gotui/v5"
)

// Anchor within a logical block using non-whitespace text, so line wrapping
// and indentation can change without moving the reader to unrelated content.
type viewAnchor struct {
	block string
	units int
}

// viewSection is one activity record's or row's choices as a view keeps
// them: whether its line is open, a row's output, and for a thought which of
// its parts show every retained row.
type viewSection struct {
	open           bool
	outputExpanded bool
	full           []bool
}

func rowTextUnits(row []ui.Cell) int {
	n := 0
	for _, c := range row {
		if c.Rune != 0 && !unicode.IsSpace(c.Rune) {
			n++
		}
	}
	return n
}

func rememberViewPosition(m *replModel, s *viewState) {
	if s.follow {
		return
	}
	row := s.top
	for _, b := range m.visual.blocks {
		if row < len(b.rows) {
			s.anchor = viewAnchor{block: b.key}
			for _, r := range b.rows[:max(0, row)] {
				s.anchor.units += rowTextUnits(r)
			}
			return
		}
		row -= len(b.rows)
	}
}

func restoreViewPosition(m *replModel, s *viewState) {
	if s.follow || s.anchor.block == "" {
		return
	}
	start := 0
	for _, b := range m.visual.blocks {
		if b.key == s.anchor.block {
			units := 0
			for row, r := range b.rows {
				n := rowTextUnits(r)
				if units+n > s.anchor.units || row == len(b.rows)-1 {
					s.top = start + row
					return
				}
				units += n
			}
		}
		start += len(b.rows)
	}
}

func toolSectionKey(r *toolDisclosureRecord) string {
	if len(r.rows) == 0 {
		return ""
	}
	if row := r.rows[0]; row.isProjectedAgent() {
		if row.agent.workflowID != "" {
			return "swarm:workflow:" + row.agent.workflowID
		}
		return "swarm:member:" + row.agent.viewID
	}
	return r.rows[0].sectionKey
}

func rememberViewSections(m *replModel, s *viewState) {
	if s.sections == nil {
		s.sections = make(map[string]viewSection)
	}
	for _, r := range m.toolDisclosures.all() {
		if key := toolSectionKey(r); key != "" {
			v := s.sections[key]
			v.open = r.expanded
			s.sections[key] = v
		}
		for _, row := range r.rows {
			if row.sectionKey == "" {
				continue
			}
			v := s.sections[row.sectionKey]
			v.outputExpanded = row.outputExpanded
			s.sections[row.sectionKey] = v
		}
	}
	for _, r := range m.reasoningRecords.all() {
		if r.sectionKey == "" {
			continue
		}
		full := make([]bool, len(r.segments))
		for i, segment := range r.segments {
			full[i] = segment.full
		}
		s.sections[r.sectionKey] = viewSection{open: r.expanded, full: full}
	}
	s.revision++
}

// applyViewSections gives a projection the view's depth and the
// choices made in it: a section the view remembers takes its remembered
// state, any other output stays closed and any other thought part takes the
// depth's default, and a line nobody chose for stays as the projection
// opened it.
func applyViewSections(m *replModel, s viewState) {
	m.depth = s.depth
	full := s.depth == depthFull
	// Output and part choices live outside the record's text, so a record
	// whose text does not change still needs the layout redone when they
	// do.
	changed := false
	for _, r := range m.toolDisclosures.all() {
		for i := range r.rows {
			row := &r.rows[i]
			want := false
			if v, ok := s.sections[row.sectionKey]; ok {
				want = v.outputExpanded
			}
			if row.outputExpanded != want {
				row.outputExpanded, changed = want, true
			}
		}
		if v, ok := s.sections[toolSectionKey(r)]; ok && r.expanded != v.open {
			r.expanded, changed = v.open, true
		}
		m.refreshToolDisclosureWithAnchor(r, false)
	}
	for _, r := range m.reasoningRecords.all() {
		v, ok := s.sections[r.sectionKey]
		r.ensureSegments()
		for i := range r.segments {
			want := full
			if ok && i < len(v.full) {
				want = v.full[i]
			}
			if r.segments[i].full != want {
				r.segments[i].full, changed = want, true
			}
		}
		if ok && r.expanded != v.open {
			// The bounded tail lives in the transcript entry; the dirty sweep
			// re-renders it at the pane's width before the view paints, so
			// the row never opens with nothing under it.
			r.expanded, r.dirty = v.open, true
		}
	}
	if changed {
		m.visual.invalidate()
	}
}
