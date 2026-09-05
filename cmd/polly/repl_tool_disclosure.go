package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
)

// Tool disclosures: the collapsible activity rows for tool calls.

// toolPreviewRows bounds an expanded tool disclosure the same way
// reasoningPreviewLines bounds thinking: the newest rows, everywhere the
// disclosure renders. Earlier rows collapse into one elision line; the header
// keeps the full count.
const toolPreviewRows = 5

// activeTool is one still-executing tool call, pinned to its disclosure row.
type activeTool struct {
	id      string
	row     int
	label   string
	started time.Time
	// child is the screen model of the subagent tab this call spawned, so
	// the row can show the child's progress; nil for other tools. childName
	// is that tab's name.
	child       *replModel
	childName   string
	childStatus string
}

// currentToolDisclosure returns the disclosure currently receiving rows, or —
// once a batch has settled mid-turn — the turn's most recent disclosure, so
// callers inspecting the just-completed batch still find it. Caller must hold
// m.mu.
func (m *replModel) currentToolDisclosure() *toolDisclosureRecord {
	if m.turnToolDisclosureID != 0 {
		return m.toolDisclosures[m.turnToolDisclosureID]
	}
	if n := len(m.turnToolDisclosureIDs); n > 0 {
		return m.toolDisclosures[m.turnToolDisclosureIDs[n-1]]
	}
	return nil
}

func (m *replModel) ensureToolDisclosure() *toolDisclosureRecord {
	// Only the live pointer receives new rows: once a batch settles, the next
	// batch opens a fresh disclosure at its own transcript position.
	if record := m.toolDisclosures[m.turnToolDisclosureID]; record != nil {
		return record
	}
	m.toolDisclosureSeq++
	record := &toolDisclosureRecord{id: m.toolDisclosureSeq, transcriptIndex: len(m.transcript)}
	m.toolDisclosures[record.id] = record
	m.turnToolDisclosureID = record.id
	m.turnToolDisclosureIDs = append(m.turnToolDisclosureIDs, record.id)
	m.appendLine("")
	record.transcriptIndex = len(m.transcript) - 1
	m.toolDisclosureAt[record.transcriptIndex] = record.id
	return record
}

func (m *replModel) appendToolStartRow(id, label string) *toolDisclosureRecord {
	label = stripTranscriptImageMarkers(label)
	record := m.ensureToolDisclosure()
	row := len(record.rows)
	record.rows = append(record.rows, toolDisclosureRow{
		callID: id,
		label:  label,
		line:   runningToolLine(label, 0),
	})
	if len(m.activeTools) == 0 {
		// A fresh batch restarts the pulse clock; drop the drained batch's
		// phase so a bucket collision cannot skip the first repaint.
		m.activeToolsPhase = -1
	}
	m.activeTools = append(m.activeTools, activeTool{
		id:      id,
		row:     row,
		label:   label,
		started: time.Now(),
	})
	return record
}

func toolDisclosureHeader(total int, expanded, complete bool) string {
	glyph := "▸"
	if expanded {
		glyph = "▾"
	}
	return "  " + inlineActivityControl(glyph, turnToolLabel(total), complete)
}

func toolDisclosureText(record *toolDisclosureRecord) (string, []transcriptImage) {
	if record == nil {
		return "", nil
	}
	header := toolDisclosureHeader(len(record.rows), record.expanded, record.complete)
	if !record.expanded {
		return header, nil
	}
	var b strings.Builder
	b.WriteString(header)
	var images []transcriptImage
	seen := make(map[string]struct{})
	rows := record.rows
	if len(rows) > toolPreviewRows {
		b.WriteString("\n  ")
		b.WriteString(styled(fmt.Sprintf("… %d earlier", len(rows)-toolPreviewRows), "muted", ""))
		rows = rows[len(rows)-toolPreviewRows:]
	}
	for _, row := range rows {
		if row.line == "" {
			continue
		}
		b.WriteByte('\n')
		b.WriteString(row.line)
		appendToolDisclosureImages(&b, &images, row.images, "    ", seen)
	}
	return b.String(), images
}

