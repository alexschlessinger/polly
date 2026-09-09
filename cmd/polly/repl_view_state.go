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

type viewSection struct {
	tools, images, agents, thought bool
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
	return r.rows[0].inspectionKey
}

func rememberViewSections(m *replModel, s *viewState) {
	if s.sections == nil {
		s.sections = make(map[string]viewSection)
	}
	for _, r := range m.toolDisclosures.all() {
		if key := toolSectionKey(r); key != "" {
			s.sections[key] = viewSection{tools: r.expanded, images: r.imagesExpanded, agents: r.agentsExpanded}
		}
	}
	for _, r := range m.reasoningRecords.all() {
		if r.inspectionKey != "" {
			s.sections[r.inspectionKey] = viewSection{thought: r.expanded}
		}
	}
	s.revision++
}

func applyViewSections(m *replModel, s viewState) {
	for _, r := range m.toolDisclosures.all() {
		if v, ok := s.sections[toolSectionKey(r)]; ok {
			r.expanded, r.imagesExpanded, r.agentsExpanded = v.tools, v.images, v.agents
			m.refreshToolDisclosureWithAnchor(r, false)
		}
	}
	for _, r := range m.reasoningRecords.all() {
		if v, ok := s.sections[r.inspectionKey]; ok {
			r.expanded = v.thought
			r.dirty = true
		}
	}
}
