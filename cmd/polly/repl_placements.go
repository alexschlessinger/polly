package main

import (
	"maps"
	"slices"
	"strings"
)

// Placements: mapping visible disclosure rows to screen cells and toggling them.

// The default keeps the newest activity line open and the rest folded, with
// manual choices intact, until Ctrl-O selects a level. The explicit levels are
// sticky for blocks that arrive later.
type disclosureMode uint8

const (
	disclosuresDefault disclosureMode = iota
	disclosuresFolded
	disclosuresOpen
	disclosuresDetailed
)

// next is an inspected view's cycle: opened, opened with details, folded.
func (mode disclosureMode) next() disclosureMode {
	switch mode {
	case disclosuresOpen:
		return disclosuresDetailed
	case disclosuresDetailed:
		return disclosuresFolded
	default:
		return disclosuresOpen
	}
}

// nextInline is the conversation's cycle: the newest line open (the
// default), every line open, every line open with details, every line
// folded, and the default again.
func (mode disclosureMode) nextInline() disclosureMode {
	switch mode {
	case disclosuresDefault:
		return disclosuresOpen
	case disclosuresOpen:
		return disclosuresDetailed
	case disclosuresDetailed:
		return disclosuresFolded
	default:
		return disclosuresDefault
	}
}

// disclosureKinds lists the clickable activity kinds: the activity line is
// one control, so there is one.
var disclosureKinds = [...]activityKind{activityAll}

// placeDisclosures records every kind's hit-testable controls for the frame
// just laid out against viewport.
func (m *replModel) placeDisclosures(v transcriptViewport) {
	for _, kind := range disclosureKinds {
		m.disclosurePlacements[kind] = m.visibleDisclosurePlacements(v, kind)
	}
}

// visibleDisclosurePlacements projects one kind of activity control into
// absolute screen cells for mouse hit-testing.
func (m *replModel) visibleDisclosurePlacements(v transcriptViewport, kind activityKind) []disclosurePlacement {
	// Only the block's activity controls are click targets. Truncation may
	// leave no fully visible control; the header never stands in for one.
	var placements []disclosurePlacement
	rowOffset := 0
	for _, block := range m.visual.blocks {
		row := rowOffset
		rowOffset += len(block.rows)
		if !block.isActivity() || len(block.rows) == 0 {
			continue
		}
		// The keyboard names a control by its record: a tool disclosure
		// when the line has one, else its reasoning record, negated so the
		// two spaces never collide.
		recordID := int64(0)
		if len(block.toolDisclosureIDs) > 0 {
			recordID = block.toolDisclosureIDs[0]
		} else if len(block.reasoningIDs) > 0 {
			recordID = -block.reasoningIDs[0]
		}
		for _, field := range block.activityFields {
			if field.kind != kind || field.X >= v.width || !v.contains(row+field.Y) {
				continue
			}
			placements = append(placements, disclosurePlacement{
				recordID:   recordID,
				recordIDs:  append([]int64(nil), block.toolDisclosureIDs...),
				thoughtIDs: append([]int64(nil), block.reasoningIDs...),
				X:          field.X,
				Y:          v.screenY(row + field.Y),
				Cols:       min(field.Cols, v.width-field.X),
			})
		}
	}
	return placements
}

// toggleDisclosureAt toggles the activity control under screen cell (x, y),
// if any, laid out at width. Returns whether a control was hit.
func (m *replModel) toggleDisclosureAt(x, y, width int) bool {
	for _, kind := range disclosureKinds {
		for _, p := range m.disclosurePlacements[kind] {
			if y != p.Y || x < p.X || x >= p.X+p.Cols {
				continue
			}
			if kind == activityAll {
				return m.toggleActivityLine(p.thoughtIDs, p.recordIDs, width)
			}
			ids := p.recordIDs
			if len(ids) == 0 {
				ids = []int64{p.recordID}
			}
			return m.toggleDisclosureGroup(kind, ids, width)
		}
	}
	return false
}

