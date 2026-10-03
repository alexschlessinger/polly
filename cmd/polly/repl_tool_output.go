package main

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"io"
	"regexp"
	"strings"
	"unicode"

	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/markdown"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/messages"
)

const inlineOutputBytes = 256 << 10
const inlineOutputLines = 500

// Output snapshots are immutable, so a cached agent projection can share them
// with its source without sharing expansion or in-flight load state.
type inlineToolOutput struct {
	body, language, text string
	artifact             *artifacts.Ref
	loaded               bool
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
	o.text = formatInlineToolOutput(o.body, o.language)
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
	truncated := len(body) > inlineOutputBytes
	if truncated {
		body = body[:inlineOutputBytes]
	}
	if json.Valid([]byte(body)) {
		var text string
		if json.Unmarshal([]byte(body), &text) == nil {
			body = cleanInlineToolOutput(text)
		} else {
			var pretty bytes.Buffer
			if json.Indent(&pretty, []byte(body), "", "  ") == nil {
				body, language = pretty.String(), "json"
			}
		}
	}
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	if len(lines) > inlineOutputLines {
		lines, truncated = lines[:inlineOutputLines], true
	}
	body = strings.Join(lines, "\n")
	if strings.TrimSpace(body) == "" {
		return style.Styled("No text output", "muted", "")
	}
	text := strings.Join(markdown.HighlightCodeLines(body, language), "\n")
	if truncated {
		text += "\n" + style.Styled("… additional output omitted", "muted", "")
	}
	return text
}

func (row toolDisclosureRow) inlineOutputText() string {
	text := style.Styled("Output unavailable in saved history", "muted", "")
	switch {
	case row.outputLoading:
		text = style.Styled("Loading output…", "muted", "")
	case row.output != nil:
		text = row.output.text
	case !row.settled:
		text = style.Styled("Running…", "muted", "")
	}
	// Six columns leave two spaces inside the activity rail's four-column
	// indent, without adding another frame or a repeated section heading.
	return "      " + strings.ReplaceAll(text, "\n", "\n      ")
}

func (row toolDisclosureRow) hasInlineEditDiff() bool {
	name := compactToolName(row.toolName)
	return row.changeText != "" && (name == "edit" || name == "write")
}

type toolOutputTarget struct {
	recordID   int64
	key        string
	line, cols int
}

type toolOutputLink struct {
	recordID   int64
	key        string
	X, Y, Cols int
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
		record, row := m.toolOutputRow(link)
		if row == nil {
			return false
		}
		var state *viewState
		if target != nil {
			state = r.workspace().viewState(*target)
		}
		m.mutateAnchored(m.disclosureLayoutWidth(0), matchToolGroup([]int64{record.id}), func(bool) {
			row.outputExpanded = !row.outputExpanded
			m.refreshToolDisclosureWithAnchor(record, false)
			m.visual.invalidate()
		})
		if row.outputExpanded && !row.hasInlineEditDiff() && row.output != nil && !row.output.loaded && !row.outputLoading {
			r.loadInlineToolOutput(m, link, row, state)
		}
		m.refreshToolDisclosureWithAnchor(record, false)
		if state != nil {
			state.top, state.lastRows = m.scrollAnchor, -1
			rememberViewSections(m, state)
		}
		return true
	}
	return false
}

func readInlineToolOutput(ctx context.Context, store artifacts.Store, source *inlineToolOutput) *inlineToolOutput {
	next := *source
	next.loaded = true
	reader, err := store.Open(ctx, source.artifact.ID)
	if err == nil {
		var data []byte
		data, err = io.ReadAll(io.LimitReader(reader, inlineOutputBytes+1))
		closeErr := reader.Close()
		if err == nil {
			err = closeErr
		}
		if err == nil {
			next.text = formatInlineToolOutput(string(data), source.language)
		}
	}
	if err != nil {
		next.text = style.Styled("Full output unavailable; showing saved preview", "muted", "") + "\n" + source.text
	}
	return &next
}

func (r *managedREPL) loadInlineToolOutput(m *replModel, link toolOutputLink, row *toolDisclosureRow, state *viewState) {
	source, store := row.output, m.artifactStore
	if store == nil {
		next := *source
		next.loaded = true
		next.text = style.Styled("Full output unavailable; showing saved preview", "muted", "") + "\n" + source.text
		row.output = &next
		return
	}
	row.outputLoading = true
	if !r.background(func() {
		next := readInlineToolOutput(r.work.ctx, store, source)
		r.postUI(r.work.ctx, func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			if state != nil {
				i := &r.workspace().inspector
				if i.current != nil && i.current.model != nil && i.current.model != m && r.workspace().viewState(i.target) == state {
					current := i.current.model
					current.mu.Lock()
					installInlineToolOutput(current, link.key, source, next)
					current.mu.Unlock()
				}
				state.revision++
			}

			// A replay, reset, or newer durable result supersedes this read.
			record := m.toolDisclosures.get(link.recordID)
			if record == nil {
				return
			}
			for i := range record.rows {
				current := &record.rows[i]
				if current.sectionKey != link.key || current.output != source {
					continue
				}
				m.mutateAnchored(m.disclosureLayoutWidth(0), matchToolGroup([]int64{record.id}), func(bool) {
					current.output, current.outputLoading = next, false
					m.refreshToolDisclosureWithAnchor(record, false)
					m.visual.invalidate()
				})

				return
			}
		})
	}) {
		row.outputLoading = false
	}
}

// An already-open row may acquire an artifact when its durable result arrives.
// Start that read at paint time, with the same on-demand policy as a click.
func (r *managedREPL) ensureInlineToolOutputs(m *replModel, target *viewTarget) {
	var state *viewState
	if target != nil {
		state = r.workspace().viewState(*target)
	}
	changed := false
	for _, record := range m.toolDisclosures.all() {
		if !record.expanded {
			continue
		}
		for i := range record.rows {
			row := &record.rows[i]
			if !row.outputExpanded || row.hasInlineEditDiff() || row.output == nil || row.output.loaded || row.outputLoading {
				continue
			}
			r.loadInlineToolOutput(m, toolOutputLink{recordID: record.id, key: row.sectionKey}, row, state)
			m.refreshToolDisclosureWithAnchor(record, false)
			changed = true
		}
	}
	if changed {
		m.visual.invalidate()
		if state != nil {
			rememberViewSections(m, state)
		}
	}
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

func installInlineToolOutput(m *replModel, key string, source, next *inlineToolOutput) {
	for _, record := range m.toolDisclosures.all() {
		for i := range record.rows {
			row := &record.rows[i]
			if row.sectionKey == key && sameInlineToolOutput(row.output, source) {
				row.output, row.outputLoading = next, false
				m.refreshToolDisclosureWithAnchor(record, false)
				m.visual.invalidate()
				return
			}
		}
	}
}
