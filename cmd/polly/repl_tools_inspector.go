package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/markdown"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/messages"
	ui "github.com/metaspartan/gotui/v5"
)

// Items are immutable projections. Result bodies are materialized only for
// expanded calls and reused across appends, clock ticks and resizing.
type toolInspectorItem struct {
	tool         inspectedTool
	arguments    string
	bash         *bashInspectorCommand
	expanded     bool
	preview      toolDisclosureRow
	argumentBody string
	setupBody    string
	output       []transcriptDisplayBlock
	outputMeta   string
}

type toolInspectorList struct {
	selected string
	items    []toolInspectorItem
	origin   string
	root     string
}

func toolInspectorBlock(key, section string) string { return "tool-list/" + section + "/" + key }

func (toolView) Project(ctx context.Context, source viewSource, state viewState) (*replModel, error) {
	m := newReplModel()
	m.inspectorWrap = true
	if source.info != nil {
		m.artifactStore = source.info.Artifacts
	}
	previous := make(map[string]toolInspectorItem)
	if source.previousTools != nil {
		for _, item := range source.previousTools.items {
			previous[item.tool.key] = item
		}
	}
	list := &toolInspectorList{origin: source.revision, selected: state.toolSelected}
	if source.model != nil {
		list.root = source.model.toolBaseDir
	}
	if strings.HasPrefix(source.revision, "live:") {
		list.origin = source.revision[:strings.LastIndex(source.revision, ":")]
	}
	var catalogue []inspectedTool
	if source.model != nil {
		catalogue = source.model.inspections.tools
	}
	for _, tool := range catalogue {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		old, cached := previous[tool.key]
		item := toolInspectorItem{tool: tool, arguments: tool.call.Arguments, expanded: state.toolItemExpanded(tool.key)}
		item.preview = toolDisclosureRow{label: toolLabel(tool.call)}
		item.preview.setCall(tool.call)
		// Retain display metadata, not a second copy of every result.
		item.tool.result = messages.ChatMessage{}
		item.tool.call.Arguments = ""
		if cached && old.tool.call.Name == tool.call.Name && old.arguments == item.arguments {
			item.bash, item.argumentBody, item.setupBody = old.bash, old.argumentBody, old.setupBody
		} else if command, ok := bashCommandOf(tool.call); ok {
			item.bash = newBashInspectorCommand(command)
		}
		if item.bash != nil && item.expanded && item.setupBody == "" {
			item.setupBody = strings.Join(markdown.RenderFence("setup", markdown.HighlightCodeLines(item.bash.setup, "bash"))[1:], "\n")
		}
		if item.bash == nil && item.expanded && item.argumentBody == "" {
			arguments := strings.TrimSpace(item.arguments)
			title, lang := "arguments", ""
			if json.Valid([]byte(arguments)) {
				title, lang = "arguments · json", "json"
			}
			lines := []string{style.Styled("(none)", "muted", "")}
			if arguments != "" {
				lines = markdown.HighlightCodeLines(strings.TrimRight(readableResult(arguments), "\n"), lang)
			}
			item.argumentBody = strings.Join(markdown.RenderFence(title, lines), "\n")
		}
		if item.expanded {
			if cached && old.expanded && old.tool.version == tool.version && list.origin != "" && source.previousTools.origin == list.origin {
				item.output, item.outputMeta = old.output, old.outputMeta
			} else {
				output := newReplModel()
				output.artifactStore = m.artifactStore
				var err error
				item.outputMeta, err = appendInspectedToolOutput(ctx, output, &tool)
				if err != nil {
					output.appendErrorLine(err.Error())
				}
				for n, entry := range output.transcript {
					item.output = append(item.output, transcriptDisplayBlock{key: toolInspectorBlock(tool.key, fmt.Sprintf("output-body:%d", n)), text: entry.text, images: entry.images})
				}
			}
		}
		list.items = append(list.items, item)
	}
	m.toolInspector = list
	return m, nil
}

func (v toolView) Rows(m *replModel, width int) [][]ui.Cell {
	if list := m.toolInspector; list != nil {
		for _, item := range list.items {
			if !item.tool.complete {
				tick := time.Now().UnixMilli() / 100
				if m.toolInspectorTick != tick {
					m.toolInspectorTick = tick
					m.visual.invalidate()
				}
				break
			}
		}
	}
	return m.transcriptRows(width)
}

func (list *toolInspectorList) blocks(width int) []transcriptDisplayBlock {
	var blocks []transcriptDisplayBlock
	for n, item := range list.items {
		key := item.tool.key
		add := func(section, text string) {
			blocks = append(blocks, transcriptDisplayBlock{key: toolInspectorBlock(key, section), text: text})
		}
		if n > 0 && list.items[n-1].expanded {
			add("gap", "")
		}
		marker := "  "
		if key == list.selected {
			marker = style.Styled("› ", "accent", "bold")
		}
		if width <= 2 {
			text := " "
			if key == list.selected {
				text = style.Styled("›", "accent", "bold")
			}
			add("title", text)
		} else {
			add("title", marker+item.previewAt(width-2, list.root))
		}
		if !item.expanded {
			continue
		}
		if item.bash != nil {
			if item.bash.setup != "" {
				add("setup", style.Styled("setup", "muted", ""))
				add("setup-body", item.setupBody)
			}
			add("command-body", item.bash.commandAtWidth(width))
		} else {
			add("arguments-body", item.argumentBody)
		}
		blocks = append(blocks, item.output...)
		if item.tool.call.Name == "spawn_agent" {
			add("agent", style.Styled("Open agent", "accent", ""))
		}
	}
	if len(blocks) == 0 {
		blocks = append(blocks, transcriptDisplayBlock{key: "tools-empty", text: style.Styled("No tools to inspect", "muted", "")})
	}
	return blocks
}

