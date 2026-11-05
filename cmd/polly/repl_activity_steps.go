package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/messages"
	rw "github.com/mattn/go-runewidth"
	ui "github.com/metaspartan/gotui/v5"
)

// Steps: a turn's activity by the model response that produced it.
//
// Every model response is one step. The thought it streamed and the calls
// it made carry the step's number, and the activity line lays the steps out
// in that order, each step's thought above its calls, so the line reads as
// the turn happened rather than as thoughts, then calls, then agents. A
// step also carries when it began, as time since the prompt, when the clock
// is known: live from the attempt hook, on reload from the turn audit.

// beginTurnStep opens the next step of the running turn. Caller must hold
// m.mu.
func (m *replModel) beginTurnStep() {
	m.turnStep = m.turnSteps
	m.turnSteps++
	m.turnStepKnown = !m.turnStarted.IsZero()
	m.turnStepAt = 0
	if m.turnStepKnown {
		m.turnStepAt = time.Since(m.turnStarted)
	}
}

// stepOffsetLabel is a step's start as the right column shows it: minutes
// and seconds since the prompt, hours in front past the first.
// stepColumn is the right column every row of an open line laid out at
// width keeps for its step's start: the widest label short of an hour, two
// columns clear of the row. Thought rows wrap short of it, so the start
// sits on a thought's first row; call rows fit short of it, so they all end
// at one edge. A narrow pane keeps the column's width for its rows and
// puts each start on the first row with room for it.
func stepColumn(width int) int {
	if width < stepColumnMinWidth {
		return 0
	}
	return 2 + len("00:00")
}

const stepColumnMinWidth = 64

func stepOffsetLabel(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d / time.Hour)
	mi := int(d % time.Hour / time.Minute)
	s := int(d % time.Minute / time.Second)
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, mi, s)
	}
	return fmt.Sprintf("%d:%02d", mi, s)
}

// beginSegment opens a segment for step at the tail's end, unless the
// newest segment is already that step's. A segment break that will put a
// newline before the text starts the segment after it.
func (record *reasoningRecord) beginSegment(step int, at time.Duration, known bool, segmentBreak bool, full bool) {
	if n := len(record.segments); n > 0 && record.segments[n-1].step == step {
		return
	}
	start := len(record.tail)
	if segmentBreak && start > 0 && record.tail[start-1] != '\n' {
		start++
	}
	record.segments = append(record.segments, reasoningSegment{step: step, start: start, at: at, known: known, full: full})
}

// ensureSegments gives a record whose tail was appended without a step one
// segment for the whole of it, so its part can be addressed like any other.
func (record *reasoningRecord) ensureSegments() {
	if len(record.segments) == 0 {
		record.segments = []reasoningSegment{{}}
	}
}

// trimSegments moves the segments back by the runes compaction dropped
// from the front of the tail; segments left with nothing merge into the
// first one that still owns text.
func (record *reasoningRecord) trimSegments(dropped int) {
	if dropped <= 0 || len(record.segments) == 0 {
		return
	}
	kept := record.segments[:0]
	for i, segment := range record.segments {
		segment.start -= dropped
		next := len(record.tail)
		if i+1 < len(record.segments) {
			next = record.segments[i+1].start - dropped
		}
		if next <= 0 && i+1 < len(record.segments) {
			continue // wholly dropped; the next segment owns the front
		}
		segment.start = max(0, segment.start)
		kept = append(kept, segment)
	}
	record.segments = kept
}

// segmentTexts is the retained thought split by step, in order: the whole
// tail when no step was recorded. The fence context a compaction kept
// belongs to the first part.
func (record *reasoningRecord) segmentTexts() []string {
	if len(record.segments) == 0 {
		return []string{record.tailPrefix + string(record.tail)}
	}
	texts := make([]string, len(record.segments))
	for i, segment := range record.segments {
		start := min(max(0, segment.start), len(record.tail))
		end := len(record.tail)
		if i+1 < len(record.segments) {
			end = min(max(start, record.segments[i+1].start), len(record.tail))
		}
		texts[i] = string(record.tail[start:end])
	}
	texts[0] = record.tailPrefix + texts[0]
	return texts
}

