package main

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/swarm"
	ui "github.com/metaspartan/gotui/v5"
)

const (
	agentOverviewAssignmentRows = 3
	agentOverviewUpdateRows     = 6
	agentOverviewOutputRows     = 6
	agentOverviewOutputBytes    = 8 << 10
	agentOverviewRecentTools    = 3
)

// agentOverviewState belongs to navigation, not the execution model. A missing
// tool choice follows its activity (open while running, closed when settled);
// an explicit choice, including false, always wins across display refreshes.
// completed is supplied only by a source that knows the execution completed.
type agentOverviewState struct {
	assignmentExpanded bool
	updateExpanded     bool
	tools              map[string]bool
	expandAll          bool
	completed          bool
	assignment         string // current execution brief, when available
	currentTranscript  bool   // verified execution identity, independent of augmented prompt text
	liveExecution      string // execution/generation identity for transient call keys
	liveTools          []swarm.LiveToolActivity
	toolStarted        map[string]time.Time // exact host call identity; metadata only
}

// actions is parallel to rows, with empty strings for non-interactive rows.
// Geometry is deliberately separate from m.visual: Watch owns the transcript's
// cache, native media placements, output paging and scroll anchors.
type agentOverviewRender struct {
	rows    [][]ui.Cell
	actions []string
}

// renderAgentOverview projects only recorded display content. It performs no
// history/artifact reads and does not change disclosure choices. m must be an
// isolated display model (or locked by its owner), as Markdown may materialize.
func renderAgentOverview(m *replModel, width int, state agentOverviewState) agentOverviewRender {
	return renderAgentOverviewCached(m, width, state, nil)
}

// renderAgentOverviewCached reuses the assignment and update excerpts across
// paints. The caller owns cache with its display state, separately from Watch.
func renderAgentOverviewCached(m *replModel, width int, state agentOverviewState, cache *agentOverviewCache) agentOverviewRender {
	b := agentOverviewBuilder{width: max(1, width)}
	var assignmentCache, updateCache *agentOverviewExcerptCache
	if cache != nil {
		assignmentCache, updateCache = &cache.assignment, &cache.update
	}
	if m == nil {
		b.text(style.Styled("Agent display unavailable", "muted", ""), "")
		return b.agentOverviewRender
	}
	m.renderMarkdownAtWidth(b.width)
	m.renderPendingMarkdown()

	assignment, media := agentOverviewAssignment(m)
	if state.assignment != "" {
		assignment, media = style.Escape(style.StripImageMarkers(state.assignment)), false
	}
	if assignment == "" {
		if assignmentCache != nil {
			*assignmentCache = agentOverviewExcerptCache{}
		}
		b.heading("Task", "", false)
		b.unavailable("Assignment unavailable in this display")
	} else {
		b.heading("Task", "overview:assignment", state.assignmentExpanded)
		limit := agentOverviewAssignmentRows
		if state.assignmentExpanded {
			limit = 0
		}
		if b.excerptCached(assignment, nil, limit, assignmentCache) {
			b.text(style.Styled("Show full assignment", "muted", ""), "overview:assignment")
		}
		if media {
			b.media()
		}
	}

	latest := -1
	// A refreshed execution brief may arrive before its durable transcript.
	// Do not pair the new assignment with the preceding task's answer.
	if state.currentTranscript || state.assignment == "" || state.assignment == m.assignment {
		latest = agentOverviewLatestAssistant(m)
	}
	if latest >= 0 {
		b.blank()
		label := "Latest update"
		if state.completed && agentOverviewFinalAssistant(m, latest) {
			label = "Result"
		}
		b.heading(label, "", false)
		entry := m.transcript[latest]
		limit := agentOverviewUpdateRows
		if state.updateExpanded {
			limit = 0
		}
		if b.excerptCached(entry.text, entry.images, limit, updateCache) {
			b.text(style.Styled("▸ Show full update", "accent", ""), "overview:update")
		} else if state.updateExpanded {
			b.text(style.Styled("▾ Collapse update", "accent", ""), "overview:update")
		}
		if len(entry.images) > 0 {
			b.media()
		}
	} else if updateCache != nil {
		*updateCache = agentOverviewExcerptCache{}
	}

	tools := agentOverviewTools(m)
	tools = append(tools, agentOverviewLiveTools(state)...)
	active := 0
	for _, tool := range tools {
		if tool.active {
			if active == 0 {
				b.blank()
				b.heading("Current operation", "", false)
			}
			b.tool(tool, m.toolBaseDir, state)
			active++
		}
	}

	// The catalog records call order, not a completion timestamp. Retain the
	// last three settled calls in that order rather than inventing chronology.
	remaining := len(tools) - active
	if remaining > 0 {
		b.blank()
		b.heading("Recent work", "", false)
	}
	for _, tool := range tools {
		if tool.active {
			continue
		}
		remaining--
		if remaining < agentOverviewRecentTools || tool.expanded(state) {
			b.tool(tool, m.toolBaseDir, state)
		}
	}
	if len(tools) > 0 {
		b.text(style.Styled("All tools → Watch", "accent", ""), "watch")
	}
	return b.agentOverviewRender
}

