package main

import (
	"encoding/json"
	"fmt"
	"slices"
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
// it made carry the step's number, and an open chunk lays its steps out in
// that order, each step's thought above its calls, so the chunk reads as the
// turn happened rather than as thoughts, then calls, then agents. A step
// also carries when it began, as time since the prompt, when the clock is
// known: live from the attempt hook, on reload from the turn audit.

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

// inferNextTurnStep is the no-attempt-hook fallback. Once a tool batch has
// drained, fresh reasoning or a new batch is observable evidence of another
// response, even though we did not observe that response's start time.
func (m *replModel) inferNextTurnStep() {
	if m.turnSteps != 0 || m.runningTools != 0 {
		return
	}
	r := m.currentToolDisclosure()
	if r != nil && len(r.rows) > 0 && r.rows[len(r.rows)-1].step == m.turnStep {
		m.turnStep++
	}
}

// stepOffsetLabel is a step's start as the right column shows it: minutes
// and seconds since the prompt, hours in front past the first.
// stepColumn is the right column every row of an open chunk laid out at
// width keeps for its step's start: the widest label short of an hour, two
// columns clear of the row. Call rows fit short of it, so they all end at
// one edge. A narrow pane keeps the column's width for its rows and puts
// each start on the first row with room for it.
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
// newline before the text starts the segment after it. open is whether the
// part shows its text from the start, as it does while the reader follows
// the turn's thinking.
func (record *reasoningRecord) beginSegment(step int, at time.Duration, known bool, segmentBreak bool, open bool) {
	if n := len(record.segments); n > 0 && record.segments[n-1].step == step {
		return
	}
	start := len(record.tail)
	if segmentBreak && start > 0 && record.tail[start-1] != '\n' {
		start++
	}
	record.segments = append(record.segments, reasoningSegment{step: step, start: start, at: at, known: known, open: open})
}

// ensureSegments gives a record whose tail was appended without a step one
// segment for the whole of it, so its part can be addressed like any other.
func (record *reasoningRecord) ensureSegments() {
	if len(record.segments) == 0 {
		record.segments = []reasoningSegment{{}}
	}
}

// partOpen reports whether part i of the thought shows its text.
func (record *reasoningRecord) partOpen(i int) bool {
	return i >= 0 && i < len(record.segments) && record.segments[i].open
}

// anyOpen reports whether any part of the thought shows its text. Only then
// is the retained text worth rendering: a closed thought is one row.
func (record *reasoningRecord) anyOpen() bool {
	return slices.ContainsFunc(record.segments, func(s reasoningSegment) bool { return s.open })
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

// previewSegment is the rendered rows part i contributed; a record with no
// preview split yet owns one part of every row. A part the last render did
// not know has none until the next paint.
func (record *reasoningRecord) previewSegment(i int) []string {
	if len(record.previewSegments) == 0 {
		if i == 0 {
			return record.previewLines
		}
		return nil
	}
	if i < 0 || i >= len(record.previewSegments) {
		return nil
	}
	start := 0
	for j := 0; j < i; j++ {
		start += record.previewSegments[j]
	}
	end := min(len(record.previewLines), start+record.previewSegments[i])
	return record.previewLines[min(start, end):end]
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

// activityBody is an open chunk's body: the block-indented rows and the
// targets at their line offsets. open says the body ends in an opened detail
// (a thought's text, an output, a diff), which wants a row of air before
// whatever follows it.
type activityBody struct {
	text    string
	targets []toolOutputTarget
	open    bool
}

// stepStart is when the step that made part began, when that is known.
func (p chunkPart) stepStart() (time.Duration, bool) {
	if p.thought != nil {
		if p.segment < len(p.thought.segments) {
			return p.thought.segments[p.segment].at, p.thought.segments[p.segment].known
		}
		return 0, false
	}
	row := p.tools.rows[p.row]
	return row.stepAt, row.stepKnown
}

// chunkBody lays an open chunk's rows out under its header: one row for
// every recorded call, however many, and one for each thought, in the order
// the turn made them. Nothing else opens with the chunk. A thought's text,
// a call's output and an edit's diff show only where their own row was
// opened by a click, and a row carries no blank row above it unless the row
// before left a detail open. The first row of a step whose start is known
// carries that start, as time since the prompt, in the right column; calls
// requested together share it rather than each claiming a time.
func (m *replModel) chunkBody(c *activityChunk, width int) activityBody {
	rowWidth := activityRailContentWidth(width)
	var lines []string
	var targets []toolOutputTarget
	openAbove := false
	stamped := map[int]bool{}
	for _, p := range c.parts {
		right := ""
		if at, known := p.stepStart(); known && !stamped[p.step] {
			right = stepOffsetLabel(at)
		}
		if p.thought != nil {
			if openAbove {
				lines = append(lines, "")
				openAbove = false
			}
			label, live := m.thoughtLabel(p, false)
			row := reasoningBlockIndent + renderThoughtRow(fitLabel(headerText(label), max(1, rowWidth-len(reasoningBlockIndent)-2)), live)
			cols := style.TextWidth(row) - len(reasoningBlockIndent)
			if right != "" {
				if padded, ok := padStepOffset(row, right, rowWidth); ok {
					row = padded
					stamped[p.step] = true
				}
			}
			targets = append(targets, toolOutputTarget{recordID: p.thought.id, key: m.thoughtPartKey(p.thought, p.segment), action: thoughtPartAction, line: len(lines), cols: cols})
			lines = append(lines, row)
			if text := thoughtTextRows(p.thought, p.segment); len(text) > 0 {
				for _, t := range text {
					lines = append(lines, toolBodyIndent+t)
				}
				openAbove = true
			}
			continue
		}
		// An agent has no row here: the chunk lists its agents beneath.
		if p.ordinal < 0 || p.ordinal >= len(p.tools.displayRows) {
			continue
		}
		row := p.tools.displayRows[p.ordinal]
		if row.line == "" {
			continue
		}
		if openAbove {
			lines = append(lines, "")
		}
		chunk := inlineToolRowChunk(row, rowWidth, m.toolBaseDir, p.tools.id, right)
		if right != "" {
			stamped[p.step] = true
		}
		for _, target := range chunk.targets {
			target.line += len(lines)
			targets = append(targets, target)
		}
		lines = append(lines, strings.Split(chunk.text, "\n")...)
		openAbove = chunk.open
	}
	return activityBody{text: strings.Join(lines, "\n"), targets: targets, open: openAbove}
}

// thoughtTextRows is the text an opened thought part shows under its row:
// the retained, provider-supplied text of that one step, as last painted,
// behind a note when the retained text lost its beginning. A closed part,
// or one not painted yet, has none.
func thoughtTextRows(record *reasoningRecord, segment int) []string {
	if !record.partOpen(segment) {
		return nil
	}
	rows := proseRows(record.previewSegment(segment))
	if len(rows) > 0 && segment == 0 && record.tailCut {
		rows = append([]string{style.Styled("… earlier text not kept", "muted", "")}, rows...)
	}
	return rows
}

// thoughtPartAction is the output-link action of a thought row; the link's
// key names the part. A click shows the part's text or hides it again.
const thoughtPartAction = "thought"

// thoughtPartKey names one part of one thought. The record is in the key so
// that two thought rows on screen are two keyboard stops, not one.
func (m *replModel) thoughtPartKey(record *reasoningRecord, index int) string {
	return fmt.Sprintf("thought:%d/%s", index, m.thoughtSection(record))
}

// toggleThoughtPart shows or hides one thought part's text, in place, and
// nothing else: not the other parts of the same thought, not its
// neighbours. Opening the thought that is still streaming follows the
// turn's thinking: the turn's later thoughts open as they start, until the
// reader hides one or the turn ends. Caller holds m.mu.
func (m *replModel) toggleThoughtPart(link toolOutputLink) bool {
	record := m.reasoningRecords.get(link.recordID)
	var index int
	if record == nil {
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
		open := !record.segments[index].open
		record.segments[index].open = open
		if m.busy && slices.Contains(m.turnReasoningIDs, record.id) {
			switch {
			case !open:
				m.followThoughts = false
			case record.active && index == len(record.segments)-1:
				m.followThoughts = true
			}
		}
		// The text renders on the next paint; until then the row stands alone.
		m.markReasoningDirty(record)
	})
	return true
}

// openChunkOfThought opens the chunk a thought has just joined when that
// thought's text is showing, so a reader following the turn's thinking keeps
// the text in view when its calls arrive and fold it into a run. record is
// the tool record that took the step's calls. Caller holds m.mu.
func (m *replModel) openChunkOfThought(record *toolDisclosureRecord, step int) {
	if record == nil || record.transcriptIndex < 0 || record.transcriptIndex >= len(m.transcript) {
		return
	}
	chunks, thoughtIDs, toolIDs := m.lineChunks(record.transcriptIndex)
	for i := range chunks {
		c := &chunks[i]
		if c.group == chunkThought || m.chunkOpen(c.key) {
			continue
		}
		for _, p := range c.parts {
			if p.thought != nil && p.step == step && p.thought.partOpen(p.segment) {
				// A fold like any other, so a held viewport stays put.
				m.setChunkOpen(c.key, thoughtIDs, toolIDs, 0, true)
				break
			}
		}
	}
}

// proseRows is rows with a blank row kept only after a paragraph of more
// than one row: prose keeps its paragraph breaks, and the one-line
// summaries some models think in stack without gaps.
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

// padStepOffset puts right against the right edge of a block-indented row
// laid out at rowWidth, two columns clear of the text, or reports that the
// row has no room for it.
func padStepOffset(text, right string, rowWidth int) (string, bool) {
	pad := rowWidth - style.TextWidth(text) - rw.StringWidth(right)
	if pad < 2 {
		return text, false
	}
	return text + strings.Repeat(" ", pad) + style.Styled(right, "muted", ""), true
}