// previewSegment is the preview rows segment i contributed, with the
// segment's step facts; a record with no preview split yet owns one
// segment of every row.
func (record *reasoningRecord) previewSegment(i int) (lines []string, segment reasoningSegment) {
	if len(record.previewSegments) == 0 {
		if i == 0 {
			if len(record.segments) > 0 {
				segment = record.segments[0]
			}
			return record.previewLines, segment
		}
		return nil, reasoningSegment{}
	}
	start := 0
	for j := 0; j < i && j < len(record.previewSegments); j++ {
		start += record.previewSegments[j]
	}
	end := min(len(record.previewLines), start+record.previewSegments[i])
	if i < len(record.segments) {
		segment = record.segments[i]
	}
	return record.previewLines[min(start, end):end], segment
}

// previewSegmentCount is how many parts the preview is split into.
func (record *reasoningRecord) previewSegmentCount() int {
	if len(record.previewSegments) == 0 {
		return 1
	}
	return len(record.previewSegments)
}

// decodeTurnAudit reads a saved turn audit back from message metadata,
// whether it is still the typed record or the generic JSON a store returns.
func decodeTurnAudit(value any) (messages.TurnAudit, bool) {
	switch v := value.(type) {
	case nil:
		return messages.TurnAudit{}, false
	case messages.TurnAudit:
		return v, true
	case *messages.TurnAudit:
		if v == nil {
			return messages.TurnAudit{}, false
		}
		return *v, true
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return messages.TurnAudit{}, false
	}
	var audit messages.TurnAudit
	if err := json.Unmarshal(encoded, &audit); err != nil || audit.StartedAt.IsZero() {
		return messages.TurnAudit{}, false
	}
	return audit, true
}

// stepStartsFromAudit maps each step, by the display ID of the response
// that was that step, to how long after the turn began its first attempt
// started. Steps the audit does not name are left out.
func stepStartsFromAudit(audit messages.TurnAudit, stepIDs []string) map[int]time.Duration {
	starts := map[int]time.Duration{}
	byID := map[string]time.Time{}
	for _, attempt := range audit.Attempts {
		if attempt.ID == "" || attempt.StartedAt.IsZero() {
			continue
		}
		if at, ok := byID[attempt.ID]; !ok || attempt.StartedAt.Before(at) {
			byID[attempt.ID] = attempt.StartedAt
		}
	}
	for step, id := range stepIDs {
		if at, ok := byID[id]; ok && id != "" {
			starts[step] = max(0, at.Sub(audit.StartedAt))
		}
	}
	return starts
}

// activityBody is an open activity line's body: the block-indented rows,
// the thought rows among them, and the targets at their line offsets.
type activityBody struct {
	text    string
	thought string
	targets []toolOutputTarget
}

// activityStep is one model response as the body lays it out: the parts of
// the line's thoughts it produced, then the calls it made, and when it
// began where that is known.
type activityStep struct {
	step     int
	at       time.Duration
	known    bool
	thoughts []activityThoughtPart
	rows     []activityRowPart
}

type activityThoughtPart struct {
	record *reasoningRecord
	index  int
	lines  []string
	full   bool
}

type activityRowPart struct {
	record *toolDisclosureRecord
	row    toolDisclosureRow
}

