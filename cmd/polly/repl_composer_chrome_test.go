package main

import (
	"image"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/messages"
	rw "github.com/mattn/go-runewidth"
	ui "github.com/metaspartan/gotui/v5"
)

func TestComposerBoundaryRoles(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		setup      func(*replModel)
		inspector  bool
	}{
		{name: "visible", want: "accent"},
		{name: "busy is not progress", want: "accent", setup: func(m *replModel) { m.busy = true }},
		{name: "approval", want: "active", setup: func(m *replModel) { m.approval = &approvalState{} }},
		{name: "disabled", want: "muted", setup: func(m *replModel) { m.affordances.enabled = false }},
		{name: "quiet", want: "muted", setup: func(m *replModel) { m.quiet = true }},
		{name: "hidden", want: "muted", setup: func(m *replModel) { m.hidden = true }},
		{name: "modal", want: "muted", setup: func(m *replModel) { m.modal = &replModal{} }},
		{name: "unfocused", want: "muted", setup: func(m *replModel) { m.focusKnown = true }},
		{name: "focused", want: "accent", setup: func(m *replModel) { m.focusKnown, m.focused = true, true }},
		{name: "inspector", want: "muted", inspector: true},
		{name: "approval behind inspector", want: "muted", inspector: true, setup: func(m *replModel) { m.approval = &approvalState{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newReplModel()
			m.affordances.enabled = true
			if tc.setup != nil {
				tc.setup(m)
			}
			if got := m.composerBoundaryRole(tc.inspector); got != tc.want {
				t.Fatalf("boundary role = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestComposerBoundaryDoesNotAcknowledgeSubmission(t *testing.T) {
	for _, busy := range []bool{false, true} {
		for _, attachment := range []bool{false, true} {
			r := newManagedREPL(&Config{}, "ctx", 0, 0)
			m := r.model
			m.affordances.enabled, m.busy = true, busy
			prompt := "hello"
			if attachment {
				path := filepath.Join(t.TempDir(), "image.png")
				writeImageFixture(t, path, 8, 8)
				prompt += " " + m.registerAttachment(path, "image.png")
			}
			l := r.frameLayoutFor(80, 24)
			before := m.dividerRow(l)
			m.ed.setText(prompt)
			r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
			if got := m.dividerRow(l); got != before {
				t.Fatal("submission or preparation changed the composer boundary")
			}
			awaitReferencePreparation(t, r)
			if m.ed.text() != "" || len(m.hist.entries) != 1 || busy && len(m.queue) != 1 {
				t.Fatalf("input was not accepted: queued=%v attachment=%v", busy, attachment)
			}
			if got := m.dividerRow(l); got != before {
				t.Fatal("accepted or queued input flashed the composer boundary")
			}
		}
	}
}

func TestComposerBoundaryCharacterRuleAndCallerLink(t *testing.T) {
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	m := r.model
	m.affordances.enabled = true
	for _, width := range []int{1, 2, 4, 24, 80} {
		l := r.frameLayoutFor(width, 24)
		for _, inactive := range []bool{false, true} {
			row := m.dividerRow(l, inactive)
			if plainStyledText(row) != strings.Repeat("─", width) {
				t.Fatalf("rule changed glyphs/width: %q", row)
			}
			cells := style.ParseCells(row, ui.NewStyle(ui.ColorClear))
			for x, cell := range cells {
				role := "muted"
				if !inactive && x < composerBoundaryAccentCols {
					role = "accent"
				}
				want := style.ParseCells(style.Styled("─", role, ""), ui.NewStyle(ui.ColorClear))[0]
				if cell != want {
					t.Fatalf("width %d cell %d = %+v, want %+v", width, x, cell, want)
				}
			}
		}
		m.status.parentName = "caller"
		for _, joined := range []bool{false, true} {
			l.chrome.joined = joined
			l.chrome.main = image.Rectangle{}
			baseline := plainStyledText(m.dividerRow(l))
			hit := m.parentLink
			m.approval = &approvalState{}
			if row := plainStyledText(m.dividerRow(l)); row != baseline || rw.StringWidth(row) != width || m.parentLink != hit || hit.Empty() {
				t.Fatalf("approval moved caller link: row=%q hit=%v, want row=%q hit=%v", row, m.parentLink, baseline, hit)
			}
			m.approval = nil
		}
		m.status.parentName = ""
	}
	for _, quiet := range []bool{false, true} {
		m.status.parentName, m.quiet = "caller", quiet
		height := 2
		if quiet {
			height = 24
		}
		if row := m.dividerRow(r.frameLayoutFor(80, height)); row != "" || !m.parentLink.Empty() {
			t.Fatal("hidden divider retained chrome or caller hitbox")
		}
	}
}

func TestComposerBoundaryKeepsCueWithJoinedInspector(t *testing.T) {
	withDisplayTTY(t)
	r, screen := chromeTestREPL(t)
	m := r.model
	m.affordances.enabled = true
	m.appendNoticeLine("inspected body")
	r.inspect(tabViewTarget(r.visibleTab()))
	for _, width := range []int{140, 80} {
		screen.SetSize(width, 40)
		waitInspector(t, r, width)
		for _, cue := range []string{"accent", "active", "muted"} {
			m.approval = nil
			r.workspace().inspector.focused = cue == "muted"
			if cue == "active" {
				m.approval = &approvalState{calls: []messages.ChatMessageToolCall{{Name: "read_file"}}}
			}
			r.render()
			y := r.frameLayoutFor(width, 40).composerRow(0) - 1
			for x := 0; x <= composerBoundaryAccentCols; x++ {
				want := chromeColor(cue)
				if x == composerBoundaryAccentCols {
					want = chromeColor("muted")
					if width == 80 && r.inspectorFocused() {
						want = ui.ColorClear // The original focused frame styling.
					} else if width == 80 && m.approval != nil {
						want = chromeColor("active")
					}
				}
				glyph, st, _ := screen.Get(x, y)
				wantGlyph := "─"
				if width == 80 && x == 0 {
					wantGlyph = "╰"
				}
				if glyph != wantGlyph || st.GetForeground() != want {
					t.Fatalf("width=%d cue=%s x=%d: got %q/%v, want %q/%v", width, cue, x, glyph, st.GetForeground(), wantGlyph, want)
				}
			}
		}
	}
}

func TestComposerBoundaryAcceptanceDoesNotScheduleRepaint(t *testing.T) {
	r, _ := affordanceTestREPL(t)
	m := r.model
	m.ed.setText("draft") // Exclude the independent idle-cursor animation.
	r.recordAcceptedInput("accepted")
	r.render()
	before := r.painter.buffer
	now := time.Now()
	for i := range 20 {
		r.tickAffordances(now.Add(time.Duration(i) * 50 * time.Millisecond))
		if r.painter.buffer != before || r.needsTick() {
			t.Fatal("input acceptance requested a repaint")
		}
	}
}

func TestComposerBoundaryInspectorFocusDoesNotPerpetuallyTick(t *testing.T) {
	r, _ := affordanceTestREPL(t)
	m := r.model
	m.affordances.inputAt = time.Now().Add(-time.Hour)
	r.workspace().inspector.open, r.workspace().inspector.focused = true, true
	for i := range 20 {
		r.tickAffordances(time.Now().Add(time.Duration(i) * time.Second))
		if r.painter.buffer != nil {
			t.Fatal("unfocused composer requested an idle-cursor repaint")
		}
	}
}