// toggleActivityLine opens or folds everything one activity line carries:
// its reasoning, tool rows, agents and images, as one control. The line
// points down whenever any of them is open, so its first click folds the
// whole line; only an entirely folded line opens. Row outputs keep their
// own state. width is as for toggleDisclosureGroup.
func (m *replModel) toggleActivityLine(thoughtIDs, toolIDs []int64, width int) bool {
	anyExpanded, found := false, false
	for _, id := range thoughtIDs {
		if expanded, ok := disclosureOps[activityThought].expanded(m, id); ok {
			anyExpanded, found = anyExpanded || expanded, true
		}
	}
	for _, kind := range []activityKind{activityTools, activityAgents, activityImages} {
		for _, id := range toolIDs {
			if expanded, ok := disclosureOps[kind].expanded(m, id); ok {
				anyExpanded, found = anyExpanded || expanded, true
			}
		}
	}
	if !found {
		return false
	}
	expand := !anyExpanded
	m.applyDisclosureToggle(width, matchActivityGroup(thoughtIDs, toolIDs), func(layoutWidth int, held bool) {
		disclosureOps[activityTools].apply(m, toolIDs, expand, layoutWidth, held)
		disclosureOps[activityThought].apply(m, thoughtIDs, expand, layoutWidth, held)
		disclosureOps[activityAgents].apply(m, toolIDs, expand, layoutWidth, held)
		disclosureOps[activityImages].apply(m, toolIDs, expand, layoutWidth, held)
	})
	return true
}

func activityGroupContains(blockIDs, ids []int64) bool {
	if len(ids) == 0 {
		return false
	}
	for _, id := range ids {
		if !slices.Contains(blockIDs, id) {
			return false
		}
	}
	return true
}

// disclosureKindOps is how one activity kind reads and writes its expansion
// state. expanded reports a record's state and whether it takes part in this
// kind at all; apply writes the new state to every participating record and
// re-lays-out what changed.
type disclosureKindOps struct {
	expanded func(m *replModel, id int64) (expanded, ok bool)
	apply    func(m *replModel, ids []int64, expand bool, width int, held bool)
	match    func(ids []int64) func(*transcriptVisualBlock) bool
}

var disclosureOps = map[activityKind]disclosureKindOps{
	activityThought: {
		expanded: func(m *replModel, id int64) (bool, bool) {
			record := m.reasoningRecords.get(id)
			return record != nil && record.expanded, record != nil
		},
		apply: func(m *replModel, ids []int64, expand bool, width int, held bool) {
			// Apply the new state to the whole group before refreshing any
			// record: each refresh re-lays-out the merged activity row, so
			// refreshing mid-loop would render intermediate frames from a
			// half-toggled group.
			var changed []*reasoningRecord
			for _, id := range ids {
				record := m.reasoningRecords.get(id)
				// A folded thought forgets its reveal, like a folded line
				// its rows.
				if !expand && record.allLines {
					record.allLines = false
					m.visual.invalidate()
				}
				if record.expanded != expand {
					record.expanded = expand
					changed = append(changed, record)
				}
			}
			for _, record := range changed {
				m.refreshReasoningRecordWithAnchor(record, width, !held)
			}
		},
		match: matchReasoningGroup,
	},
	activityTools: {
		expanded: func(m *replModel, id int64) (bool, bool) {
			record := m.toolDisclosures.get(id)
			return record != nil && record.expanded, record != nil
		},
		apply: func(m *replModel, ids []int64, expand bool, _ int, held bool) {
			for _, id := range ids {
				record := m.toolDisclosures.get(id)
				// A folded line forgets its reveal: opening shows the
				// window again.
				if !expand && record.allRows {
					record.allRows = false
					m.visual.invalidate()
				}
				if record.expanded != expand {
					m.setToolDisclosureExpanded(record, expand, !held)
				}
			}
		},
		match: matchToolGroup,
	},
	activityAgents: {
		expanded: func(m *replModel, id int64) (bool, bool) {
			record := m.toolDisclosures.get(id)
			return record != nil && record.agentsExpanded, record != nil && m.agentCounts([]int64{id}).Total > 0
		},
		apply: func(m *replModel, ids []int64, expand bool, _ int, _ bool) {
			for _, id := range ids {
				m.toolDisclosures.get(id).agentsExpanded = expand
			}
			m.visual.invalidate()
		},
		match: matchToolGroup,
	},
	activityImages: {
		expanded: func(m *replModel, id int64) (bool, bool) {
			record := m.toolDisclosures.get(id)
			return record != nil && record.imagesExpanded, record != nil && len(m.toolInspectionImages([]int64{id})) > 0
		},
		apply: func(m *replModel, ids []int64, expand bool, _ int, _ bool) {
			for _, id := range ids {
				m.toolDisclosures.get(id).imagesExpanded = expand
			}
			m.visual.invalidate()
		},
		match: matchToolGroup,
	},
}

