package main

import (
	"fmt"
	"image"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/sessions"
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
	b.lines[row] += style.Styled(text, color, modifier)
	if action != "" {
		at := b.origin.Add(image.Pt(b.col, row))
		rect := image.Rect(at.X, at.Y, at.X+cols, at.Y+1)
		// Adjacent spans with one action are one control, so a title and its
		// arrow share a hitbox.
		if n := len(b.buttons); n > 0 && b.buttons[n-1].action == action && b.buttons[n-1].rect.Min.Y == at.Y && b.buttons[n-1].rect.Max.X == at.X {
			b.buttons[n-1].rect.Max.X = rect.Max.X
		} else {
			b.buttons = append(b.buttons, inspectorButton{rect, action})
		}
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

// link adds a clickable word: accent when enabled, muted and inert when not,
// and the attention color when it is the action the row exists for.
func (b *inspectorHeaderBuilder) link(label, action string, enabled, selected bool) {
	color, modifier := "accent", ""
	if !enabled {
		color, action = "muted", ""
	} else if selected {
		color, modifier = "active", "bold"
	}
	b.item(label, color, modifier, action)
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
	isRoot := r.targetsVisibleTab(i.target)
	name, detail := i.target.session.Name, ""
	if i.current != nil && i.current.info != nil && i.current.info.Metadata != nil {
		metadata := i.current.info.Metadata
		name = sessions.DisplayLabel(metadata)
		if !isRoot {
			if name != metadata.Name {
				detail = metadata.Name
			}
			if brief := strings.Join(strings.Fields(metadata.Description), " "); brief != "" && brief != name {
				if detail != "" {
					detail += " · "
				}
				detail += brief
			}
		}
	}
	if name == "" {
		name = "Conversation"
	}
	// The arrow and the title are one control: either returns to the caller.
	b.write("‹ ", "accent", "", "parent")
	titleWidth := max(0, width-b.col)
	itemName, status, launch := "", "", false
	if i.target.kind != conversationViewKind {
		itemName = "Thought"
		if i.target.kind == swarmViewKind {
			itemName = "Swarm"
			if i.target.item != "" {
				itemName += " · " + i.target.item
			}
		}
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
		var index, total int
		index, total, status, launch = inspectorSequencePosition(i)
		if index > 0 {
			itemName += fmt.Sprintf(" · %d/%d", index, total)
		}
	}
	if itemName != "" {
		b.write(rw.Truncate(itemName, titleWidth, "…"), "accent", "bold", "parent")
	} else {
		b.write(rw.Truncate(name, titleWidth, "…"), "accent", "bold", "parent")
		if room := titleWidth - rw.StringWidth(name) - 3; detail != "" && room >= 8 {
			b.write(" · "+rw.Truncate(detail, room, "…"), "muted", "", "parent")
		}
	}

	// The second row carries the item's state and its actions; it exists
	// only when there is something to say.
	sep := func() {
		if b.col > 0 {
			b.write(" ·", "muted", "", "")
		}
	}
	if i.searching {
		// Search replaces the contextual row and creates no hidden hitboxes.
		b.newline()
		b.write("Find: ", "accent", "", "")
		hint := "  Enter find · Esc cancel"
		room := width - b.col - 1
		if room-rw.StringWidth(hint) >= 8 {
			room -= rw.StringWidth(hint)
		} else {
			hint = ""
		}
		b.write(rw.TruncatePrefix(i.searchInput.text(), max(0, room), "…")+"▏", "", "", "")
		b.write(hint, "muted", "", "")
	} else if i.target.kind == swarmViewKind {
		b.newline()
		b.link("Agents", "swarm_agents", true, false)
		for _, name := range []string{"members", "tasks", "messages", "publications", "workflows", "integrations", "previews", "raw"} {
			sep()
			b.link(name, "swarm_"+name, true, i.target.item == name)
		}
	} else if i.target.kind == toolViewKind {
		if status != "" || launch {
			b.newline()
			if status != "" {
				b.item(status, "muted", "", "")
			}
			if launch {
				sep()
				b.link("Open agent", "agent", true, false)
			}
		}
	} else if i.target.kind == conversationViewKind && !isRoot {
		if runtime := r.inspectedSwarm(i.target); runtime != nil {
			r.model.mu.Lock()
			p, approval, _ := r.swarmListing(i.target.session.ID, runtime.ID)
			r.model.mu.Unlock()
			b.newline()
			if status := listingLabel(p, approval); status != "" {
				b.item(status, "muted", "", "")
			}
			if p.Busy {
				sep()
				b.link("Stop agent", "stop", true, false)
			}
			sep()
			b.link("Send request", "message", true, false)
			if approval {
				sep()
				b.link("Review approval", "review", true, true)
			}
		} else {
			busy, canceling, approval := false, false, false
			outcome, elapsed := turnOutcomeNone, time.Duration(0)
			if tab := r.inspectionTab(i.target); tab != nil && tab != root {
				m := tab.model
				m.mu.Lock()
				busy, canceling, approval = m.busy, m.canceling, m.approval != nil
				outcome, elapsed = m.lastOutcome, m.lastElapsed
				m.mu.Unlock()
			} else if i.current != nil && i.current.model != nil {
				outcome, elapsed = i.current.model.lastOutcome, i.current.model.lastElapsed
			}
			status := ""
			switch {
			case r.heldElsewhere(i.target):
				status = "open in another polly · read-only"
			case approval:
				status = "approval needed"
			case busy:
				status = "running"
			case outcome != turnOutcomeNone:
				status = turnOutcomeLabel(outcome)
				if elapsed > 0 && outcome != turnOutcomeCanceled {
					status += " · " + formatElapsed(elapsed)
				}
			case i.current != nil && i.current.info != nil && i.current.info.Metadata != nil && i.current.info.Metadata.SpawnOutcome != "":
				status = spawnOutcomeStatus(i.current.info.Metadata.SpawnOutcome)
			}
			if status != "" {
				b.newline()
				b.item(status, "muted", "", "")
			}
			if busy {
				sep()
				b.link("Stop agent", "stop", !canceling, false)
			}
			if approval {
				sep()
				b.link("Review approval", "review", true, true)
			}
		}
	}
	return b.layout(height)
}

// inspectorSequencePosition locates the inspected item in its catalogue: its
// position, the tool's state with its clock, and whether it launched an agent.
func inspectorSequencePosition(i *inspectorState) (index, total int, status string, launch bool) {
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
				status = inspectedToolStatus(tool)
				break
			}
		}
	}
	return
}

// inspectedToolStatus is the tool's state and clock: a live clock while it
// runs, its duration once it settled.
func inspectedToolStatus(t inspectedTool) string {
	status := t.status
	switch {
	case !t.complete && !t.started.IsZero():
		status += " · " + formatElapsed(time.Since(t.started))
	case t.complete && t.duration > 0:
		status += " · " + formatElapsed(t.duration)
	}
	return status
}
