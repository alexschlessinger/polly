package main

import (
	"errors"
	"fmt"
	"image"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/swarm"
	rw "github.com/mattn/go-runewidth"
	ui "github.com/metaspartan/gotui/v5"
)

func usagePopupState() *swarm.State {
	input, output := 4000, 40
	return &swarm.State{
		Members: map[string]*swarm.Member{
			"worker-a": {ID: "worker-a", Label: "Parser reviewer", Model: "test/reviewer"},
			"worker-b": {ID: "worker-b", Label: "Failed worker", Model: "test/other"},
		},
		Executions: map[string]*swarm.Execution{
			"exec-a": {ID: "exec-a", Member: "worker-a", Status: "completed", Workspace: "released", Usage: swarm.Usage{Samples: 1, InputTokens: &input, OutputTokens: &output}},
			"exec-b": {ID: "exec-b", Member: "worker-a", Status: "running", Workflow: "workflow", Request: swarm.AgentRequest{Session: "worker-a"}, Usage: swarm.Usage{Samples: 1, InputTokens: &input, OutputTokens: &output}},
			"exec-c": {ID: "exec-c", Member: "worker-b", Status: "failed", Usage: swarm.Usage{Samples: 1, OutputTokens: &output}},
		},
	}
}

func seedUsagePopup(r *managedREPL) {
	d := r.model.modal.context
	d.snapshot.usage = contextSessionUsage{}
	d.snapshot.usage.addExecutions(usagePopupState())
	d.nextRead = time.Now().Add(time.Hour)
}

func TestContextUsageOverviewColumns(t *testing.T) {
	r := contextDashboardREPL(t, false)
	seedUsagePopup(r)
	r.render()
	var ends []int
	for _, item := range r.model.modal.items {
		line := plainStyledText(item.display)
		if strings.HasPrefix(strings.TrimSpace(line), "Parent ") || item.value == contextUsageAction || strings.HasPrefix(strings.TrimSpace(line), "Total ") {
			ends = append(ends, rw.StringWidth(line))
			fields := strings.Fields(line)
			output := fields[len(fields)-1]
			inputEnd := rw.StringWidth(strings.TrimRight(strings.TrimSuffix(line, output), " "))
			if inputEnd != 29 {
				t.Fatalf("input column ends at %d, want 29: %q", inputEnd, line)
			}
		}
	}
	if !reflect.DeepEqual(ends, []int{44, 44, 44}) {
		t.Fatalf("usage column ends = %v, want aligned rows", ends)
	}
}

func TestContextUsagePopupKeepsContextSize(t *testing.T) {
	for _, count := range []int{0, 1, 12} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			r := contextDashboardREPL(t, false)
			m, d := r.model.modal, r.model.modal.context
			d.nextRead = time.Now().Add(time.Hour)
			for i := 0; i < count; i++ {
				d.snapshot.usage.agents = append(d.snapshot.usage.agents, contextAgentUsage{id: fmt.Sprint(i), label: fmt.Sprintf("Agent %d", i)})
			}
			for _, size := range []image.Point{{100, 40}, {28, 12}, {80, 30}} {
				ui.DefaultBackend.Screen.SetSize(size.X, size.Y)
				r.render()
				bounds := m.bounds
				d.activate(m, contextUsageAction)
				r.render()
				if m.bounds != bounds {
					t.Fatalf("size %v: usage bounds %v, want Context bounds %v", size, m.bounds, bounds)
				}
				if count == 12 {
					r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<End>"})
					r.render()
					if m.top == 0 || !strings.Contains(r.modalW.Text, "Agent 11") || r.modalScrollbar.thumb.Empty() {
						t.Fatal("overflowing agents did not scroll to the last agent")
					}
				}
				d.activate(m, contextBackAction)
				r.render()
				if m.bounds != bounds {
					t.Fatal("Back changed the Context dimensions")
				}
			}
		})
	}
}

