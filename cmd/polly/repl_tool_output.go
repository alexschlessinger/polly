package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/markdown"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/messages"
	rw "github.com/mattn/go-runewidth"
)

const inlineOutputBytes = 256 << 10
const inlineOutputLines = 500

// Output snapshots are immutable, so a cached agent projection can share them
// with its source without sharing expansion or in-flight load state.
type inlineToolOutput struct {
	body, language, text string
	artifact             *artifacts.Ref
	loaded               bool
	// Only one bounded page is retained. Offsets refer to the recorded bytes,
	// before display cleanup or JSON formatting; previous holds page starts.
	offset, end int64
	previous    []int64
	more        bool
	empty       bool // the page holds no text
	notice      string
	retry       int
}

func newInlineToolOutput(call messages.ChatMessageToolCall, result messages.ChatMessage) *inlineToolOutput {
	o := &inlineToolOutput{body: result.GetContent(), loaded: true}
	if call.Name == "read_file" || call.Name == "read" {
		if file := fileSummaryOf(call); file != nil {
			if lexer := lexers.Match(file.path); lexer != nil {
				o.language = lexer.Config().Name
			}
		}
	}
	for _, part := range result.Parts {
		if part.Artifact != nil && part.Artifact.Kind == artifacts.KindText {
			ref := *part.Artifact
			o.artifact, o.loaded = &ref, false
			break
		}
	}
	o.setPage(o.body)
	return o
}

func sameInlineToolOutput(a, b *inlineToolOutput) bool {
	if a == nil || b == nil || a.body != b.body || a.language != b.language {
		return false
	}
	if a.artifact == nil || b.artifact == nil {
		return a.artifact == b.artifact
	}
	return a.artifact.ID == b.artifact.ID
}

var toolOutputEscapes = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)

func cleanInlineToolOutput(body string) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	body = toolOutputEscapes.ReplaceAllString(body, "")
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, style.StripImageMarkers(body))
}

func formatInlineToolOutput(body, language string) string {
	body = cleanInlineToolOutput(body)
	if json.Valid([]byte(body)) {
		var text string
		if json.Unmarshal([]byte(body), &text) == nil {
			text = cleanInlineToolOutput(text)
			if inlineOutputFormattingFits(text) {
				body = text
			}
		} else if inlineJSONIndentFits(body) {
			var pretty bytes.Buffer
			if json.Indent(&pretty, []byte(body), "", "  ") == nil {
				body, language = pretty.String(), "json"
			}
		}
	}
	body = strings.TrimRight(body, "\n")
	if strings.TrimSpace(body) == "" {
		return style.Styled("No text output", "muted", "")
	}
	if language == "" && looksLikeUnifiedDiff(body) {
		return strings.Join(renderUnifiedDiffLines(body), "\n")
	}
	return strings.Join(markdown.HighlightCodeLines(body, language), "\n")
}

// JSON formatting is optional: when expansion would exceed a page's display
// budget, retain the complete raw JSON, including string escapes. Decoding a
// string is linear in the bounded source; account for four-column tab expansion
// before sending that decoded text to the code renderer.
func inlineOutputFormattingFits(body string) bool {
	body = strings.TrimRight(body, "\n")
	return len(body)+3*strings.Count(body, "\t") <= inlineOutputBytes && strings.Count(body, "\n")+1 <= inlineOutputLines
}

// inlineJSONIndentFits is a conservative upper bound for json.Indent with two
// spaces per level, computed BEFORE indentation can allocate quadratically.
// Input is valid JSON. Count structural newlines and their indentation outside
// strings, retaining source whitespace in the estimate (even though Indent
// removes it). Empty containers may overcount; falling back to raw is harmless.
func inlineJSONIndentFits(body string) bool {
	size, lines := len(body), strings.Count(body, "\n")+1
	depth, quoted, escaped := 0, false, false
	for i := 0; i < len(body); i++ {
		c := body[i]
		if quoted {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				quoted = false
			}
			continue
		}
		switch c {
		case '"':
			quoted = true
		case '[', '{':
			depth++
			size += 1 + 2*depth
			lines++
		case ']', '}':
			depth--
			size += 1 + 2*depth
			lines++
		case ',':
			size += 1 + 2*depth
			lines++
		case ':':
			size++
		}
		if size > inlineOutputBytes || lines > inlineOutputLines {
			return false
		}
	}
	return size <= inlineOutputBytes && lines <= inlineOutputLines
}