// toggleDisclosureGroup expands or collapses the records in ids that take
// part in kind, as one control. The group header points down whenever any
// member is expanded, so its first click collapses the whole group; only an
// entirely closed group expands on click. width is the layout width when
// known (mouse clicks and shortcuts), else 0 for the last renderer width.
func (m *replModel) toggleDisclosureGroup(kind activityKind, ids []int64, width int) bool {
	ops := disclosureOps[kind]
	anyExpanded := false
	valid := make([]int64, 0, len(ids))
	for _, id := range ids {
		if expanded, ok := ops.expanded(m, id); ok {
			anyExpanded = anyExpanded || expanded
			valid = append(valid, id)
		}
	}
	if len(valid) == 0 {
		return false
	}
	expand := !anyExpanded
	m.applyDisclosureToggle(width, ops.match(valid), func(layoutWidth int, held bool) {
		ops.apply(m, valid, expand, layoutWidth, held)
	})
	return true
}

// applyDisclosureToggle records width as the last renderer width when known,
// then runs apply under the anchored mutation every disclosure toggle uses,
// at the width the transcript is laid out with. Caller must hold m.mu.
func (m *replModel) applyDisclosureToggle(width int, match func(*transcriptVisualBlock) bool, apply func(layoutWidth int, held bool)) {
	if width > 0 {
		m.reasoningWidth = width
	}
	layoutWidth := m.disclosureLayoutWidth(width)
	m.mutateAnchored(layoutWidth, match, func(held bool) { apply(layoutWidth, held) })
}

// toggleAllDisclosures cycles an inspected view: opened → opened with
// details → folded. cycleInlineDisclosures is the conversation's cycle, which
// adds the default (newest line open) after folded. Opened shows reasoning,
// tool/agent rows and media; details also shows outputs and diffs. Manual
// choices do not change the cycle, and new blocks inherit its selected
// level. width is 0 for the last renderer width. Caller holds m.mu.
func (m *replModel) toggleAllDisclosures(width int) bool {
	return m.cycleDisclosures(width, m.disclosures.next())
}

func (m *replModel) cycleInlineDisclosures(width int) bool {
	return m.cycleDisclosures(width, m.disclosures.nextInline())
}

func (m *replModel) cycleDisclosures(width int, level disclosureMode) bool {
	if !m.hasActivityRecords() {
		return false
	}
	m.disclosures = level
	// A whole-view toggle resizes every activity block at once, including
	// outputs whose enclosing group stays open, so anchor across all of them.
	m.applyDisclosureToggle(width, (*transcriptVisualBlock).isActivity, func(layoutWidth int, held bool) {
		m.applyDisclosureLevel(layoutWidth, held)
	})
	return true
}

// applyDisclosureLevel sets every record to the selected level: at the
// default the newest line open and the rest folded, at opened every line
// open, at detailed every row's output too. Caller holds m.mu, inside the
// anchored mutation when a frame is on screen.
func (m *replModel) applyDisclosureLevel(layoutWidth int, held bool) {
	expand, details := m.disclosures >= disclosuresOpen, m.disclosures == disclosuresDetailed
	var newestThoughts, newestTools map[int64]bool
	if m.disclosures == disclosuresDefault {
		newestThoughts, newestTools = m.newestLineRecords()
	}
	var openThoughts, closedThoughts, openTools, closedTools []int64
	for id, record := range m.toolDisclosures.all() {
		open := expand || newestTools[id]
		for i := range record.rows {
			row := &record.rows[i]
			row.outputExpanded = details && !row.isAgent()
		}
		record.allRows = details
		m.setToolDisclosureExpanded(record, open, !held)
		if open {
			openTools = append(openTools, id)
		} else {
			closedTools = append(closedTools, id)
		}
	}
	for id, record := range m.reasoningRecords.all() {
		record.allLines = details
		if expand || newestThoughts[id] {
			openThoughts = append(openThoughts, id)
		} else {
			closedThoughts = append(closedThoughts, id)
		}
	}
	disclosureOps[activityThought].apply(m, openThoughts, true, layoutWidth, held)
	disclosureOps[activityThought].apply(m, closedThoughts, false, layoutWidth, held)
	for _, kind := range []activityKind{activityAgents, activityImages} {
		disclosureOps[kind].apply(m, openTools, true, layoutWidth, held)
		disclosureOps[kind].apply(m, closedTools, false, layoutWidth, held)
	}
	m.visual.invalidate()
}

// newestTurnStart is the transcript index of the newest prompt; the newest
// activity line is whatever follows it.
func (m *replModel) newestTurnStart() int {
	for i := len(m.transcript) - 1; i >= 0; i-- {
		if strings.HasPrefix(m.transcript[i].text, userGutter()) {
			return i
		}
	}
	return 0
}