func TestContextUsagePopupDetailsMatchRows(t *testing.T) {
	r := contextDashboardREPL(t, false)
	seedUsagePopup(r)
	m, d := r.model.modal, r.model.modal.context
	d.snapshot.usage.agents[0].label = strings.Repeat("Named worker ", 6)
	check := func() {
		t.Helper()
		if m.details == nil || len(m.details) != len(m.items) {
			t.Fatalf("details rows = %d, want %d non-nil rows", len(m.details), len(m.items))
		}
		for i, item := range m.items {
			if want := strings.TrimSpace(item.label); m.details[i] != want {
				t.Fatalf("details row %d = %q, want rendered label %q", i, m.details[i], want)
			}
		}
	}
	d.activate(m, contextUsageAction)
	check() // Navigation installs both representations before the next paint.
	for _, size := range []image.Point{{100, 40}, {28, 12}, {80, 30}} {
		ui.DefaultBackend.Screen.SetSize(size.X, size.Y)
		r.render()
		check()
	}
	d.snapshot.usage.err = errors.New("coordination unavailable")
	r.render()
	check()
	if m.titleNotice == "" || strings.Contains(strings.Join(m.details, "\n"), "Named worker") {
		t.Fatal("error retained stale usage details or lost its notice")
	}
}

func TestContextUsageBars(t *testing.T) {
	for _, tc := range []struct {
		name     string
		count    contextTokenTotal
		total    int
		fraction float64
		value    string
	}{
		{name: "known", count: contextTokenTotal{tokens: 30}, total: 40, fraction: .75, value: "30"},
		{name: "partial", count: contextTokenTotal{tokens: 30, missing: true}, total: 40, fraction: .75, value: "≥30"},
		{name: "unknown", count: contextTokenTotal{missing: true}, value: "—"},
		{name: "zero", value: "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, label := range []string{"↑ in", "↓ out"} {
				for _, width := range []int{46, 18} {
					text := plainStyledText(contextUsageBar(label, tc.count, tc.total, width, 5))
					want := fmt.Sprintf("%-7s", label) + contextBar(tc.fraction, width-13) + strings.Repeat(" ", 6-rw.StringWidth(tc.value)) + tc.value
					if text != want || rw.StringWidth(text) != width {
						t.Fatalf("bar width %d: %q, want %q", width, text, want)
					}
				}
			}
		})
	}
	var usage contextSessionUsage
	usage.addExecutions(usagePopupState())
	usage.agents[0].label = strings.Repeat("Agent 名称 ", 10) + "[red]"
	d := &contextPopover{snapshot: contextSnapshot{usage: usage}}
	for _, width := range []int{50, 24, 12} {
		for _, item := range d.usageItems(width) {
			if rw.StringWidth(plainStyledText(item.display)) > width-3 {
				t.Fatalf("wrapped row exceeds width %d: %q", width, item.display)
			}
		}
	}
}

func TestContextUsagePopupKeyboardBackAndChart(t *testing.T) {
	r := contextDashboardREPL(t, true)
	seedUsagePopup(r)
	m, d := r.model.modal, r.model.modal.context
	r.model.ed.setText("untouched draft")
	before := transcriptTexts(r.model)
	key := func(id string) {
		r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: id})
		r.render()
	}
	key("<Tab>")
	if m.selectedValue() != contextUsageAction {
		t.Fatal("Tab did not select Subagents")
	}
	top, selected := m.top, m.selected
	r.scrollDrag = scrollDragState{pane: "modal"}
	key("<Enter>")
	if r.model.modal != m || !d.usageOpen || m.title != "Subagent usage" || r.scrollDrag.pane != "" {
		t.Fatal("usage did not open in the existing modal or retained its drag")
	}
	if d.ctx.Err() != nil || len(r.images.Placements()) != 0 {
		t.Fatal("usage canceled its reader or retained the context chart")
	}
	text := strings.Join(m.details, "\n")
	for _, want := range []string{"Parser reviewer", "Failed worker", "↑ in", "↓ out", "8.0k", "80", "40", "—", "█"} {
		if !strings.Contains(text, want) {
			t.Fatalf("usage missing %q:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"test/reviewer", "worker-a", "exec-a", "completed", "follow-up", "workflow", "workspace released", "Total", "Subtotal"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("usage retained execution detail %q:\n%s", unwanted, text)
		}
	}
	key("<End>")
	key("<Enter>") // A diagnostic row is not a close action.
	if r.model.modal != m || !d.usageOpen {
		t.Fatal("Enter on a diagnostic closed the breakdown")
	}
	key("<Escape>")
	if d.usageOpen || m.title != "Context" || m.top != top || m.selected != selected || d.ctx.Err() != nil {
		t.Fatal("Back did not restore the live overview and viewport")
	}
	if len(r.images.Placements()) != 1 {
		t.Fatal("Back did not restore the context chart")
	}
	for _, back := range []string{"<Left>", "<Backspace>", "<Enter>"} {
		key("<Right>")
		if !d.usageOpen {
			t.Fatal("Right did not activate Subagents")
		}
		key(back) // The Back action is initially selected.
		if d.usageOpen || d.ctx.Err() != nil {
			t.Fatalf("%s did not return without canceling the reader", back)
		}
	}
	if r.model.ed.text() != "untouched draft" || !reflect.DeepEqual(before, transcriptTexts(r.model)) {
		t.Fatal("usage navigation changed the draft or transcript")
	}
	key("<Escape>")
	if r.model.modal != nil || d.ctx.Err() == nil {
		t.Fatal("Escape from the overview did not close and cancel its reader")
	}
}