// inlineOutputPageEnd always advances, even through a single enormous line.
// Prefer whole lines; keep UTF-8 and CRLF intact when a line must split.
// The lookahead byte determines
// whether another page exists without treating the saved preview as the end.
func inlineOutputPageEnd(body string) int {
	end := min(len(body), inlineOutputBytes)
	if end < len(body) {
		for end > 0 && !utf8.RuneStart(body[end]) {
			end--
		}
		if end == 0 { // Malformed UTF-8 must not prevent progress either.
			end = inlineOutputBytes
		}
		if end > 0 && body[end-1] == '\r' && body[end] == '\n' {
			end--
		}
	}
	lines, lastNewline := 0, 0
	for i := 0; i < end; i++ {
		switch body[i] {
		case '\r':
			// Cleanup displays a lone CR as a newline too. CRLF is one
			// indivisible break, not two lines or two pages.
			if i+1 < end && body[i+1] == '\n' {
				i++
			}
		case '\n':
		default:
			continue
		}
		lastNewline = i + 1
		lines++
		if lines == inlineOutputLines {
			return i + 1
		}
	}
	if end < len(body) && lastNewline > 0 {
		return lastNewline
	}
	return end
}

func (o *inlineToolOutput) setPage(body string) {
	end := inlineOutputPageEnd(body)
	o.end, o.more = o.offset+int64(end), end < len(body)
	o.empty = strings.TrimSpace(cleanInlineToolOutput(body[:end])) == ""
	o.text = formatInlineToolOutput(body[:end], o.language)
	o.notice = ""
}

func (o *inlineToolOutput) pageRequest(direction int) *inlineToolOutput {
	next := *o
	switch {
	case direction > 0 && o.more:
		next.previous = append(append([]int64(nil), o.previous...), o.offset)
		next.offset = o.end
	case direction < 0 && len(o.previous) > 0:
		next.offset = o.previous[len(o.previous)-1]
		next.previous = append([]int64(nil), o.previous[:len(o.previous)-1]...)
	}
	return &next
}

type inlineOutputControl struct{ action, label string }

func (row toolDisclosureRow) inlineOutputControls() []inlineOutputControl {
	o := row.output
	if o == nil || row.outputLoading || !o.loaded {
		return nil
	}
	if o.notice != "" {
		return []inlineOutputControl{{"retry", "retry"}}
	}
	var controls []inlineOutputControl
	if len(o.previous) > 0 {
		controls = append(controls, inlineOutputControl{"previous", "previous page"})
	}
	if o.more {
		controls = append(controls, inlineOutputControl{"next", "next page"})
	}
	return controls
}

// inlineOutputLink is a page control on the controls row under the body:
// its action, and where its label sits in the row's body columns.
type inlineOutputLink struct {
	action  string
	x, cols int
}

// inlineOutputSection is what an open row shows under its command: one
// muted label row with the facts about the output ("output · bytes
// 1–19,498"), the body, then a row where a reader of a long page arrives:
// the page controls ("previous page · next page"), or "loading…" while a
// page is read, so nothing above the reader changes under them. Each line
// sits behind toolBodyIndent. A page with no text says so on the label row
// and has no body.
func (row toolDisclosureRow) inlineOutputSection() (label, body, controls string, links []inlineOutputLink) {
	o := row.output
	var parts []string
	switch {
	case o == nil && !row.settled:
		parts = []string{"running…"}
	case o == nil:
		parts = []string{"output unavailable in saved history"}
	default:
		parts = []string{"output"}
		if o.loaded && (o.offset > 0 || o.more) {
			parts = append(parts, fmt.Sprintf("bytes %s–%s", groupDigits(o.offset+1), groupDigits(o.end)))
		}
		if o.loaded && o.empty && o.notice == "" {
			parts[0] = "no output"
		} else {
			body = o.text
		}
		if o.notice != "" {
			body += "\n" + style.Styled(o.notice, "muted", "")
		}
	}
	label = style.Styled(strings.Join(parts, " · "), "muted", "")
	if row.outputLoading {
		return label, body, style.Styled("loading…", "muted", ""), nil
	}
	plain := ""
	for _, control := range row.inlineOutputControls() {
		if plain != "" {
			plain += " · "
			controls += style.Styled(" · ", "muted", "")
		}
		links = append(links, inlineOutputLink{action: control.action, x: rw.StringWidth(plain), cols: rw.StringWidth(control.label)})
		plain += control.label
		controls += style.Link(control.label)
	}
	if body != "" {
		body = toolBodyIndent + strings.ReplaceAll(strings.TrimPrefix(body, "\n"), "\n", "\n"+toolBodyIndent)
	}
	return label, body, controls, links
}

