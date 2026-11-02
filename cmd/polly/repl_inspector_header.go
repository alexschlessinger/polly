package main

import (
	"image"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/swarm"
	rw "github.com/mattn/go-runewidth"
	ui "github.com/metaspartan/gotui/v5"
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
	if b.col >= b.width {
		return
	}
	if len(b.lines) == 0 {
		b.newline()
	}
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

type inspectorHeaderAction struct {
	label, action     string
	enabled, selected bool
}

// title reserves room for the actions before truncating the name. Only when
// the controls themselves cannot fit do they wrap; separators never stand alone.
func (b *inspectorHeaderBuilder) title(name string, actions ...inspectorHeaderAction) {
	room := b.width
	for _, action := range actions {
		room -= 3 + rw.StringWidth(action.label)
	}
	b.newline()
	b.write(rw.Truncate(name, max(1, room), "…"), "accent", "bold", "parent")
	for _, action := range actions {
		if b.col > 0 {
			if b.col+3+rw.StringWidth(action.label) > b.width {
				b.newline()
			} else {
				b.write(" ·", "muted", "", "")
			}
		}
		b.link(action.label, action.action, action.enabled, action.selected)
	}
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
	// Rows are added as content needs them: a view with no title and no
	// contextual row has no header, and its body starts under the frame.
	b := inspectorHeaderBuilder{width: max(0, width), origin: image.Pt(x, y)}
	root := r.visibleTab()
	isRoot := r.targetsVisibleTab(i.target)
	// Only a conversation has a title: the agent's name, which returns to the
	// caller. Every view closes from the button on the frame's top border.
	name := i.target.session.Name
	if i.target.kind == conversationViewKind {
		// One name: the readable one when the session has it, else its
		// generated name.
		if i.current != nil && i.current.info != nil && i.current.info.Metadata != nil {
			name = sessions.DisplayLabel(i.current.info.Metadata)
		}
		if name == "" {
			name = "Conversation"
		}
		if i.searching || isRoot {
			b.title(name)
		}
	}

	// Additional rows carry contextual actions or search. Individual tool
	// states and launch actions belong to the scrolling list.
	sep := func() {
		if b.col > 0 {
			b.write(" ·", "muted", "", "")
		}
	}
	agentTitle := func(actions ...inspectorHeaderAction) {
		navigation := []inspectorHeaderAction{{"Watch ↗", "watch", true, false}}
		if w.viewState(i.target).watch {
			navigation = []inspectorHeaderAction{
				{"Back to inspection", "inspection", true, false},
				{"Message…", "message", !r.heldElsewhere(i.target), false},
			}
		}
		b.title(name, append(navigation, actions...)...)
	}
	agentNotices := func() {
		if v := i.current; v != nil && v.model != nil && !v.unavailable && v.failures == 0 {
			if !v.live.active {
				b.newline()
				b.item("Saved transcript · updates at checkpoints", "muted", "", "")
			}
			if v.live.truncated {
				b.newline()
				b.item("Live buffer limit reached · some live output omitted", "warn", "", "")
			}
			if v.live.incomplete {
				b.newline()
				b.item("Live output incomplete · showing saved checkpoint", "warn", "", "")
			}
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
		if p, ok := r.parentPresentation(root); ok {
			b.item(p.Display, "muted", "", "")
			sep()
		}
		b.link("Agents", "swarm_agents", true, false)
		for _, name := range swarmInspectorSections {
			sep()
			b.link(name, "swarm_"+name, true, i.target.item == name)
		}
	} else if i.target.kind == conversationViewKind && !isRoot {
		owner := r.rootTab(root)
		if snapshot := owner.swarmSnapshot; snapshot != nil && snapshot.Members[i.target.session.ID] != nil {
			member := snapshot.Members[i.target.session.ID]
			owner.model.mu.Lock()
			approval := owner.model.memberNeedsApproval(member.ID)
			owner.model.mu.Unlock()
			canControl := r.inspectedSwarm(i.target) != nil
			busy := agentExecutionBusy(snapshot, member)
			var actions []inspectorHeaderAction
			if canControl && busy && member.Control != swarm.MemberControlStopped {
				actions = append(actions, inspectorHeaderAction{"Stop", "stop", true, false})
			}
			if canControl && !busy && member.Control == swarm.MemberControlStopped {
				actions = append(actions, inspectorHeaderAction{"Resume agent", "resume", true, false})
			}
			if canControl && approval {
				actions = append(actions, inspectorHeaderAction{"Review approval", "review", true, true})
			}
			agentTitle(actions...)
			status, warn, details := agentInspectionStatus(snapshot, member, owner.swarmActivities[member.ID], approval, time.Now())
			text, clock := warnedClock(status, warn)
			for _, row := range style.VisualRows(style.Escape(text), ui.StyleClear, max(1, width)) {
				b.newline()
				b.write(ui.CellsToString(row), "muted", "", "")
			}
			if clock != "" {
				if b.col+rw.StringWidth(clock) > b.width {
					b.newline()
				}
				b.write(clock, "active", "", "")
			}
			for _, detail := range append(details, agentActivityDetails(snapshot, member)...) {
				for _, row := range style.VisualRows(style.Escape(detail), ui.StyleClear, max(1, width)) {
					b.newline()
					b.item(ui.CellsToString(row), "muted", "", "")
				}
			}
			if w.viewState(i.target).watch {
				tools := agentOverviewLiveTools(r.agentOverviewState())
				for _, tool := range tools[:min(3, len(tools))] {
					b.newline()
					b.write(ui.CellsToString(style.ParseCells(tool.row.inlineLineAt(width, ""), ui.StyleClear)), "muted", "", "")
				}
			}
			agentNotices()
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
			var actions []inspectorHeaderAction
			if busy {
				actions = append(actions, inspectorHeaderAction{"Stop", "stop", !canceling, false})
			}
			if approval {
				actions = append(actions, inspectorHeaderAction{"Review approval", "review", true, true})
			}
			agentTitle(actions...)
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
			agentNotices()
		}
	} else if i.target.kind == conversationViewKind && isRoot {
		// The root's own line: how its last swarm turn ended, or what the
		// swarm still needs from it.
		if p, ok := r.parentPresentation(root); ok && tabHasSwarm(root) && parentInformative(p) {
			b.newline()
			b.item(p.Display, "muted", "", "")
		}
	}
	return b.layout(height)
}