func TestContextUsagePopupMouseAndNarrowRows(t *testing.T) {
	r := contextDashboardREPL(t, false)
	seedUsagePopup(r)
	m, d := r.model.modal, r.model.modal.context
	d.snapshot.usage.agents = append(d.snapshot.usage.agents, contextAgentUsage{id: "worker-c", label: "Queued worker"})
	screen := ui.DefaultBackend.Screen
	screen.SetSize(28, 12)
	r.render()
	// The Subagents total wraps. Every continuation must remain clickable.
	var actionRows []int
	for i, item := range m.items {
		if item.value == contextUsageAction {
			actionRows = append(actionRows, i)
		}
	}
	if len(actionRows) < 2 {
		t.Fatal("fixture did not wrap the action")
	}
	for _, index := range actionRows {
		m.selected = index
		r.render()
		p := image.Pt(m.listBounds.Min.X+1, m.listBounds.Min.Y+index-m.top)
		r.mousePositionKnown = true
		if hover := r.hoverTargetAt(p); hover.rect.Empty() {
			t.Fatal("wrapped action has no hover target")
		}
		r.handleEvent(ui.Event{Type: ui.MouseEvent, ID: "<MouseLeft>", Payload: ui.Mouse{X: p.X, Y: p.Y}})
		r.render()
		if !d.usageOpen || m.title != "Subagent usage" {
			t.Fatal("wrapped usage row did not open the breakdown")
		}
		for _, item := range m.items {
			if rw.StringWidth(plainStyledText(item.display)) > m.bounds.Dx()-2 {
				t.Fatalf("narrow breakdown row overflows: %q", item.label)
			}
		}
		// Ordinary diagnostic rows remain inert and have no hover target.
		p = image.Pt(m.listBounds.Min.X+1, m.listBounds.Min.Y+2)
		if hover := r.hoverTargetAt(p); !hover.rect.Empty() {
			t.Fatal("diagnostic row advertised an action")
		}
		r.handleEvent(ui.Event{Type: ui.MouseEvent, ID: "<MouseLeft>", Payload: ui.Mouse{X: p.X, Y: p.Y}})
		if !d.usageOpen {
			t.Fatal("diagnostic click navigated away")
		}
		p = image.Pt(m.listBounds.Min.X+1, m.listBounds.Min.Y)
		r.handleEvent(ui.Event{Type: ui.MouseEvent, ID: "<MouseLeft>", Payload: ui.Mouse{X: p.X, Y: p.Y}})
		r.render()
		if d.usageOpen || d.ctx.Err() != nil {
			t.Fatal("Back click did not retain the live reader")
		}
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Tab>"})
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
	r.render()
	r.handleEvent(ui.Event{Type: ui.MouseEvent, ID: "<MouseWheelDown>", Payload: ui.Mouse{X: m.bounds.Min.X, Y: m.bounds.Min.Y}})
	r.render()
	if m.selected <= 0 {
		t.Fatal("usage wheel did not scroll")
	}
	r.setScrollTop("modal", len(m.items))
	r.render()
	if m.top == 0 {
		t.Fatal("usage scrollbar did not scroll")
	}
	screen.SetSize(80, 30)
	r.render()
	if !d.usageOpen || m.top+m.visible > len(m.items) {
		t.Fatal("resize lost the page or left an invalid viewport")
	}
	r.handleEvent(ui.Event{Type: ui.MouseEvent, ID: "<MouseLeft>", Payload: ui.Mouse{X: 0, Y: 0}})
	if r.model.modal != nil || d.ctx.Err() == nil {
		t.Fatal("outside click did not dismiss and cancel the drilldown")
	}
}

func TestContextUsagePopupBackPreservesAction(t *testing.T) {
	for _, scenario := range []string{"first request arrives", "narrower", "wider"} {
		t.Run(scenario, func(t *testing.T) {
			r := contextDashboardREPL(t, false)
			m, d := r.model.modal, r.model.modal.context
			d.nextRead = time.Now().Add(time.Hour)
			if scenario == "first request arrives" {
				d.snapshot.trend = nil
			} else if scenario == "wider" {
				ui.DefaultBackend.Screen.SetSize(28, 12)
			}
			r.render()
			key := func(id string) {
				r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: id})
				r.render()
			}
			key("<Tab>")
			if scenario == "wider" {
				key("<Down>") // Select a wrapped continuation that disappears when widened.
			}
			if m.selectedValue() != contextUsageAction {
				t.Fatal("fixture did not select the usage link")
			}
			key("<Enter>")
			switch scenario {
			case "first request arrives":
				d.snapshot.trend = []contextTrendPoint{{tokens: 1200}}
			case "narrower":
				ui.DefaultBackend.Screen.SetSize(28, 12)
			case "wider":
				ui.DefaultBackend.Screen.SetSize(100, 40)
			}
			r.render()
			key("<Escape>")
			if m.selectedValue() != contextUsageAction {
				t.Fatalf("Back lost Subagents focus: selected %q", m.items[m.selected].label)
			}
			key("<Enter>")
			if r.model.modal != m || !d.usageOpen {
				t.Fatal("Enter after Back did not reopen usage")
			}
		})
	}
}