// Use the transcript's width-aware, literal-safe summaries without loading
// result bodies. Inspection records supply status for both live and saved calls.
func (item toolInspectorItem) previewAt(width int, root string) string {
	glyph := "▸"
	if item.expanded {
		glyph = "▾"
	}
	prefix := style.Styled(glyph, "accent", "bold")
	if width <= 2 {
		return prefix
	}
	tool := item.tool
	line := tool.pres.inline()
	switch {
	case !tool.complete && !tool.started.IsZero():
		line = runningInlineTool(time.Since(tool.started))
	case tool.complete && !tool.available && tool.pres.outcome == toolOutcomeUnknown:
		line.meta = "output unavailable"
	}
	row := item.preview
	row.setLine(line)
	body := row.inlineLineAt(width, root)
	if strings.HasPrefix(body, "  ") {
		return prefix + " " + strings.TrimPrefix(body, "  ")
	}
	return prefix + " " + strings.TrimPrefix(row.inlineLineAt(width-2, root), "  ")
}

// toggleToolInspectorItems is Ctrl-O in the tools list: every entry opens, or
// every entry closes when nothing is left closed. Its expansion state lives in
// the view, not in transcript records, so the list is projected again from it.
// The decision is sticky like the conversation's: while it opens, calls that
// appear in the list later open too, until the press that closes everything.
// The caller has checked that the inspected view is the tools list. Caller
// must hold r.model.mu.
func (r *managedREPL) toggleToolInspectorItems() {
	i := &r.workspace().inspector
	if i.current == nil || i.current.model == nil {
		return
	}
	tools := i.current.model.inspections.tools
	if len(tools) == 0 {
		return
	}
	s := r.workspace().viewState(i.target)
	if s.toolExpanded == nil {
		s.toolExpanded = make(map[string]bool)
	}
	expand := false
	for _, tool := range tools {
		if !s.toolItemExpanded(tool.key) {
			expand = true
			break
		}
	}
	for _, tool := range tools {
		s.toolExpanded[tool.key] = expand
	}
	s.expandAll = expand
	relayoutToolList(i.current.model, s)
}

// relayoutToolList projects the tools list again after its expansion state
// changed. The new layout replaces the rows below the top one, so the view
// keeps where it was and re-anchors on the next paint, and the new-output
// baseline is re-seeded because the rows moved without any new output.
func relayoutToolList(m *replModel, s *viewState) {
	s.follow = false
	rememberViewPosition(m, s)
	s.lastRows, s.revision = -1, s.revision+1
}

func (r *managedREPL) toolInspectorAction(action string) bool {
	rest, ok := strings.CutPrefix(action, "tool-list/")
	if !ok {
		return false
	}
	section, key, ok := strings.Cut(rest, "/")
	i := &r.workspace().inspector
	if !ok || i.target.kind != toolViewKind || i.current == nil || i.current.model == nil {
		return true
	}
	for _, tool := range i.current.model.inspections.tools {
		if tool.key != key {
			continue
		}
		if section == "agent" {
			if tool.call.Name == "spawn_agent" {
				r.inspectLaunchedAgent(i.target, tool.call.ID)
			}
			return true
		}
		if section != "title" {
			return true
		}
		s := r.workspace().viewState(i.target)
		s.toolSelected = key
		if s.toolExpanded == nil {
			s.toolExpanded = make(map[string]bool)
		}
		s.toolExpanded[key] = !s.toolItemExpanded(key)
		relayoutToolList(i.current.model, s)
		return true
	}
	return true
}

// navigateToolsInspector keeps selection by call identity across refreshes.
func (r *managedREPL) navigateToolsInspector(key string) bool {
	i := &r.workspace().inspector
	switch key {
	case "<Up>", "<Down>", "<Enter>", "<Left>", "<Right>":
	default:
		return false
	}
	if i.current == nil || i.current.model == nil {
		return true
	}
	tools := i.current.model.inspections.tools
	if len(tools) == 0 {
		return true
	}
	s := r.workspace().viewState(i.target)
	index := len(tools) - 1
	for n, tool := range tools {
		if tool.key == s.toolSelected {
			index = n
			break
		}
	}
	switch key {
	case "<Up>":
		index = max(0, index-1)
	case "<Down>":
		index = min(len(tools)-1, index+1)
	}
	s.toolSelected = tools[index].key
	if key == "<Enter>" || key == "<Left>" || key == "<Right>" {
		if s.toolExpanded == nil {
			s.toolExpanded = make(map[string]bool)
		}
		expanded := !s.toolItemExpanded(s.toolSelected)
		if key == "<Left>" {
			expanded = false
		}
		if key == "<Right>" {
			expanded = true
		}
		s.toolExpanded[s.toolSelected] = expanded
	}
	relayoutToolList(i.current.model, s)
	s.toolJump = s.toolSelected
	return true
}