func agentOverviewAssignment(m *replModel) (text string, media bool) {
	text = style.Escape(style.StripImageMarkers(strings.TrimSpace(m.assignment)))
	for _, entry := range m.transcript {
		if !entry.initialPrompt {
			continue
		}
		media = (len(entry.images) > 0 || len(entry.contextFiles) > 0) && (m.assignment == "" || strings.HasPrefix(entry.text, formattedUserPrompt(m.assignment)))
		if text == "" {
			// Compatibility for a display snapshot built before assignment
			// retention. Remove only the renderer-owned gutter, not source text.
			lines := strings.Split(entry.text, "\n")
			for i := range lines {
				lines[i] = strings.TrimPrefix(lines[i], userGutter())
			}
			text = strings.Join(lines, "\n")
		}
		break
	}
	return text, media
}

func agentOverviewLatestAssistant(m *replModel) int {
	for i := len(m.transcript) - 1; i >= 0; i-- {
		entry := m.transcript[i]
		if entry.assistant && strings.TrimSpace(entry.text) != "" {
			return i
		}
		// An unanswered follow-up belongs to a new turn, even while the
		// preceding task's saved response remains visible in Watch.
		if strings.HasPrefix(entry.text, userGutter()) {
			return -1
		}
	}
	return -1
}

// A completed execution does not turn pre-tool commentary into a final answer.
// Nor may a later unanswered prompt inherit the preceding turn's result label.
func agentOverviewFinalAssistant(m *replModel, index int) bool {
	if m.quiet && len(m.displayCatalog.tools) > 0 {
		// Quiet live displays retain calls in the catalog but not their
		// transcript positions. Without that ordering, keep the neutral label.
		return false
	}
	for _, record := range m.toolDisclosures.all() {
		if record.transcriptIndex > index {
			return false
		}
	}
	for _, entry := range m.transcript[index+1:] {
		if strings.HasPrefix(entry.text, userGutter()) {
			return false
		}
	}
	for _, tool := range m.displayCatalog.tools {
		if !tool.complete {
			return false
		}
	}
	return true
}

type agentOverviewTool struct {
	key    string
	row    *toolDisclosureRow
	source *displayTool
	active bool
}

// displayRow materializes only tools selected for painting. Catalogs may retain
// thousands of old calls whose arguments and presentations are never visible.
func (t agentOverviewTool) displayRow(state agentOverviewState) toolDisclosureRow {
	var row toolDisclosureRow
	if t.row != nil {
		row = *t.row
	}
	if source := t.source; source != nil {
		if t.row == nil {
			row = toolDisclosureRow{sectionKey: source.key, callID: source.call.ID, label: toolLabel(source.call)}
			row.setCall(source.call)
		}
		row.settled, row.pres = source.complete, source.pres
		row.setLine(source.pres.inline())
		if !source.complete {
			started := source.started
			if observed := state.toolStarted[source.call.DisplayID]; !observed.IsZero() {
				// Hydration time is not execution start time. A resize or a new
				// live revision must not restart the current operation's clock.
				started = observed
			}
			elapsed := time.Duration(0)
			if !started.IsZero() {
				elapsed = max(0, time.Since(started))
			}
			row.setLine(runningInlineTool(elapsed))
		}
	}
	return row
}

