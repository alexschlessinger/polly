package main

import (
	"fmt"
	"strings"
)

// The abridged log: an open line's steps as one list, in the order the turn
// ran them, one entry per thought row and per call. Adjacent entries of one
// kind are a run, across step boundaries, and a run shows its newest
// activityRunRows entries, and every call of it still running; the area
// then shows the newest activityTailRows of what is left, so entries leave
// only off its top or from inside a run that outgrew its rows. One blank row,
// counted against the area, separates thoughts from calls.
//
// Every abridged cut, the log's and its line's agents' and images', reads
// the same: a muted "… " in the rail of the row after the hidden ones. A
// click on it opens the cut thing whole in place, and a click in the rail
// of the opened thing's first row cuts it again.

// activityRunRows is how many entries a run of thought rows or calls shows.
const activityRunRows = 4

// openCutAction is the output-link action of a cut's control; the link's
// key names the cut. A thought-anchored key starts "t:".
const openCutAction = "cut"

type logEntry struct {
	thought bool
	text    string // a thought row
	call    activityRowPart
	record  int64 // the entry's record
	step    *activityStep
	key     string
	run     int
	visible bool
	counted bool
}

// activityLog lists the steps' entries in order and groups them into runs,
// returning each run's first entry index.
func activityLog(steps []*activityStep) ([]logEntry, []int) {
	var log []logEntry
	for _, step := range steps {
		for _, part := range step.thoughts {
			for i, line := range textRows(part.lines) {
				log = append(log, logEntry{thought: true, text: line, record: part.record.id, step: step, key: fmt.Sprintf("t:%d:%d:%d", part.record.id, part.index, i)})
			}
		}
		for _, part := range step.rows {
			log = append(log, logEntry{call: part, record: part.record.id, step: step, key: fmt.Sprintf("c:%d:%s", part.record.id, part.row.sectionKey)})
		}
	}
	var starts []int
	for i := range log {
		if i == 0 || log[i].thought != log[i-1].thought {
			starts = append(starts, i)
		}
		log[i].run = len(starts) - 1
	}
	return log, starts
}

