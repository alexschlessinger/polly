package main

import (
	"maps"
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
// them: a row's output, and for a thought which of its parts show their
// text. Chunk folds are kept beside them, by chunk key.
type viewSection struct {
	outputExpanded bool
	open           []bool
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
		open := make([]bool, len(r.segments))
		for i, segment := range r.segments {
			open[i] = segment.open
		}
		s.sections[r.sectionKey] = viewSection{open: open}
	}
	s.chunks = maps.Clone(m.chunkFolds)
	s.runtimeFolds = m.rememberRuntimeFolds(s.runtimeFolds)
	s.revision++
}

// visitRuntimeFoldChunks visits only lines carrying evidence for a runtime
// fold handoff. A line may hold several disclosures; build its chunks once.
func (m *replModel) visitRuntimeFoldChunks(match func(*toolDisclosureRow) bool, visit func(*activityChunk)) {
	seen := map[int64]bool{}
	for _, record := range m.toolDisclosures.all() {
		if seen[record.id] {
			continue
		}
		for i := range record.rows {
			if !match(&record.rows[i]) {
				continue
			}
			chunks, _, tools := m.lineChunks(record.transcriptIndex)
			for _, id := range tools {
				seen[id] = true
			}
			for i := range chunks {
				visit(&chunks[i])
			}
			break
		}
	}
}

func (m *replModel) rememberRuntimeFolds(previous map[string]bool) map[string]bool {
	if len(m.chunkFolds) == 0 || len(previous) == 0 && len(m.activeTools) == 0 {
		return nil
	}
	// Keep absent calls until the next projection can reconcile them. Use
	// copy-on-write so an in-flight inspector projection owns its snapshot.
	hints := maps.Clone(previous)
	match := func(row *toolDisclosureRow) bool {
		_, tracked := previous[row.displayID]
		return row.displayID != "" && (row.runtimeOnly || tracked)
	}
	m.visitRuntimeFoldChunks(match, func(c *activityChunk) {
		open, manual := m.chunkFolds[c.key]
		for _, p := range c.parts {
			if p.tools == nil || !match(&p.tools.rows[p.row]) {
				continue
			}
			id := p.tools.rows[p.row].displayID
			if manual {
				if hints == nil {
					hints = make(map[string]bool)
				}
				hints[id] = open
			} else {
				delete(hints, id)
			}
		}
	})
	return hints
}

func (m *replModel) projectedChunkFolds(s viewState) map[string]bool {
	folds := maps.Clone(s.chunks)
	if len(folds) == 0 || len(s.runtimeFolds) == 0 {
		return folds
	}
	match := func(row *toolDisclosureRow) bool {
		_, ok := s.runtimeFolds[row.displayID]
		return ok
	}
	m.visitRuntimeFoldChunks(match, func(c *activityChunk) {
		if _, manual := folds[c.key]; manual {
			return // an explicit choice for the resulting chunk wins
		}
		for _, p := range c.parts {
			if p.tools != nil {
				if open, ok := s.runtimeFolds[p.tools.rows[p.row].displayID]; ok {
					folds[c.key] = open
					break
				}
			}
		}
	})
	return folds
}

// applyViewSections gives a projection the view's bulk state and the
// choices made in it: a chunk the view folded or opened by hand takes that
// fold and any other follows the bulk state; an output or a thought the view
// opened is open, and every other one stays closed.
func applyViewSections(m *replModel, s viewState) {
	// The bulk state, the folds and the part choices live outside the
	// records' text, so a record whose text does not change still needs the
	// layout redone when they do.
	folds := m.projectedChunkFolds(s)
	changed := m.depth != s.depth || !maps.Equal(m.chunkFolds, folds)
	m.depth, m.chunkFolds = s.depth, folds
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
		m.refreshToolDisclosureWithAnchor(r, false)
	}
	for _, r := range m.reasoningRecords.all() {
		v := s.sections[r.sectionKey]
		r.ensureSegments()
		for i := range r.segments {
			want := i < len(v.open) && v.open[i]
			if r.segments[i].open != want {
				// The bounded tail lives in the transcript entry; the dirty
				// sweep renders it at the pane's width before the view
				// paints, so the row never opens with nothing under it.
				r.segments[i].open, r.dirty, changed = want, true, true
			}
		}
	}
	if changed {
		m.visual.invalidate()
	}
}