func (t agentOverviewTool) expanded(state agentOverviewState) bool {
	if expanded, chosen := state.tools[t.key]; chosen {
		return expanded
	}
	return state.expandAll || t.active
}

// agentOverviewToolExpanded lets the click handler invert the same default
// used by rendering, without persisting automatic choices as manual ones.
func agentOverviewToolExpanded(m *replModel, key string, state agentOverviewState) bool {
	if expanded, chosen := state.tools[key]; chosen {
		return expanded
	}
	for _, tool := range agentOverviewLiveTools(state) {
		if tool.key == key {
			return true
		}
	}
	if m != nil {
		for _, tool := range agentOverviewTools(m) {
			if tool.key == key {
				return state.expandAll || tool.active
			}
		}
	}
	return false
}

func agentOverviewLiveTools(state agentOverviewState) []agentOverviewTool {
	var tools []agentOverviewTool
	for _, tool := range state.liveTools {
		row := toolDisclosureRow{callID: tool.Call.ID, label: toolLabel(tool.Call)}
		row.setCall(tool.Call)
		row.setLine(runningInlineTool(max(0, time.Since(tool.Started))))
		key := fmt.Sprintf("live:%s:%s:%s:%d", state.liveExecution, tool.Call.ID, tool.Call.Name, tool.Started.UnixNano())
		if tool.Call.DisplayID != "" {
			key = displayToolKey(tool.Call, 0)
		}
		row.sectionKey = key
		tools = append(tools, agentOverviewTool{key: key, row: &row, active: true})
	}
	return tools
}

// The live transcript owns the status once it contains this exact host call
// identity. Metadata can lag a completed result; never append a second running
// row beside it. Legacy/missing identities stay separate, not heuristically
// matched by provider call ID, name, arguments, or content.
func agentOverviewUnrecordedLiveTools(m *replModel, live []swarm.LiveToolActivity) []swarm.LiveToolActivity {
	if len(live) == 0 {
		return nil
	}
	known := make(map[string]bool)
	for _, tool := range m.displayCatalog.tools {
		if tool.call.DisplayID != "" {
			known[tool.call.DisplayID] = true
		}
	}
	remaining := make([]swarm.LiveToolActivity, 0, len(live))
	for _, tool := range live {
		if !known[tool.Call.DisplayID] {
			remaining = append(remaining, tool)
		}
	}
	return remaining
}

func agentOverviewTools(m *replModel) []agentOverviewTool {
	var records []*toolDisclosureRecord
	for _, record := range m.toolDisclosures.all() {
		records = append(records, record)
	}
	slices.SortFunc(records, func(a, b *toolDisclosureRecord) int {
		return a.transcriptIndex - b.transcriptIndex
	})
	rows := make(map[string]*toolDisclosureRow)
	var fallback []agentOverviewTool
	for _, record := range records {
		for i := range record.rows {
			row := &record.rows[i]
			if row.isProjectedAgent() {
				continue
			}
			key := row.sectionKey
			if key == "" {
				key = fmt.Sprintf("tool-row:%d:%d", record.id, i)
			}
			rows[key] = row
			fallback = append(fallback, agentOverviewTool{key: key, row: row, active: !row.settled && !record.complete})
		}
	}
	tools := make([]agentOverviewTool, 0, len(m.displayCatalog.tools))
	seen := make(map[string]bool)
	for i := range m.displayCatalog.tools {
		source := &m.displayCatalog.tools[i]
		tools = append(tools, agentOverviewTool{key: source.key, row: rows[source.key], source: source, active: !source.complete})
		if len(fallback) > 0 {
			seen[source.key] = true
		}
	}
	for _, tool := range fallback {
		if !seen[tool.key] {
			tools = append(tools, tool)
		}
	}
	return tools
}