// activitySteps groups the line's open records by step: every thought
// segment and every ordinary call row, in step order, records in transcript
// order within a step. Records never tagged with a step share step zero.
func (m *replModel) activitySteps(reasoningIDs, toolIDs []int64) []*activityStep {
	type ref struct {
		index     int
		reasoning *reasoningRecord
		tools     *toolDisclosureRecord
	}
	var refs []ref
	for _, id := range reasoningIDs {
		if record := m.reasoningRecords.get(id); record != nil && record.expanded {
			refs = append(refs, ref{index: record.transcriptIndex, reasoning: record})
		}
	}
	for _, id := range toolIDs {
		if record := m.toolDisclosures.get(id); record != nil && record.expanded {
			refs = append(refs, ref{index: record.transcriptIndex, tools: record})
		}
	}
	sort.SliceStable(refs, func(i, j int) bool { return refs[i].index < refs[j].index })
	var steps []*activityStep
	byNumber := map[int]*activityStep{}
	stepFor := func(number int, at time.Duration, known bool) *activityStep {
		step := byNumber[number]
		if step == nil {
			step = &activityStep{step: number}
			byNumber[number] = step
			steps = append(steps, step)
		}
		if known && !step.known {
			step.at, step.known = at, true
		}
		return step
	}
	for _, r := range refs {
		if r.reasoning != nil {
			if m.depth == depthCalls {
				continue // calls alone: no thinking
			}
			for i := range r.reasoning.previewSegmentCount() {
				lines, segment := r.reasoning.previewSegment(i)
				if len(lines) == 0 {
					continue
				}
				step := stepFor(segment.step, segment.at, segment.known)
				step.thoughts = append(step.thoughts, activityThoughtPart{record: r.reasoning, index: i, lines: lines, full: segment.full})
			}
			continue
		}
		for _, row := range r.tools.displayRows {
			if row.line == "" {
				continue
			}
			step := stepFor(row.step, row.stepAt, row.stepKnown)
			step.rows = append(step.rows, activityRowPart{record: r.tools, row: row})
		}
	}
	sort.SliceStable(steps, func(i, j int) bool { return steps[i].step < steps[j].step })
	return steps
}

// activityStepBody lays the line's steps out under it. An abridged line
// shows each step's thought as its newest reasoningPreviewLines rows of
// text, then the step's calls, and only the newest activityRowWindow calls
// of the line, the steps above them going whole; a step cut at its first
// call keeps the thought that led to it. A full line shows every row; a
// line at the calls depth shows no thought at all.
// A blank row separates a step that opens with a thought from the one
// before it, and the first row of a step whose start is known carries that
// start, as time since the prompt, in the right column.
func (m *replModel) activityStepBody(reasoningIDs, toolIDs []int64, width int) activityBody {
	steps := m.activitySteps(reasoningIDs, toolIDs)
	if len(steps) == 0 {
		return activityBody{}
	}
	rowWidth := activityRailContentWidth(width)
	var lines []string
	var thoughtLines []int
	var targets []toolOutputTarget
	type rowMark struct{ line, stepLine int }
	var marks []rowMark
	openAbove := false
	for i, step := range steps {
		// A thought is a heading for the calls under it and wants a blank
		// row above; a step of bare calls follows the one before it
		// directly, its start in the right column marking the boundary.
		if i > 0 && len(step.thoughts) > 0 {
			lines = append(lines, "")
			openAbove = false
		}
		stepLine := len(lines)
		right := ""
		if step.known {
			right = stepOffsetLabel(step.at)
		}
		// add appends one thought row, the step's start on the first
		// (or, past an hour, the first with room for the longer label).
		add := func(text string) {
			if right != "" {
				if padded, ok := padStepOffset(text, right, rowWidth); ok {
					text, right = padded, ""
				}
			}
			thoughtLines = append(thoughtLines, len(lines))
			lines = append(lines, text)
		}
		for _, part := range step.thoughts {
			if openAbove {
				lines = append(lines, "")
				openAbove = false
			}
			visible, text := part.lines, textRows(part.lines)
			overflow := len(text) > reasoningPreviewLines
			if part.full {
				if part.index == 0 && part.record.tailCut {
					add(reasoningBlockIndent + style.Styled("… earlier text not kept", "muted", ""))
				}
				visible = proseRows(part.lines)
			} else {
				// An abridged part is rows of text: the blank rows Markdown
				// puts between paragraphs return with the whole part.
				visible = text
				if overflow {
					visible = visible[len(visible)-reasoningPreviewLines:]
				}
			}
			// A part with more than its abridged rows is a control: a
			// click shows the whole of it, another abridges it again.
			if overflow {
				targets = append(targets, toolOutputTarget{recordID: part.record.id, key: thoughtPartKey(part.index), action: thoughtPartAction, line: len(lines), cols: rowWidth - 2})
			}
			// A cut part marks the rows above its first in the indent
			// column, which doubles as the sign that a click opens it.
			for i, line := range visible {
				indent := reasoningBlockIndent
				if i == 0 && overflow && !part.full {
					indent = reasoningCutIndent
				}
				add(indent + line)
			}
		}
		for _, part := range step.rows {
			if openAbove {
				lines = append(lines, "")
			}
			chunk := inlineToolRowChunk(part.row, rowWidth, m.toolBaseDir, part.record.id, right)
			right = ""
			marks = append(marks, rowMark{line: len(lines), stepLine: stepLine})
			for _, target := range chunk.targets {
				target.line += len(lines)
				targets = append(targets, target)
			}
			lines = append(lines, strings.Split(chunk.text, "\n")...)
			openAbove = chunk.open
		}
	}
	// The abridged window keeps the newest calls, and the steps above them
	// go whole; a step cut at its first call keeps the thought that led to
	// it.
	if hidden := len(marks) - activityRowWindow; hidden > 0 && m.depth != depthFull {
		mark := marks[hidden]
		cut := mark.line
		if marks[hidden-1].stepLine != mark.stepLine {
			cut = mark.stepLine
		}
		var windowed []toolOutputTarget
		for _, target := range targets {
			if target.line >= cut {
				target.line -= cut
				windowed = append(windowed, target)
			}
		}
		var thought []int
		for _, line := range thoughtLines {
			if line >= cut {
				thought = append(thought, line-cut)
			}
		}
		lines, targets, thoughtLines = lines[cut:], windowed, thought
	}
	body := activityBody{text: strings.Join(lines, "\n"), targets: targets}
	thought := make([]string, 0, len(thoughtLines))
	for _, line := range thoughtLines {
		thought = append(thought, lines[line])
	}
	body.thought = strings.Join(thought, "\n")
	return body
}