// activityLogBody lays an abridged line's steps out as the log.
func (m *replModel) activityLogBody(steps []*activityStep, rowWidth int) activityBody {
	log, starts := activityLog(steps)
	if len(log) == 0 {
		return activityBody{}
	}
	runOpen := func(run int) bool { return m.openCuts[log[starts[run]].key] }
	tailKey := log[0].key + ":tail"
	tailOpen := m.openCuts[tailKey]
	runEnd := func(run int) int {
		if run+1 < len(starts) {
			return starts[run+1]
		}
		return len(log)
	}
	// A run shows its newest entries and its running calls; an open one
	// shows every entry, its older ones taking none of the area's rows.
	for run, start := range starts {
		end, open := runEnd(run), runOpen(run)
		for i := start; i < end; i++ {
			newest := end-i <= activityRunRows
			running := !log[i].thought && !log[i].call.row.settled
			log[i].visible = open || newest || running
			log[i].counted = newest || (running && !open)
		}
	}
	// The blank row between a thought and a call takes one of the area's
	// rows like an entry does.
	counted, below, tailCut := 0, -1, false
	for i := len(log) - 1; i >= 0; i-- {
		if !log[i].visible {
			continue
		}
		cost := 0
		if log[i].counted {
			cost++
		}
		if below >= 0 && log[below].thought != log[i].thought {
			cost++
		}
		if counted+cost > activityTailRows {
			tailCut = true
			if tailOpen {
				break
			}
			// An open run the cut lands in stays whole: opening it asked
			// for every row, which takes rows beyond the area's.
			for j := i; j >= 0; j-- {
				if log[j].run != log[i].run || !runOpen(log[j].run) {
					log[j].visible = false
				}
			}
			break
		}
		counted += cost
		below = i
	}

	var lines []string
	var thoughtLines []int
	var targets []toolOutputTarget
	labelled := map[*activityStep]bool{}
	// One blank row sets thoughts apart from calls, and an opened call from
	// whatever follows it; neighbours of one kind touch.
	openAbove, above := false, -1
	for i, entry := range log {
		if !entry.visible {
			continue
		}
		if openAbove || (above >= 0 && log[above].thought != entry.thought) {
			lines = append(lines, "")
		}
		top := above < 0
		openAbove, above = false, i
		// A step's start sits on its first visible entry with room for it.
		right := ""
		if entry.step.known && !labelled[entry.step] {
			right = stepOffsetLabel(entry.step.at)
		}
		// The row after hidden ones marks the cut. Hidden entries of its
		// own run are its run's, which an open run never has, so the area's
		// cut is the top row's only when it falls between runs; the top row
		// of an opened area closes it.
		runKey, runStart := log[starts[entry.run]].key, i == starts[entry.run]
		key, cut := "", false
		switch {
		case i > 0 && log[i-1].run == entry.run && !log[i-1].visible:
			key, cut = runKey, true
		case top && tailCut && (tailOpen || i > 0):
			key, cut = tailKey, !tailOpen
		case runStart && runOpen(entry.run) && runEnd(entry.run)-i > activityRunRows:
			key = runKey
		}
		if entry.thought {
			text := reasoningBlockIndent + entry.text
			if right != "" {
				if padded, ok := padStepOffset(text, right, rowWidth); ok {
					text, labelled[entry.step] = padded, true
				}
			}
			// A thought row has no click of its own: the whole row is
			// its cut's control.
			if key != "" {
				targets = append(targets, toolOutputTarget{recordID: entry.record, key: key, action: openCutAction, line: len(lines), x: -activityRailCols, cols: rowWidth - 2 + activityRailCols})
			}
			if cut {
				text = reasoningCutIndent + strings.TrimPrefix(text, reasoningBlockIndent)
			}
			thoughtLines = append(thoughtLines, len(lines))
			lines = append(lines, text)
			continue
		}
		chunk := inlineToolRowChunk(entry.call.row, rowWidth, m.toolBaseDir, entry.call.record.id, right)
		if right != "" {
			labelled[entry.step] = true
		}
		rows := strings.Split(chunk.text, "\n")
		// A call's row opens its output, so only the rail is its cut's.
		if key != "" {
			targets = append(targets, toolOutputTarget{recordID: entry.record, key: key, action: openCutAction, line: len(lines), x: -activityRailCols, cols: activityRailCols})
		}
		if cut {
			rows[0] = reasoningCutIndent + strings.TrimPrefix(rows[0], activityIndent)
		}
		for _, target := range chunk.targets {
			target.line += len(lines)
			targets = append(targets, target)
		}
		lines = append(lines, rows...)
		openAbove = chunk.open
	}
	body := activityBody{text: strings.Join(lines, "\n"), targets: targets}
	thought := make([]string, 0, len(thoughtLines))
	for _, line := range thoughtLines {
		thought = append(thought, lines[line])
	}
	body.thought = strings.Join(thought, "\n")
	return body
}

// toggleCut opens an abridged cut whole, or cuts it again, in place.
// Caller holds m.mu.
func (m *replModel) toggleCut(link toolOutputLink) bool {
	if link.key == "" {
		return false
	}
	match := matchToolGroup([]int64{link.recordID})
	if strings.HasPrefix(link.key, "t:") {
		match = matchReasoningGroup([]int64{link.recordID})
	}
	m.mutateAnchored(m.disclosureLayoutWidth(0), match, func(bool) {
		if m.openCuts == nil {
			m.openCuts = map[string]bool{}
		}
		if m.openCuts[link.key] {
			delete(m.openCuts, link.key)
		} else {
			m.openCuts[link.key] = true
		}
		m.visual.invalidate()
	})
	return true
}