type agentOverviewBuilder struct {
	agentOverviewRender
	width int
}

func (b *agentOverviewBuilder) append(rows [][]ui.Cell, action string) {
	b.rows = append(b.rows, rows...)
	for range rows {
		b.actions = append(b.actions, action)
	}
}

func (b *agentOverviewBuilder) text(text, action string) {
	b.append(style.VisualRows(style.StripImageMarkers(text), ui.StyleClear, b.width), action)
}

func (b *agentOverviewBuilder) blank() { b.append([][]ui.Cell{nil}, "") }

func (b *agentOverviewBuilder) heading(label, action string, expanded bool) {
	if action != "" {
		glyph := "▸"
		if expanded {
			glyph = "▾"
		}
		label = glyph + " " + label
	}
	b.text(style.Styled(label, "accent", "bold"), action)
}

func (b *agentOverviewBuilder) unavailable(text string) {
	b.text(style.Styled(text, "muted", ""), "")
}

// excerpt keeps the original rendered prose, with no heuristic summarization.
// Native media stays in Watch; its existing caption/path fallback remains here.
func (b *agentOverviewBuilder) excerpt(text string, images []style.Image, limit int) bool {
	return b.excerptCached(text, images, limit, nil)
}

func (b *agentOverviewBuilder) excerptCached(text string, images []style.Image, limit int, cache *agentOverviewExcerptCache) bool {
	rows, truncated := cache.render(text, images, b.width, limit)
	b.append(rows, "")
	return truncated
}

func (b *agentOverviewBuilder) media() {
	b.text(style.Styled("View media in Watch", "accent", ""), "watch")
}

func (b *agentOverviewBuilder) tool(tool agentOverviewTool, root string, state agentOverviewState) {
	expanded := tool.expanded(state)
	glyph := "▸"
	if expanded {
		glyph = "▾"
	}
	row := tool.displayRow(state)
	b.text(style.Styled(glyph, "accent", "")+" "+strings.TrimLeft(row.inlineLineAt(max(1, b.width-2), root), " "), "overview:tool:"+tool.key)
	if !expanded {
		return
	}
	if row.output == nil {
		if tool.active {
			// Tool callbacks currently publish output only on settlement.
			b.unavailable("Output pending")
		} else {
			b.unavailable("Output unavailable in this display")
		}
	} else {
		body, clipped := agentOverviewOutputTail(row.output.body)
		if strings.TrimSpace(body) == "" {
			b.unavailable("No text output")
		} else {
			rows := style.VisualRows(style.Escape(body), ui.StyleClear, b.width)
			if len(rows) > agentOverviewOutputRows {
				rows = rows[len(rows)-agentOverviewOutputRows:]
				clipped = true
			}
			if clipped {
				b.unavailable("… output tail")
			}
			b.append(rows, "")
		}
		// The existing transcript owns artifact loading, pagination and diffs.
		b.text(style.Styled("Full output in Watch", "accent", ""), "watch")
	}
	if len(row.inspectionImages) > 0 {
		b.excerpt(style.RenderImages(row.inspectionImages, ""), row.inspectionImages, 0)
		b.media()
	}
}

// Bound before parsing/wrapping: a single enormous output line must not make
// painting the overview allocate in proportion to the full stored result.
func agentOverviewOutputTail(body string) (string, bool) {
	body = strings.TrimRight(body, "\r\n")
	clipped := len(body) > agentOverviewOutputBytes
	if clipped {
		start := len(body) - agentOverviewOutputBytes
		for start < len(body) && !utf8.RuneStart(body[start]) {
			start++
		}
		body = body[start:]
	}
	return cleanInlineToolOutput(body), clipped
}