func transcriptImageIdentity(img transcriptImage) string {
	if img.Path != "" {
		return img.Path
	}
	return img.DisplayPath + "\x00" + img.Alt
}

func appendToolDisclosureImages(b *strings.Builder, rendered *[]transcriptImage, candidates []transcriptImage, prefix string, seen map[string]struct{}) {
	remaining := maxTranscriptImagesPerBlock - len(*rendered)
	if remaining <= 0 || len(candidates) == 0 {
		return
	}
	selected := make([]transcriptImage, 0, min(len(candidates), remaining))
	for _, img := range candidates {
		identity := transcriptImageIdentity(img)
		if _, duplicate := seen[identity]; duplicate {
			continue
		}
		seen[identity] = struct{}{}
		selected = append(selected, img)
		if len(selected) == remaining {
			break
		}
	}
	if len(selected) == 0 {
		return
	}
	block := offsetTranscriptImageMarkers(renderTranscriptImages(selected, prefix), len(*rendered))
	if block == "" {
		return
	}
	b.WriteByte('\n')
	b.WriteString(block)
	*rendered = append(*rendered, selected...)
}

func (m *replModel) toolInspectionImages(ids []int64) []transcriptImage {
	images := make([]transcriptImage, 0)
	for _, id := range ids {
		record := m.toolDisclosures[id]
		if record == nil {
			continue
		}
		for _, row := range record.rows {
			for _, img := range row.inspectionImages {
				images = append(images, img)
				if len(images) == maxTranscriptImagesPerBlock {
					return images
				}
			}
		}
	}
	return images
}

func (m *replModel) toolInspectionExpanded(ids []int64) bool {
	for _, id := range ids {
		if record := m.toolDisclosures[id]; record != nil && record.imagesExpanded {
			return true
		}
	}
	return false
}

func (m *replModel) refreshToolDisclosure(record *toolDisclosureRecord) {
	m.refreshToolDisclosureWithAnchor(record, true)
}

func (m *replModel) refreshToolDisclosureWithAnchor(record *toolDisclosureRecord, reanchor bool) {
	if record == nil || record.transcriptIndex < 0 || record.transcriptIndex >= len(m.transcript) {
		return
	}
	index := record.transcriptIndex
	text, images := toolDisclosureText(record)
	if m.transcript[index].text == text && transcriptImagesEqual(m.transcript[index].images, images) {
		return
	}
	// Tool activity renders inline in the transcript, so updates re-anchor a
	// held viewport like any other transcript mutation.
	if !reanchor {
		m.setTranscriptEntry(index, text, images)
		return
	}
	m.mutateAnchored(m.disclosureLayoutWidth(0), matchToolGroup([]int64{record.id}), func(bool) {
		m.setTranscriptEntry(index, text, images)
	})
}

// completeToolDisclosure settles the live tool disclosure. A deliberately
// expanded disclosure stays open until the turn settles; the next batch opens
// a fresh disclosure at its own transcript position. Caller must hold m.mu.
func (m *replModel) completeToolDisclosure() {
	if record := m.toolDisclosures[m.turnToolDisclosureID]; record != nil {
		record.complete = true
		m.refreshToolDisclosure(record)
	}
	m.turnToolDisclosureID = 0
}

// collapseTurnToolDisclosures auto-collapses every disclosure of the turn at
// settlement. Caller must hold m.mu.
func (m *replModel) collapseTurnToolDisclosures() {
	for _, id := range m.turnToolDisclosureIDs {
		if record := m.toolDisclosures[id]; record != nil {
			changed := record.expanded || record.imagesExpanded
			record.expanded = false
			record.imagesExpanded = false
			if changed {
				m.refreshToolDisclosure(record)
				// Image expansion is derived by the shared activity layout rather
				// than stored in the raw tool entry, so force that projection closed.
				m.visual.invalidate()
			}
		}
	}
}

// resetToolDisclosure drops the active pointer while leaving the previous
// turn's disclosure in scrollback. Caller must hold m.mu.
func (m *replModel) resetToolDisclosure() {
	if record := m.currentToolDisclosure(); record != nil && record.transcriptIndex < 0 {
		delete(m.toolDisclosures, record.id)
	}
	m.turnToolDisclosureID = 0
}

