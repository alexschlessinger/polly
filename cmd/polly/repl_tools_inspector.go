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

type toolInspectorSections struct {
	setup, command, output bool
}

// Items are immutable projections. Result bodies are materialized only for
// open output sections and reused across appends, clock ticks and resizing.
type toolInspectorItem struct {
	tool         inspectedTool
	arguments    string
	bash         *bashInspectorCommand
	sections     toolInspectorSections
	argumentBody string
	setupBody    string
	output       []transcriptDisplayBlock
	outputMeta   string
}

type toolInspectorList struct {
	items  []toolInspectorItem
	origin string
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
	list := &toolInspectorList{origin: source.revision}
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
		item := toolInspectorItem{tool: tool, arguments: tool.call.Arguments, sections: state.toolSections[tool.key]}
		// Retain display metadata, not a second copy of every result.
		item.tool.result = messages.ChatMessage{}
		item.tool.call.Arguments = ""
		if cached && old.tool.call.Name == tool.call.Name && old.arguments == item.arguments {
			item.bash, item.argumentBody, item.setupBody = old.bash, old.argumentBody, old.setupBody
		} else if command, ok := bashCommandOf(tool.call); ok {
			item.bash = newBashInspectorCommand(command)
		}
		if item.bash != nil && item.sections.setup && item.setupBody == "" {
			item.setupBody = strings.Join(markdown.RenderFence("setup", markdown.HighlightCodeLines(item.bash.setup, "bash"))[1:], "\n")
		}
		if item.bash == nil && item.sections.command && item.argumentBody == "" {
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
		if item.sections.output {
			if cached && old.sections.output && old.tool.version == tool.version && list.origin != "" && source.previousTools.origin == list.origin {
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
		if n > 0 {
			add("gap", "")
		}
		header := inspectorHeaderBuilder{width: width, lines: []string{""}}
		header.toolTitle(item.tool.call.Name, "", inspectedToolStatus(item.tool))
		add("title", header.lines[0])
		section := func(name string, expanded bool, meta string) {
			glyph := "▸"
			if expanded {
				glyph = "▾"
			}
			label := name
			if expanded && meta != "" {
				label += " · " + meta
			}
			add(name, style.Styled(glyph, "accent", "bold")+" "+style.Styled(label, "muted", ""))
		}
		if item.bash != nil {
			if item.bash.setup != "" {
				add("setup", item.bash.setupLabel(width, item.sections.setup))
				if item.sections.setup {
					add("setup-body", item.setupBody)
				}
			}
			section("command", item.sections.command, "")
			if item.sections.command {
				_, body, _ := strings.Cut(item.bash.commandAtWidth(width), "\n")
				add("command-body", body)
			}
		} else {
			section("arguments", item.sections.command, "")
			if item.sections.command {
				_, body, _ := strings.Cut(item.argumentBody, "\n")
				add("arguments-body", body)
			}
		}
		section("output", item.sections.output, item.outputMeta)
		if item.sections.output {
			blocks = append(blocks, item.output...)
		}
		if item.tool.call.Name == "spawn_agent" {
			add("agent", style.Styled("Open agent", "accent", ""))
		}
	}
	if len(blocks) == 0 {
		blocks = append(blocks, transcriptDisplayBlock{key: "tools-empty", text: style.Styled("No tools to inspect", "muted", "")})
	}
	return blocks
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
		s := r.workspace().viewState(i.target)
		if s.toolSections == nil {
			s.toolSections = make(map[string]toolInspectorSections)
		}
		expanded := s.toolSections[key]
		switch section {
		case "setup":
			expanded.setup = !expanded.setup
		case "command", "arguments":
			expanded.command = !expanded.command
		case "output":
			expanded.output = !expanded.output
		default:
			return true
		}
		s.toolSections[key] = expanded
		s.follow = false
		rememberViewPosition(i.current.model, s)
		s.lastRows = -1
		s.revision++
		return true
	}
	return true
}