// groupDigits writes n with thousands separators.
func groupDigits(n int64) string {
	digits := fmt.Sprint(n)
	var b strings.Builder
	for i, r := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (row toolDisclosureRow) hasInlineEditDiff() bool {
	name := compactToolName(row.toolName)
	return row.changeText != "" && (name == "edit" || name == "write")
}

type toolOutputTarget struct {
	recordID   int64
	key        string
	action     string // empty toggles the row; otherwise a page control
	line, cols int
	x          int // columns in from the rail; the row itself starts at it
}

type toolOutputLink struct {
	recordID   int64
	key        string
	action     string // empty toggles the row; otherwise a page control
	X, Y, Cols int
	Rows       int // header/control and its body; the click target stays one row
}

func (l toolOutputLink) rect() image.Rectangle { return image.Rect(l.X, l.Y, l.X+l.Cols, l.Y+1) }

func (m *replModel) visibleToolOutputLinks(v transcriptViewport, x int) []toolOutputLink {
	var links []toolOutputLink
	offset := 0
	for _, block := range m.visual.blocks {
		for _, link := range block.toolOutputLinks {
			if link.key != "" && link.Cols > 0 && v.contains(offset+link.Y) {
				link.X += x
				link.Y = v.screenY(offset + link.Y)
				links = append(links, link)
			}
		}
		offset += len(block.rows)
	}
	return links
}

func (m *replModel) toolOutputRow(link toolOutputLink) (*toolDisclosureRecord, *toolDisclosureRow) {
	record := m.toolDisclosures.get(link.recordID)
	if record != nil && record.expanded {
		for i := range record.rows {
			row := &record.rows[i]
			if row.sectionKey == link.key && !row.isAgent() {
				return record, row
			}
		}
	}
	return nil, nil
}

// The caller holds the main model lock. Pane projections and view state are
// event-loop owned. Filesystem reads happen only on the background worker.
func (r *managedREPL) toggleToolOutputAt(m *replModel, point image.Point, target *viewTarget) bool {
	for _, link := range m.toolOutputLinks {
		if !point.In(link.rect()) {
			continue
		}
		return r.activateToolOutput(m, link, target)
	}
	return false
}

func (r *managedREPL) activateToolOutput(m *replModel, link toolOutputLink, target *viewTarget) bool {
	switch link.action {
	case earlierRowsAction:
		return r.revealToolRows(m, link, target)
	case earlierLinesAction:
		return r.revealThoughtLines(m, link, target)
	}
	record, row := m.toolOutputRow(link)
	if row == nil {
		return false
	}
	var state *viewState
	if target != nil {
		state = r.workspace().viewState(*target)
		m.followBottom, m.scrollAnchor = state.follow, state.top
	}
	direction := 0
	if link.action != "" {
		if row.output == nil || !row.outputExpanded || row.outputLoading {
			return true
		}
		switch link.action {
		case "previous":
			if len(row.output.previous) == 0 {
				return true
			}
			direction = -1
		case "next":
			if !row.output.more {
				return true
			}
			direction = 1
		case "retry":
			direction = row.output.retry
		default:
			return false
		}
	}
	m.mutateAnchored(m.disclosureLayoutWidth(0), matchToolGroup([]int64{record.id}), func(bool) {
		if link.action == "" {
			row.outputExpanded = !row.outputExpanded
		}
		if row.outputExpanded && !row.hasInlineEditDiff() && row.output != nil && !row.outputLoading {
			if row.output.artifact != nil && (!row.output.loaded || link.action != "") {
				r.loadInlineToolOutput(m, link, row, state, direction)
			} else if link.action != "" {
				next := row.output.pageRequest(direction)
				next.setPage(next.body[next.offset:])
				row.output = next
			}
		}
		m.refreshToolDisclosureWithAnchor(record, false)
		m.visual.invalidate()
	})
	if state != nil {
		state.top, state.lastRows = m.scrollAnchor, -1
		rememberViewSections(m, state)
	}
	return true
}

// revealToolRows shows every tool row of the activity line the link's
// record belongs to, in place of the window. The reveal is the line's: every
// record the line merged takes the flag, so it survives the line's layout
// being rebuilt from any of them.
func (r *managedREPL) revealToolRows(m *replModel, link toolOutputLink, target *viewTarget) bool {
	ids := []int64{link.recordID}
	for _, block := range m.visual.blocks {
		if slices.Contains(block.toolDisclosureIDs, link.recordID) {
			ids = append(ids[:0], block.toolDisclosureIDs...)
			break
		}
	}
	found := false
	for _, id := range ids {
		if record := m.toolDisclosures.get(id); record != nil && record.expanded {
			found = true
		}
	}
	if !found {
		return false
	}
	var state *viewState
	if target != nil {
		state = r.workspace().viewState(*target)
		m.followBottom, m.scrollAnchor = state.follow, state.top
	}
	m.mutateAnchored(m.disclosureLayoutWidth(0), matchToolGroup(ids), func(bool) {
		for _, id := range ids {
			if record := m.toolDisclosures.get(id); record != nil {
				record.allRows = true
			}
		}
		m.visual.invalidate()
	})
	if state != nil {
		state.top, state.lastRows = m.scrollAnchor, -1
		rememberViewSections(m, state)
	}
	return true
}

// revealThoughtLines shows every retained row of the thought on the
// activity line the link's record belongs to, in place of the window.
func (r *managedREPL) revealThoughtLines(m *replModel, link toolOutputLink, target *viewTarget) bool {
	ids := []int64{link.recordID}
	for _, block := range m.visual.blocks {
		if slices.Contains(block.reasoningIDs, link.recordID) {
			ids = append(ids[:0], block.reasoningIDs...)
			break
		}
	}
	found := false
	for _, id := range ids {
		if record := m.reasoningRecords.get(id); record != nil && record.expanded {
			found = true
		}
	}
	if !found {
		return false
	}
	var state *viewState
	if target != nil {
		state = r.workspace().viewState(*target)
		m.followBottom, m.scrollAnchor = state.follow, state.top
	}
	m.mutateAnchored(m.disclosureLayoutWidth(0), matchReasoningGroup(ids), func(bool) {
		for _, id := range ids {
			if record := m.reasoningRecords.get(id); record != nil {
				record.allLines = true
			}
		}
		m.visual.invalidate()
	})
	if state != nil {
		state.top, state.lastRows = m.scrollAnchor, -1
		rememberViewSections(m, state)
	}
	return true
}

// Store.Open is streaming (not seekable). Skip older pages without retaining
// them; cancellation is checked on every read, including the discard pass.
// The requested page uses at most one byte of lookahead beyond its byte budget.
func readInlineToolOutput(ctx context.Context, store artifacts.Store, source *inlineToolOutput, direction int) *inlineToolOutput {
	next := source.pageRequest(direction)
	next.loaded = true
	reader, err := store.Open(ctx, source.artifact.ID)
	if err == nil {
		stream := inlineOutputReader{ctx: ctx, Reader: reader}
		if next.offset > 0 {
			_, err = io.CopyN(io.Discard, stream, next.offset)
		}
		var data []byte
		if err == nil {
			data, err = io.ReadAll(io.LimitReader(stream, inlineOutputBytes+1))
		}
		closeErr := reader.Close()
		if err == nil {
			err = closeErr
		}
		if err == nil {
			next.setPage(string(data))
		}
	}
	if err != nil {
		return unavailableInlineToolOutput(source, direction)
	}
	return next
}

type inlineOutputReader struct {
	io.Reader
	ctx context.Context
}

func (r inlineOutputReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

func unavailableInlineToolOutput(source *inlineToolOutput, direction int) *inlineToolOutput {
	next := *source
	next.loaded, next.retry = true, direction
	next.notice = "Output page unavailable; showing previous page or saved preview"
	return &next
}

func (r *managedREPL) loadInlineToolOutput(m *replModel, link toolOutputLink, row *toolDisclosureRow, state *viewState, direction int) {
	source, store, workspace := row.output, m.artifactStore, r.workspace()
	if store == nil {
		row.output = unavailableInlineToolOutput(source, direction)
		return
	}
	// A view instance survives projection replacement and cache retirement.
	// Capture that owner, not whichever inspector happens to be selected when
	// the read completes (nor the obsolete model that started the read).
	var view *viewInstance
	if current := workspace.inspector.current; state != nil && current != nil && current.model == m {
		view = current
	}
	row.outputLoading = true
	if !r.background(func() {
		next := readInlineToolOutput(r.work.ctx, store, source, direction)
		r.postUI(r.work.ctx, func() {
			model, owner := m, state
			if view != nil {
				model, owner = view.model, nil
				// The shared cache can transfer the instance to another workspace.
				// Only its actual current owner may re-anchor or fence an in-flight
				// projection; a cached instance has no active navigation state.
				for _, tab := range r.tabs {
					if w := tab.workspace; w != nil && w.inspector.current == view {
						owner = w.states[w.inspector.target.key()]
						break
					}
				}
			}
			if model == nil {
				return
			}
			model.mu.Lock()
			defer model.mu.Unlock()
			if owner != nil {
				model.followBottom, model.scrollAnchor = owner.follow, owner.top
			}
			// The exact source snapshot rejects newer results and page requests,
			// including a replacement execution with a reused tool identity.
			if installInlineToolOutput(model, link.key, source, next) && owner != nil {
				owner.top, owner.lastRows = model.scrollAnchor, -1
				owner.revision++
			}
		})
	}) {
		row.outputLoading = false
	}
}

// Detailed disclosures can open an entire history, but only outputs intersecting
// the settled viewport need their artifacts. Use the cached body spans, not the
// clickable headers: a preview may be visible with its header above the pane.
// The caller may lay out the Loading line once more, without starting more reads.
func (r *managedREPL) ensureInlineToolOutputs(m *replModel, target *viewTarget, viewport transcriptViewport) bool {
	if viewport.end <= viewport.start {
		return false
	}
	var state *viewState
	if target != nil {
		state = r.workspace().viewState(*target)
	}
	// Index each intersecting record once rather than searching its rows for
	// every link. Dense batches must stay linear in their number of outputs.
	rowsByRecord := make(map[int64]map[string]*toolDisclosureRow)
	changed := make(map[int64]*toolDisclosureRecord)
	offset := 0
	for _, block := range m.visual.blocks {
		end := offset + len(block.rows)
		if offset >= viewport.end {
			break
		}
		if end <= viewport.start {
			offset = end
			continue
		}
		for _, link := range block.toolOutputLinks {
			start := offset + link.Y
			if link.action != "" || start >= viewport.end || start+max(1, link.Rows) <= viewport.start {
				continue
			}
			record := m.toolDisclosures.get(link.recordID)
			if record == nil || !record.expanded {
				continue
			}
			rows, ok := rowsByRecord[record.id]
			if !ok {
				rows = make(map[string]*toolDisclosureRow, len(record.rows))
				for i := range record.rows {
					row := &record.rows[i]
					if !row.isAgent() {
						rows[row.sectionKey] = row
					}
				}
				rowsByRecord[record.id] = rows
			}
			row := rows[link.key]
			if row == nil || !row.outputExpanded || row.hasInlineEditDiff() || row.output == nil || row.output.artifact == nil || row.output.loaded || row.outputLoading {
				continue
			}
			source := row.output
			r.loadInlineToolOutput(m, link, row, state, 0)
			if row.outputLoading || row.output != source {
				changed[record.id] = record
			}
		}
		offset = end
	}
	for _, record := range changed {
		m.refreshToolDisclosureWithAnchor(record, false)
	}
	if len(changed) == 0 {
		return false
	}
	m.visual.invalidate()
	if state != nil {
		rememberViewSections(m, state)
	}
	return true
}

type inlineToolOutputSnapshot struct {
	output  *inlineToolOutput
	loading bool
}

func inlineToolOutputSnapshots(m *replModel) map[string]inlineToolOutputSnapshot {
	if m == nil {
		return nil
	}
	outputs := make(map[string]inlineToolOutputSnapshot)
	for _, record := range m.toolDisclosures.all() {
		for _, row := range record.rows {
			if row.output != nil {
				outputs[row.sectionKey] = inlineToolOutputSnapshot{row.output, row.outputLoading}
			}
		}
	}
	return outputs
}

// Reuse loaded bodies through refreshes, inside the budgeted conversation
// model. Navigation state retains only which rows are open.
func carryInlineToolOutputs(m *replModel, previous map[string]inlineToolOutputSnapshot) {
	if m == nil {
		return
	}
	for _, record := range m.toolDisclosures.all() {
		for i := range record.rows {
			row := &record.rows[i]
			if old, ok := previous[row.sectionKey]; ok && sameInlineToolOutput(row.output, old.output) {
				row.output, row.outputLoading = old.output, old.loading
			}
		}
	}
}

func installInlineToolOutput(m *replModel, key string, source, next *inlineToolOutput) bool {
	for _, record := range m.toolDisclosures.all() {
		for i := range record.rows {
			row := &record.rows[i]
			if row.sectionKey == key && row.output == source {
				m.mutateAnchored(m.disclosureLayoutWidth(0), matchToolGroup([]int64{record.id}), func(bool) {
					row.output, row.outputLoading = next, false
					m.refreshToolDisclosureWithAnchor(record, false)
					m.visual.invalidate()
				})
				return true
			}
		}
	}
	return false
}
