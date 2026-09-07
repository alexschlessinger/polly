package main

import (
	"fmt"
	"image"
	"strings"
	"time"

	rw "github.com/mattn/go-runewidth"
)

type inspectorHeaderLayout struct {
	text    string
	buttons []inspectorButton
	rows    int
}

// The header owns both its visible spans and their click targets. Its measured
// height is also the transcript's origin, including after wrapping or search.
type inspectorHeaderBuilder struct {
	width, col int
	origin     image.Point
	lines      []string
	buttons    []inspectorButton
}

func (b *inspectorHeaderBuilder) newline() {
	b.lines = append(b.lines, "")
	b.col = 0
}

func (b *inspectorHeaderBuilder) write(text, color, modifier, action string) {
	text = rw.Truncate(text, max(0, b.width-b.col), "…")
	cols := rw.StringWidth(text)
	if cols == 0 {
		return
	}
	row := len(b.lines) - 1
	b.lines[row] += styled(text, color, modifier)
	if action != "" {
		at := b.origin.Add(image.Pt(b.col, row))
		b.buttons = append(b.buttons, inspectorButton{image.Rect(at.X, at.Y, at.X+cols, at.Y+1), action})
	}
	b.col += cols
}

func (b *inspectorHeaderBuilder) item(text, color, modifier, action string) {
	if b.col > 0 {
		if b.col+1+rw.StringWidth(text) > b.width {
			b.newline()
		} else {
			b.write(" ", "", "", "")
		}
	}
	b.write(text, color, modifier, action)
}

func (b *inspectorHeaderBuilder) button(label, action string, enabled, selected bool) {
	color, modifier := "accent", ""
	if !enabled {
		color, action = "muted", ""
	} else if selected {
		color, modifier = "active", "bold"
	}
	b.item("["+label+"]", color, modifier, action)
}

func (b *inspectorHeaderBuilder) layout(height int) inspectorHeaderLayout {
	rows := min(max(0, height), len(b.lines))
	bounds := image.Rectangle{Min: b.origin, Max: b.origin.Add(image.Pt(b.width, rows))}
	buttons := make([]inspectorButton, 0, len(b.buttons))
	for _, button := range b.buttons {
		if !button.rect.Empty() && button.rect.In(bounds) {
			buttons = append(buttons, button)
		}
	}
	return inspectorHeaderLayout{strings.Join(b.lines[:rows], "\n"), buttons, rows}
}

// Called during paint with no model locks held. Availability comes from the
// owning runtime, never the display projection, and does not activate a runtime.
func (r *managedREPL) inspectorHeader(width, height, x, y int) inspectorHeaderLayout {
	w := r.workspace()
	i := &w.inspector
	b := inspectorHeaderBuilder{width: max(0, width), origin: image.Pt(x, y)}
	b.newline()
	root := r.visibleTab()
	isRoot := i.target.session.ID == root.viewID()
	name := i.target.session.Name
	if i.current != nil && i.current.info != nil && i.current.info.Metadata != nil {
		metadata := i.current.info.Metadata
		name = metadata.Name
		if title := strings.Join(strings.Fields(metadata.Description), " "); !isRoot && title != "" {
			name = title
		}
	}
	if name == "" {
		name = "Conversation"
	}
	// Keep the close button at the right edge even with long names.
	const chromeWidth = len("[x]")
	titleWidth := max(0, width-chromeWidth-1)
	itemName := ""
	if i.target.kind != conversationViewKind {
		itemName = "Thought"
		if i.target.kind == toolViewKind {
			itemName = "Tool"
		}
		if i.target.kind == toolViewKind && i.current != nil && i.current.model != nil {
			for _, tool := range i.current.model.inspections.tools {
				if tool.key == i.target.item {
					itemName = tool.call.Name
					break
				}
			}
		}
		if index, total, _, _ := inspectorSequencePosition(i); index > 0 {
			itemName += fmt.Sprintf(" · %d/%d", index, total)
		}
		itemName = rw.Truncate(itemName, max(1, titleWidth/2), "…")
		titleWidth = max(0, titleWidth-rw.StringWidth(itemName)-3)
	}
	if !isRoot && titleWidth >= 5 {
		rootWidth := min(rw.StringWidth(root.name), (titleWidth-3)/2)
		b.write(rw.Truncate(root.name, rootWidth, "…"), "accent", "", "root")
		b.write(" › ", "muted", "", "")
		titleWidth -= rootWidth + 3
	}
	parentAction := ""
	if i.target.kind != conversationViewKind {
		parentAction = "parent"
	}
	b.write(rw.Truncate(name, titleWidth, "…"), "accent", "bold", parentAction)
	if itemName != "" {
		b.write(" › ", "muted", "", "")
		b.write(itemName, "", "bold", "")
	}
	b.write(strings.Repeat(" ", max(0, width-chromeWidth-b.col-1)), "", "", "")
	b.button("x", "close", true, false)

	if i.searching {
		// Search replaces the contextual actions and creates no hidden hitboxes.
		b.newline()
		b.write("Find: ", "accent", "", "")
		b.write(rw.TruncatePrefix(i.searchInput.text(), max(0, width-b.col-1), "…")+"▏", "", "", "")
	} else if i.target.kind == toolViewKind {
		_, _, elapsed, launch := inspectorSequencePosition(i)
		if launch || elapsed != "" {
			b.newline()
			if launch {
				b.button("Open agent", "agent", true, false)
			}
			if elapsed != "" {
				b.item(elapsed, "muted", "", "")
			}
		}
	} else if i.target.kind == conversationViewKind && !isRoot {
		if tab := r.inspectionTab(i.target); tab != nil && tab != root {
			m := tab.model
			m.mu.Lock()
			busy, canceling, approval := m.busy, m.canceling, m.approval != nil
			m.mu.Unlock()
			if busy || approval {
				b.newline()
			}
			if busy {
				b.button("Stop agent", "stop", !canceling, false)
			}
			if approval {
				b.button("Review approval", "review", true, true)
			}
		}
	}

	b.newline()
	hint := ""
	if i.searching {
		hint = "─ Enter: find · Esc: cancel "
	}
	b.write(hint+strings.Repeat("─", max(0, width-rw.StringWidth(hint))), "muted", "", "")
	return b.layout(height)
}

func inspectorSequencePosition(i *inspectorState) (index, total int, elapsed string, launch bool) {
	if i.current == nil || i.current.model == nil {
		return
	}
	s := i.current.model.inspections
	if i.target.kind == thoughtViewKind {
		total = len(s.thoughts)
		for n, thought := range s.thoughts {
			if thought.key == i.target.item {
				index = n + 1
				break
			}
		}
	} else {
		total = len(s.tools)
		for n, tool := range s.tools {
			if tool.key == i.target.item {
				index, launch = n+1, tool.call.Name == "spawn_agent"
				if !tool.complete && !tool.started.IsZero() {
					elapsed = formatElapsed(time.Since(tool.started))
				}
				break
			}
		}
	}
	return
}