func (m *replModel) clearToolDisclosures() {
	m.toolDisclosures = make(map[int64]*toolDisclosureRecord)
	m.toolDisclosureAt = make(map[int]int64)
	m.toolDisclosureSeq = 0
	m.turnToolDisclosureID = 0
	m.turnToolDisclosureIDs = nil
	m.toolDisclosurePlacements = nil
	m.imageDisclosurePlacements = nil
}

// arrowPulse breathes the running-tool arrow between two brightnesses of one
// themed hue: it alternates the modifier (bold ↔ dim) on a fixed color so the
// arrow gently pulses while a tool executes — and follows the terminal theme.
var arrowPulse = []string{"bold", "dim"}

// arrowPulsePeriod is how long each pulse shade holds; len(arrowPulse) steps
// make one full breath (~1s), slow enough to read as a pulse, not a strobe.
const arrowPulsePeriod = 500 * time.Millisecond

// runningToolLine renders a still-executing tool entry: a breathing arrow whose
// modifier is chosen from elapsed time, the label, and a live elapsed timer.
func runningToolLine(label string, elapsed time.Duration) string {
	label = stripTranscriptImageMarkers(label)
	mod := arrowPulse[int(elapsed/arrowPulsePeriod)%len(arrowPulse)]
	return "  " + styled("→", "run", mod) + " " +
		styledToolText(label) + " " +
		styled("· "+formatElapsed(elapsed), "muted", "")
}

func (m *replModel) toggleToolDisclosure(recordID int64) bool {
	record := m.toolDisclosures[recordID]
	if record == nil || record.transcriptIndex < 0 || record.transcriptIndex >= len(m.transcript) {
		return false
	}
	record.expanded = !record.expanded
	m.refreshToolDisclosure(record)
	return true
}

// refreshActiveTools updates each running disclosure row with the current
// breathing-arrow frame and live elapsed time. The arrow pulses every
// arrowPulsePeriod and the timer rolls each second, so the disclosure is
// refreshed whenever either boundary has crossed since the last paint —
// comparing row text alone would freeze the timer for the ~1s stretches
// where formatElapsed's output is unchanged. The header is always visible,
// so this repaints whether or not the detail rows are expanded.
func (m *replModel) refreshActiveTools() {
	if len(m.activeTools) == 0 {
		return
	}
	record := m.currentToolDisclosure()
	if record == nil {
		return
	}
	// The fastest-changing element is the 500ms arrow pulse; repaint once per
	// pulse phase. Elapsed-time changes are subsumed by that finer cadence.
	phase := int(time.Since(m.activeTools[0].started) / arrowPulsePeriod)
	if phase == m.activeToolsPhase {
		return
	}
	m.activeToolsPhase = phase
	for i := range m.activeTools {
		at := &m.activeTools[i]
		if at.row < 0 || at.row >= len(record.rows) {
			continue
		}
		label := at.label
		if at.child != nil {
			label += " · " + at.childName
			if activity, ok := childActivity(at.child); ok {
				at.childStatus = activity
			}
			if at.childStatus != "" {
				label += " · " + at.childStatus
			}
		}
		record.rows[at.row].line = runningToolLine(label, time.Since(at.started))
	}
	m.refreshToolDisclosure(record)
}

// takeActiveTool stops tracking a finished tool and returns its disclosure row.
// It matches by call ID, falling back to the oldest still-running row when an
// ID is absent. Caller must hold m.mu.
func (m *replModel) takeActiveTool(id string) (int, bool) {
	if len(m.activeTools) == 0 {
		return -1, false
	}
	pick := 0
	for i, at := range m.activeTools {
		if at.id == id {
			pick = i
			break
		}
	}
	row := m.activeTools[pick].row
	m.activeTools = append(m.activeTools[:pick], m.activeTools[pick+1:]...)
	return row, true
}