// reasoningCutIndent stands in for a cut thought part's indent on its first
// visible row; indentLines paints it in the rail column.
const reasoningCutIndent = "… "

// thoughtPartAction is the output-link action of a thought part's rows; the
// link's key names the part. A click shows the whole part or abridges it.
const thoughtPartAction = "thought"

func thoughtPartKey(index int) string { return fmt.Sprintf("thought:%d", index) }

// toggleThoughtPart flips a thought part between abridged and whole, in
// place. Caller holds m.mu.
func (m *replModel) toggleThoughtPart(link toolOutputLink) bool {
	record := m.reasoningRecords.get(link.recordID)
	var index int
	if record == nil || !record.expanded {
		return false
	}
	if _, err := fmt.Sscanf(link.key, "thought:%d", &index); err != nil {
		return false
	}
	record.ensureSegments()
	if index < 0 || index >= len(record.segments) {
		return false
	}
	m.mutateAnchored(m.disclosureLayoutWidth(0), matchReasoningGroup([]int64{record.id}), func(bool) {
		record.segments[index].full = !record.segments[index].full
		m.visual.invalidate()
	})
	return true
}

// proseRows is rows with a blank row kept only after a paragraph of more
// than one row: prose keeps its paragraph breaks, and the one-line
// summaries some models think in stack as they do abridged.
func proseRows(rows []string) []string {
	kept := make([]string, 0, len(rows))
	run := 0
	for _, row := range rows {
		if len(textRows([]string{row})) == 0 {
			if run > 1 {
				kept = append(kept, row)
			}
			run = 0
			continue
		}
		run++
		kept = append(kept, row)
	}
	for len(kept) > 0 && len(textRows(kept[len(kept)-1:])) == 0 {
		kept = kept[:len(kept)-1]
	}
	return kept
}

// textRows is rows without the blank ones.
func textRows(rows []string) []string {
	kept := make([]string, 0, len(rows))
	for _, row := range rows {
		if strings.TrimSpace(ui.CellsToString(style.ParseCells(row, ui.StyleClear))) != "" {
			kept = append(kept, row)
		}
	}
	return kept
}

// padStepOffset puts right against the right edge of a block-indented
// thought row laid out at rowWidth, two columns clear of the text, or
// reports that the row has no room for it.
func padStepOffset(text, right string, rowWidth int) (string, bool) {
	pad := rowWidth - style.TextWidth(text) - rw.StringWidth(right)
	if pad < 2 {
		return text, false
	}
	return text + strings.Repeat(" ", pad) + style.Styled(right, "muted", ""), true
}
