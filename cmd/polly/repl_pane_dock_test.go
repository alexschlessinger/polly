package main

import (
	"testing"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/headlessscreen"
	ui "github.com/metaspartan/gotui/v5"
)

// userDocks plays the user docking p's window into the tab, or floating it
// again: the host has it there, and its terminal reports its new size.
func userDocks(t *testing.T, r *managedREPL, fake *fakePanes, p *satellitePane, docked bool) {
	t.Helper()
	fake.redock(p.blockID, docked)
	r.handlePaneEvent(paneEvent{pane: p, ev: ui.Event{Type: ui.ResizeEvent, ID: "<Resize>"}})
	pumpUI(t, r, func() bool { return !r.panes.locating && p.docked == docked })
}

func TestAPanelTheUserDockedOpensDocked(t *testing.T) {
	r, fake, _ := paneTestREPL(t)
	t.Cleanup(func() { _ = saveDockedPanels(nil) })
	plan := progressFixture()
	r.model.setPlanProgress(plan, plan.Todos)
	todo := func() {
		r.model.mu.Lock()
		r.openTodoPanel()
		r.model.mu.Unlock()
	}
	saved := func() string {
		value, _ := userConfigValue(envVarPanes)
		return value
	}

	// It floats until the user docks it.
	todo()
	p, _ := floatScreen(t, r, floatChecklist, 44, 10)
	if p.docked || fake.floated() != 1 || len(fake.docksOpened()) != 0 || saved() != "" {
		t.Fatalf("the checklist first opened docked=%v, with %q saved", p.docked, saved())
	}
	userDocks(t, r, fake, p, true)
	if !r.panes.docks[floatChecklist] || saved() != "todo" || !loadDockedPanels()[floatChecklist] {
		t.Fatalf("docking the checklist saved %q", saved())
	}
	// Polly no longer places it, and the others no longer plan around it.
	if len(r.panes.floatWindows()) != 0 {
		t.Fatal("a docked window is still polly's to place")
	}

	// From then on it opens as a pane beside polly, and nothing else does.
	todo()
	todo()
	p, _ = floatScreen(t, r, floatChecklist, 44, 10)
	docks := fake.docksOpened()
	if fake.floated() != 1 || len(docks) != 1 || docks[0].under != "" || docks[0].share < dockLeast || docks[0].share > dockMost || !p.docked {
		t.Fatalf("the checklist reopened in %d floating windows and panes %+v", fake.floated(), docks)
	}
	r.openAttention()
	r.render()
	if r.panes.pane == nil || r.panes.pane.docked || fake.floated() != 2 {
		t.Fatalf("the view opened docked, or in no window (%d floating)", fake.floated())
	}
	pumpUI(t, r, func() bool { return r.panes.pane.blockID != "" })
	screen, err := headlessscreen.New(60, 20)
	if err != nil {
		t.Fatal(err)
	}
	r.attachPane(r.panes.pane.token, screen, nil)

	// A second docked panel goes under the first.
	userDocks(t, r, fake, r.panes.pane, true)
	if saved() != "agents,todo" {
		t.Fatalf("docking the view saved %q", saved())
	}
	r.closeInspector()
	r.render()
	r.openAttention()
	r.render()
	pumpUI(t, r, func() bool { return r.panes.pane.blockID != "" })
	if docks := fake.docksOpened(); len(docks) != 2 || docks[1].under != p.blockID || !r.panes.pane.docked {
		t.Fatalf("the view reopened in panes %+v, want under the checklist's %s", docks, p.blockID)
	}

	// Floated again, it floats from then on, where the user put it.
	userDocks(t, r, fake, p, false)
	if r.panes.docks[floatChecklist] || saved() != "agents" || !p.loose {
		t.Fatalf("floating the checklist saved %q, loose=%v", saved(), p.loose)
	}
	todo()
	todo()
	floatScreen(t, r, floatChecklist, 44, 10)
	if fake.floated() != 3 || len(fake.docksOpened()) != 2 || r.panes.floatOf(floatChecklist).docked {
		t.Fatalf("the checklist reopened in %d floating windows and %d panes", fake.floated(), len(fake.docksOpened()))
	}
}

func TestDockedPanelsAreReadFromTheEnvironmentFirst(t *testing.T) {
	t.Cleanup(func() { _ = saveDockedPanels(nil) })
	if err := saveDockedPanels(map[floatKind]bool{floatContext: true, floatView: true}); err != nil {
		t.Fatal(err)
	}
	if docks := loadDockedPanels(); len(docks) != 2 || !docks[floatContext] || !docks[floatView] {
		t.Fatalf("saved panels read back as %v", docks)
	}
	t.Setenv(envVarPanes, "todo, nonsense")
	if docks := loadDockedPanels(); len(docks) != 1 || !docks[floatChecklist] {
		t.Fatalf("the environment's panels read as %v", docks)
	}
	t.Setenv(envVarPanes, "")
	if docks := loadDockedPanels(); len(docks) != 0 {
		t.Fatalf("an empty variable docks %v", docks)
	}
}

// A pane closed in the host widens polly's own terminal before the pane's end
// reaches polly, so polly asks where a window is that the host no longer has.
// That is not the user floating it: the panel stays docked, for this run and
// the launches after it.
func TestAPaneClosedInTheHostIsNotOneTheUserFloated(t *testing.T) {
	r, fake, _ := paneTestREPL(t)
	t.Cleanup(func() { _ = saveDockedPanels(nil) })
	plan := progressFixture()
	r.model.setPlanProgress(plan, plan.Todos)
	todo := func() {
		r.model.mu.Lock()
		r.openTodoPanel()
		r.model.mu.Unlock()
	}
	saved := func() string {
		value, _ := userConfigValue(envVarPanes)
		return value
	}
	todo()
	p, _ := floatScreen(t, r, floatChecklist, 44, 10)
	userDocks(t, r, fake, p, true)
	if saved() != "todo" {
		t.Fatalf("docking the checklist saved %q", saved())
	}

	// The user closes the pane: the host has it no more, and polly's own
	// terminal reports its new size first.
	fake.vanish(p.blockID)
	r.handleEvent(ui.Event{Type: ui.ResizeEvent, ID: "<Resize>"})
	pumpUI(t, r, func() bool { return !r.panes.locating })
	if !p.docked || !r.panes.docks[floatChecklist] || saved() != "todo" {
		t.Fatalf("a pane the host no longer has was taken for one floated: docked=%v, %q saved", p.docked, saved())
	}

	// Then the pane's end arrives, and the next one opens docked as before.
	if !r.handlePaneEvent(paneEvent{pane: p, lost: true}) || r.panes.floatOf(floatChecklist) != nil || saved() != "todo" {
		t.Fatalf("the closed pane was not let go, or %q was saved", saved())
	}
	todo()
	if again, _ := floatScreen(t, r, floatChecklist, 44, 10); !again.docked || fake.floated() != 1 || len(fake.docksOpened()) != 1 {
		t.Fatalf("the checklist reopened in %d floating windows and %d panes", fake.floated(), len(fake.docksOpened()))
	}
}