func (m *replModel) settleActiveTools(reason string) {
	if len(m.activeTools) == 0 {
		return
	}
	record := m.currentToolDisclosure()
	if record == nil {
		return
	}
	for _, at := range m.activeTools {
		if at.row < 0 || at.row >= len(record.rows) {
			continue
		}
		row := &record.rows[at.row]
		row.line = toolErrorLine(at.label, reason, "")
		row.images = nil
		row.settled = true
	}
	if record.expanded {
		m.refreshToolDisclosure(record)
	}
}

// toolOKLine / toolDeniedLine / toolErrorLine build the final transcript entry
// for a completed tool call. They return the styled string (rather than
// appending) so AppendToolEnd can freeze it over the running line in place.

// styledToolText protects arbitrary labels from gotui's inline-style parser.
// A command may contain an unmatched bracket (for example, grep "["), which
// would otherwise keep the parser nested and expose this row's style markup as
// literal text. Tool rows always flow through parseStyledCells, which restores
// these temporary bracket runes after assigning the intended style.
func styledToolText(text string) string {
	return styled(stripTranscriptImageMarkers(text), "muted", "")
}

func toolOKLine(label, duration, meta string) string {
	return "  " + styled("✓", "ok", "bold") + " " + styledToolText(toolLineBody(duration, label, meta))
}

func toolDeniedLine(label string) string {
	return "  " + styled("✗", "err", "bold") + " " + styledToolText("denied "+label)
}

// toolErrorLine renders a failed tool call as a red ✗ plus the muted metadata
// (timing · command · exit code) — the same shape as a success line. The
// tool's own output/error text is deliberately not shown; the model still
// receives the full output, this is display only.
func toolErrorLine(label, duration, meta string) string {
	return "  " + styled("✗", "err", "bold") + " " + styledToolText(toolLineBody(duration, label, meta))
}

func hydratedToolLine(label string, msg messages.ChatMessage) string {
	label = stripTranscriptImageMarkers(label)
	if toolWasDenied(msg.Content) {
		return toolDeniedLine(label)
	}
	if msg.IsError() {
		return toolErrorLine(label, "", "failed")
	}
	if succeeded, known := msg.ToolSucceeded(); known {
		if succeeded {
			return toolOKLine(label, "", "")
		}
		return toolErrorLine(label, "", "failed")
	}
	return pendingToolLine(label)
}

// pendingToolLine is the row for a tool call whose outcome is not (yet) known.
func pendingToolLine(label string) string {
	return "  " + styled("·", "muted", "bold") + " " + styledToolText(label)
}

func (m *replModel) appendCompletedToolDisclosure(rows []toolDisclosureRow) *toolDisclosureRecord {
	if len(rows) == 0 {
		return nil
	}
	m.toolDisclosureSeq++
	record := &toolDisclosureRecord{
		id:              m.toolDisclosureSeq,
		transcriptIndex: len(m.transcript),
		rows:            append([]toolDisclosureRow(nil), rows...),
		complete:        true,
	}
	for i := range record.rows {
		record.rows[i].label = stripTranscriptImageMarkers(record.rows[i].label)
		if len(record.rows[i].images) == 0 {
			record.rows[i].line = stripTranscriptImageMarkers(record.rows[i].line)
		}
		if record.rows[i].line == "" {
			record.rows[i].line = pendingToolLine(record.rows[i].label)
		}
	}
	m.appendLine("")
	record.transcriptIndex = len(m.transcript) - 1
	m.toolDisclosures[record.id] = record
	m.toolDisclosureAt[record.transcriptIndex] = record.id
	m.refreshToolDisclosure(record)
	return record
}

func (m *replModel) toolDisclosureRowForCall(callID string) (*toolDisclosureRecord, *toolDisclosureRow) {
	for i := len(m.turnToolDisclosureIDs) - 1; i >= 0; i-- {
		record := m.toolDisclosures[m.turnToolDisclosureIDs[i]]
		if record == nil {
			continue
		}
		for rowIndex := len(record.rows) - 1; rowIndex >= 0; rowIndex-- {
			if callID != "" && record.rows[rowIndex].callID != callID {
				continue
			}
			return record, &record.rows[rowIndex]
		}
	}
	return nil, nil
}