func TestContextUsagePopupEmptyErrorsAndRefresh(t *testing.T) {
	r := contextDashboardREPL(t, false)
	m, d := r.model.modal, r.model.modal.context
	d.nextRead = time.Now().Add(time.Hour)
	d.activate(m, contextUsageAction)
	r.render()
	if !strings.Contains(r.modalW.Text, "No subagents yet") {
		t.Fatal("empty state missing")
	}
	seedUsagePopup(r)
	ui.DefaultBackend.Screen.SetSize(100, 10)
	r.render()
	for i, item := range m.items {
		if item.identity == "agent:worker-b:output:0" {
			m.selected = i
		}
	}
	r.render()
	identity := m.items[m.selected].identity
	relative := m.selected - m.top
	updated := usagePopupState()
	updated.Members["worker-0"] = &swarm.Member{ID: "worker-0", Label: "New worker"}
	updated.Executions["exec-0"] = &swarm.Execution{ID: "exec-0", Member: "worker-0", Status: "queued"}
	updated.Executions["exec-b"].Status = "completed"
	snapshot := d.snapshot
	snapshot.usage = contextSessionUsage{}
	snapshot.usage.addExecutions(updated)
	d.pending = make(chan contextSnapshotResult, 1)
	d.pending <- contextSnapshotResult{snapshot: snapshot}
	r.refreshContextPopover(time.Now())
	r.render()
	if !d.usageOpen || m.items[m.selected].identity != identity || m.selected-m.top != relative {
		t.Fatal("snapshot insertion moved the selected usage row or closed the page")
	}
	if !strings.Contains(strings.Join(m.details, "\n"), "New worker") {
		t.Fatal("breakdown did not refresh from the new snapshot")
	}
	for _, failedRead := range []bool{false, true} {
		d.err = nil
		d.snapshot.usage.err = nil
		if failedRead {
			d.err = errors.New("history unavailable")
		} else {
			d.snapshot.usage.err = errors.New("coordination unavailable")
		}
		r.render()
		text := strings.Join(m.details, "\n")
		if !strings.Contains(text, "unavailable") || strings.Contains(text, "Parser reviewer") || strings.Contains(text, "Total") {
			t.Fatalf("error showed stale/empty totals: %s", text)
		}
		if m.titleNotice == "" || !d.usageOpen {
			t.Fatal("error did not retain the drilldown with an unavailable notice")
		}
	}
	d.err, d.snapshot.usage.err = nil, nil
	r.render()
	if m.titleNotice != "" || !strings.Contains(strings.Join(m.details, "\n"), "Parser reviewer") {
		t.Fatal("recovery did not restore the breakdown")
	}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-c>"})
	if r.model.modal != nil || d.ctx.Err() == nil {
		t.Fatal("Ctrl-C did not dismiss the breakdown")
	}
}