func (m *replModel) hasActivityRecords() bool {
	for range m.reasoningRecords.all() {
		return true
	}
	for range m.toolDisclosures.all() {
		return true
	}
	return false
}

// newestLineRecords are the reasoning and tool records of the newest line:
// the turn holding the newest record, which a prompt awaiting its first
// activity has not yet replaced.
func (m *replModel) newestLineRecords() (thoughts, tools map[int64]bool) {
	thoughts, tools = map[int64]bool{}, map[int64]bool{}
	last := -1
	for _, record := range m.reasoningRecords.all() {
		last = max(last, record.transcriptIndex)
	}
	for _, record := range m.toolDisclosures.all() {
		last = max(last, record.transcriptIndex)
	}
	if last < 0 {
		return thoughts, tools
	}
	// Prompts can be echoed while an earlier turn still runs. Only actual
	// turn ownership groups its records, including activity on both sides of
	// a queued echo (even after that prompt is activated or discarded).
	turn := m.transcript[last].activityTurn
	for id, record := range m.reasoningRecords.all() {
		if m.transcript[record.transcriptIndex].activityTurn == turn {
			thoughts[id] = true
		}
	}
	for id, record := range m.toolDisclosures.all() {
		if m.transcript[record.transcriptIndex].activityTurn == turn {
			tools[id] = true
		}
	}
	return thoughts, tools
}

// newRecordOpen is the state a record created now starts in: the selected
// level's, or at the default the newest line's, which is open unless this
// turn's line was folded by hand. Hydration decides for the whole transcript
// once it is complete.
func (m *replModel) newRecordOpen() bool {
	if m.disclosures != disclosuresDefault {
		return m.disclosures >= disclosuresOpen
	}
	if m.hydrating {
		return false
	}
	// Prose can split a turn into several lines with independent manual
	// choices. Inherit the latest record, regardless of its activity kind,
	// rather than letting an earlier tool override the current thought.
	last, open := -1, true
	for _, id := range m.turnToolDisclosureIDs {
		if record := m.toolDisclosures.get(id); record != nil && record.transcriptIndex > last {
			last, open = record.transcriptIndex, record.expanded
		}
	}
	for _, id := range m.turnReasoningIDs {
		if record := m.reasoningRecords.get(id); record != nil && record.transcriptIndex > last {
			last, open = record.transcriptIndex, record.expanded
		}
	}
	return open
}

// foldNewestLine folds the line of the turn that just ended as the next one
// begins: at the default level the newest line is the open one, and lines
// opened by hand further up stay as they are. Caller holds m.mu.
func (m *replModel) foldNewestLine() {
	if m.disclosures != disclosuresDefault {
		return
	}
	thoughts, tools := m.newestLineRecords()
	if len(thoughts)+len(tools) == 0 {
		return
	}
	thoughtIDs, toolIDs := slices.Collect(maps.Keys(thoughts)), slices.Collect(maps.Keys(tools))
	m.applyDisclosureToggle(0, (*transcriptVisualBlock).isActivity, func(layoutWidth int, held bool) {
		disclosureOps[activityThought].apply(m, thoughtIDs, false, layoutWidth, held)
		for _, kind := range []activityKind{activityTools, activityAgents, activityImages} {
			disclosureOps[kind].apply(m, toolIDs, false, layoutWidth, held)
		}
	})
}

// openNewestLine applies the default level to a freshly hydrated transcript:
// its newest line open, the rest folded. No frame is on screen yet, so
// nothing needs anchoring.
func (m *replModel) openNewestLine() {
	if m.disclosures != disclosuresDefault {
		return
	}
	m.applyDisclosureLevel(max(80, m.reasoningWidth), true)
}

// openImageAt opens the transcript thumbnail under the given screen cell, if
// any, in the OS image viewer. Placements come from the last rendered frame;
// the embedded splash logo (no backing file) is skipped. Caller must hold m.mu.
func (r *managedREPL) openImageAt(x, y int) {
	if r.openImage == nil {
		return
	}
	for _, p := range r.model.imagePlacements {
		if p.Path == "" || x < p.X || x >= p.X+p.Cols || y < p.Y || y >= p.Y+p.Rows {
			continue
		}
		if err := r.openImage(p.Path); err != nil {
			r.model.appendNoticeLine("Could not open the image · " + err.Error())
		}
		return
	}
}
