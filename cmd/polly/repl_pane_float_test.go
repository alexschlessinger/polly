package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/headlessscreen"
	"github.com/alexschlessinger/pollytool/sessions"
	ui "github.com/metaspartan/gotui/v5"
)

// floatScreen plays the proxy of the window showing kind: it waits for the
// host to open the window and connects a terminal of the given size.
func floatScreen(t *testing.T, r *managedREPL, kind floatKind, width, height int) (*satellitePane, *headlessscreen.Screen) {
	t.Helper()
	p := r.panes.floatOf(kind)
	if p == nil {
		t.Fatal("no window was asked for")
	}
	pumpUI(t, r, func() bool { return p.blockID != "" })
	screen, err := headlessscreen.New(width, height)
	if err != nil {
		t.Fatal(err)
	}
	r.attachPane(p.token, screen, nil)
	paintFloats(r)
	return p, screen
}

// paintFloats paints a frame, and the windows too where the test's REPL has
// no screen of its own for a frame to be painted on.
func paintFloats(r *managedREPL) {
	r.render()
	// A REPL with no screen of its own paints nothing, and still follows the
	// session on screen.
	r.syncPanels()
	r.renderFloats()
}

func floatKey(r *managedREPL, p *satellitePane, id string) {
	r.handlePaneEvent(paneEvent{pane: p, ev: ui.Event{Type: ui.KeyboardEvent, ID: id}})
	paintFloats(r)
}

// inTab reports whether a window's place is inside the tab, at its right.
func inTab(spot [4]float64) bool {
	x, y, w, h := spot[0], spot[1], spot[2], spot[3]
	return x > 0 && y > 0 && w >= 0.2 && h >= 0.1 && x+w < 1 && y+h < 1 && x+w > 0.9
}

func TestTheChecklistFloatsInTheHost(t *testing.T) {
	r, fake, _ := paneTestREPL(t)
	plan := progressFixture()
	r.model.setPlanProgress(plan, plan.Todos)
	open := func() {
		r.model.mu.Lock()
		r.openTodoPanel()
		r.model.mu.Unlock()
	}

	open()
	if r.model.modal != nil {
		t.Fatal("the checklist opened over polly's own screen")
	}
	p, screen := floatScreen(t, r, floatChecklist, 44, 10)
	if spot := fake.getFloats()[0]; !inTab(spot) || spot[1] > 0.1 {
		t.Fatalf("the window was put at %v, want the tab's top right", spot)
	}
	text := screenText(t, screen)
	if !strings.Contains(text, "TODO") || !strings.Contains(text, "1/3") || !strings.Contains(text, "Rank exact matches") {
		t.Fatalf("the window does not show the checklist:\n%s", text)
	}

	// It follows the checklist as the turn works through it.
	plan.Todos[1].State = sessions.TodoDone
	plan.Todos = append(plan.Todos, sessions.PlanTodo{Number: 6, Text: "Ship the ranking", State: sessions.TodoPending})
	r.model.mu.Lock()
	r.model.setPlanProgress(plan, plan.Todos)
	r.model.mu.Unlock()
	r.render()
	if text := screenText(t, screen); !strings.Contains(text, "2/4") || !strings.Contains(text, "Ship the ranking") {
		t.Fatalf("the window did not follow the checklist:\n%s", text)
	}

	// Its keys scroll it: in a window three rows tall the list moves.
	short, _ := headlessscreen.New(44, 3)
	p.screen, p.native = themedScreen{p.painter.track(short)}, short
	r.render()
	floatKey(r, p, "<End>")
	if text := screenText(t, short); !strings.Contains(text, "Ship the ranking") || strings.Contains(text, "Match [titles]") {
		t.Fatalf("End did not scroll to the last task:\n%s", text)
	}
	floatKey(r, p, "<Home>")
	if text := screenText(t, short); !strings.Contains(text, "Match [titles]") {
		t.Fatalf("Home did not scroll back:\n%s", text)
	}
	// Escape closes it.
	floatKey(r, p, "<Escape>")
	if r.panes.floatOf(floatChecklist) != nil {
		t.Fatal("Escape left the window open")
	}

	// Opening the panel again closes a window that is up.
	open()
	floatScreen(t, r, floatChecklist, 44, 10)
	open()
	if len(r.panes.floats) != 0 || fake.floated() != 2 || r.model.modal != nil {
		t.Fatalf("opening the panel twice left %d windows of %d", len(r.panes.floats), fake.floated())
	}

	// The window closed in the host is simply gone.
	open()
	third, _ := floatScreen(t, r, floatChecklist, 44, 10)
	if !r.handlePaneEvent(paneEvent{pane: third, lost: true}) || len(r.panes.floats) != 0 || r.panes.floatless {
		t.Fatal("a window closed in the host was not let go")
	}

	// A host that cannot float one leaves the checklist to polly's screen.
	fake.setErr(errors.New("no floating layers"))
	open()
	pumpUI(t, r, func() bool { return r.panes.floatless })
	if p := r.panes.floatOf(floatChecklist); p == nil || !p.own || len(r.panes.floats) != 1 || !strings.Contains(notices(r), "showing it here · no floating layers") {
		t.Fatalf("polly's own screen did not take the window over:\n%s", notices(r))
	}
	r.model.mu.Lock()
	r.openTodoPanel() // Closed,
	r.openTodoPanel() // and open again.
	own := r.panes.floatOf(floatChecklist)
	r.model.mu.Unlock()
	if own == nil || !own.own || fake.floated() != 4 {
		t.Fatalf("a host that cannot float was asked again (%d windows)", fake.floated())
	}
}

func TestTheContextPopoverFloatsInTheHost(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root", "beside")
	fake := hostPanes(t, r)
	open := func() {
		r.model.mu.Lock()
		r.runCommand("/context")
		r.model.mu.Unlock()
	}

	open()
	if r.model.modal != nil {
		t.Fatal("the context popover opened over polly's own screen")
	}
	p, screen := floatScreen(t, r, floatContext, 50, 30)
	if spot := fake.getFloats()[0]; !inTab(spot) || spot[1]+spot[3] > 0.9 {
		t.Fatalf("the window was put at %v, want the tab's right, above the status row", spot)
	}
	text := screenText(t, screen)
	if !strings.Contains(text, "Context") || !strings.Contains(text, "Cache hit") || !strings.Contains(text, "stored") {
		t.Fatalf("the window does not show the context:\n%s", text)
	}
	popover := p.panel.modal.context
	if popover == nil || popover.native {
		t.Fatal("the window's chart was left to images its terminal does not draw")
	}

	// The breakdown opens in the same window, and Escape comes back from it
	// before it closes the window.
	floatKey(r, p, "<Tab>")
	floatKey(r, p, "<Enter>")
	if !popover.usageOpen || !strings.Contains(screenText(t, screen), "Subagent usage") {
		t.Fatalf("Enter on the breakdown's row left:\n%s", screenText(t, screen))
	}
	floatKey(r, p, "<Escape>")
	if popover.usageOpen || r.panes.floatOf(floatContext) != p {
		t.Fatal("Escape did not come back from the breakdown")
	}

	// The window is about the session on screen: another tab's is another
	// popover, and the one left behind stops reading.
	left := popover.ctx
	r.showWorkspace(2 - r.visibleTabIndex())
	paintFloats(r)
	if now := p.panel.modal.context; now == popover || p.panel.owner != r.model || left.Err() == nil {
		t.Fatal("the window kept the popover of a session that is not on screen")
	}

	// Opening it again closes it, and so does Escape.
	open()
	if len(r.panes.floats) != 0 || fake.floated() != 1 {
		t.Fatalf("/context twice left %d windows of %d", len(r.panes.floats), fake.floated())
	}
	open()
	p, _ = floatScreen(t, r, floatContext, 50, 30)
	reading := p.panel.modal.context.ctx
	floatKey(r, p, "<Escape>")
	if len(r.panes.floats) != 0 || reading.Err() == nil {
		t.Fatal("Escape left the window open, or its popover reading")
	}

	// It floats beside the checklist, each in a window of its own.
	open()
	r.model.mu.Lock()
	r.openTodoPanel()
	r.model.mu.Unlock()
	if r.panes.floatOf(floatContext) == nil || r.panes.floatOf(floatChecklist) == nil || len(r.panes.floats) != 2 {
		t.Fatalf("%d windows are open, want the context and the checklist", len(r.panes.floats))
	}
	pumpUI(t, r, func() bool {
		return r.panes.floatOf(floatContext).blockID != "" && r.panes.floatOf(floatChecklist).blockID != ""
	})

	// A host that cannot float leaves the popover built for it to polly's screen.
	r.closeFloats()
	fake.setErr(errors.New("no floating layers"))
	open()
	built := r.panes.floatOf(floatContext).panel.modal
	pumpUI(t, r, func() bool { return r.panes.floatless })
	if p := r.panes.floatOf(floatContext); p == nil || !p.own || p.panel.modal != built || built.context.ctx.Err() != nil {
		t.Fatal("polly's own screen did not take over the popover built for the window")
	}
}

func TestTheViewFloatsOverTheTab(t *testing.T) {
	r, fake, _ := paneTestREPL(t)
	openTestPane(t, r, fake)
	if fake.floated() != 1 || len(fake.getPlaces()) != 0 {
		t.Fatalf("the view opened in %d floating windows and at places %v", fake.floated(), fake.getPlaces())
	}
	// On a 140x40 terminal: the right of the tab, clear of the composer.
	spot := fake.getFloats()[0]
	if !inTab(spot) || spot[2] != 0.6 || spot[1]+spot[3] > 1-5.0/40+1e-9 || spot[3] < 0.8 {
		t.Fatalf("the view's window was put at %v", spot)
	}
}

// overlap reports whether two windows cover any of the same tab.
func overlap(a, b [4]float64) bool {
	return a[0] < b[0]+b[2] && b[0] < a[0]+a[2] && a[1] < b[1]+b[3] && b[1] < a[1]+a[3]
}

func TestFloatingWindowsShareAColumn(t *testing.T) {
	todo := checklistWant
	todo.rows += 3
	for _, size := range [][2]int{{200, 60}, {140, 40}, {80, 24}, {40, 12}} {
		footer := 1 - 5.0/float64(size[1])
		for _, wants := range [][]floatWant{{todo}, {viewWant}, {contextWant}, {todo, viewWant}, {viewWant, contextWant}, {todo, contextWant}, {todo, viewWant, contextWant}} {
			spots := floatLayout(size[0], size[1], wants)
			for n, spot := range spots {
				if spot[0] <= 0 || spot[1] < floatInset-1e-9 || spot[2] <= 0 || spot[3] <= 0.03 || spot[0]+spot[2] > 1 || spot[1]+spot[3] > max(footer, 0.23)+1e-9 {
					t.Fatalf("%v, %d windows: window %d is at %v", size, len(wants), n, spot)
				}
				for m := range n {
					if overlap(spots[m], spot) {
						t.Fatalf("%v: windows %d and %d cover each other: %v %v", size, m, n, spots[m], spot)
					}
					if spots[m][1] > spot[1] {
						t.Fatalf("%v: window %d is under window %d", size, m, n)
					}
					if spots[m][0] != spot[0] || spots[m][2] != spot[2] {
						t.Fatalf("%v: windows %d and %d do not line up in one column: %v %v", size, m, n, spots[m], spot)
					}
				}
				if wants[n].fill && spot[3] < 0.2 {
					t.Fatalf("%v, %d windows: the view is left %v of the tab's height", size, len(wants), spot[3])
				}
			}
		}
	}
	// With room to spare each window has the rows it asked for, and the view
	// the rest: on a tab 120 rows tall, the checklist's seven and the
	// popover's 34.
	spots := floatLayout(200, 120, []floatWant{todo, viewWant, contextWant})
	if !nearSpot([4]float64{0, 0, 0, spots[0][3]}, [4]float64{0, 0, 0, 0.1}) || !nearSpot([4]float64{0, 0, 0, spots[2][3]}, [4]float64{0, 0, 0, 34.0 / 120}) || spots[1][3] < 0.45 {
		t.Fatalf("with room to spare the windows are %v", spots)
	}
	// Short of room the tallest gives way first: the checklist keeps its
	// rows, the popover scrolls, and the view keeps half the column.
	spots = floatLayout(140, 40, []floatWant{todo, viewWant, contextWant})
	if !nearSpot([4]float64{0, 0, 0, spots[0][3]}, [4]float64{0, 0, 0, 7.0 / 40}) || spots[2][3] < spots[0][3] || spots[1][3] < 0.4 {
		t.Fatalf("short of room the windows are %v", spots)
	}
	// Alone, the popover sits above the footer and the checklist at the top.
	if alone := floatLayout(140, 40, []floatWant{contextWant})[0]; alone[1]+alone[3] < 1-5.0/40-0.001 {
		t.Fatalf("the popover alone is at %v, want just above the footer", alone)
	}
	if alone := floatLayout(140, 40, []floatWant{todo})[0]; alone[1] > floatInset+1e-9 {
		t.Fatalf("the checklist alone is at %v, want the tab's top", alone)
	}
}

func TestWindowsMakeRoomForEachOther(t *testing.T) {
	r, fake, _ := paneTestREPL(t)
	plan := progressFixture()
	r.model.setPlanProgress(plan, plan.Todos)
	settle := func() {
		t.Helper()
		r.render()
		pumpUI(t, r, func() bool {
			if r.panes.arranging {
				return false
			}
			for _, p := range r.panes.floatWindows() {
				if p.blockID == "" {
					return false
				}
			}
			return true
		})
		r.render()
		pumpUI(t, r, func() bool { return !r.panes.arranging })
	}
	clear := func() {
		t.Helper()
		windows := r.panes.floatWindows()
		for n, p := range windows {
			for _, q := range windows[:n] {
				if overlap(p.spot, q.spot) {
					t.Fatalf("two windows cover each other: %v %v", q.spot, p.spot)
				}
			}
		}
	}

	// The view opens alone, down the tab's right.
	r.openAttention()
	settle()
	view := r.panes.pane
	alone := view.spot
	if moves := fake.moved(); len(moves) != 0 || alone[3] < 0.8 {
		t.Fatalf("the view alone is at %v after %d moves", alone, len(moves))
	}

	// The checklist opens above it, and the view is moved down to make room.
	r.model.mu.Lock()
	r.openTodoPanel()
	r.model.mu.Unlock()
	settle()
	todo := r.panes.floatOf(floatChecklist)
	clear()
	if moves := fake.moved(); len(moves) != 1 || moves[0].id != view.blockID || moves[0].to != view.spot || view.spot[1] <= todo.spot[1] {
		t.Fatalf("the view was not moved under the checklist: %+v", moves)
	}
	if got := fake.getFloats()[1]; got != todo.spot || got[1] > floatInset+1e-9 {
		t.Fatalf("the checklist opened at %v, want the tab's top", got)
	}

	// The popover opens under both, and the view gives up more of its room.
	r.model.mu.Lock()
	r.openContextPopover()
	r.model.mu.Unlock()
	settle()
	popover := r.panes.floatOf(floatContext)
	clear()
	if moves := fake.moved(); len(moves) < 2 || moves[len(moves)-1].id != view.blockID || popover.spot[1] <= view.spot[1] {
		t.Fatalf("the view was not moved over the popover: %+v", moves)
	}

	// A window that closes gives its room back.
	r.model.mu.Lock()
	r.openTodoPanel()
	r.model.mu.Unlock()
	settle()
	clear()
	if view.spot[1] > floatInset+1e-9 {
		t.Fatalf("the view did not take the checklist's room: %v after %+v", view.spot, fake.moved())
	}

	// One the user has taken hold of is left where they put it, for good,
	// and the others no longer plan around it.
	fake.setHeld(map[string]bool{view.blockID: true})
	r.model.mu.Lock()
	r.openTodoPanel()
	r.model.mu.Unlock()
	settle()
	if !view.loose {
		t.Fatal("a window the user moved is still polly's to place")
	}
	asked := len(fake.moved())
	r.model.mu.Lock()
	r.openContextPopover()
	r.model.mu.Unlock()
	settle()
	for _, move := range fake.moved()[asked:] {
		if move.id == view.blockID {
			t.Fatalf("a window the user moved was moved again: %+v", move)
		}
	}
	// The checklist, alone again as far as polly plans, has its rows back.
	if todo := r.panes.floatOf(floatChecklist); todo == nil || r.panes.pane != view || len(r.panes.floatWindows()) != 1 {
		t.Fatalf("polly places %d windows, want the checklist alone", len(r.panes.floatWindows()))
	}
}

// shortConnect gives the proxies of this test's panes next to no time to
// connect.
func shortConnect(t *testing.T) {
	t.Helper()
	was := paneConnectTimeout
	paneConnectTimeout = 20 * time.Millisecond
	t.Cleanup(func() { paneConnectTimeout = was })
}

// waitClosed waits for polly to have asked the host to close the terminal id.
func (f *fakePanes) waitClosed(t *testing.T, id string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		f.mu.Lock()
		closed := slices.Contains(f.closed, id)
		f.mu.Unlock()
		if closed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the host was not asked to close %s", id)
		}
	}
}

func (f *fakePanes) moved() []paneMove {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.moves)
}

func (f *fakePanes) failMoves(err error) {
	f.mu.Lock()
	f.arrangeErr = err
	f.mu.Unlock()
}

// settleFloats paints a frame and waits for the host to have opened and moved
// what the frame asked it to.
func settleFloats(t *testing.T, r *managedREPL) {
	t.Helper()
	r.render()
	pumpUI(t, r, func() bool {
		if r.panes.arranging {
			return false
		}
		for _, p := range r.panes.floatWindows() {
			if p.blockID == "" {
				return false
			}
		}
		return true
	})
}

// A window the host opened whose proxy never connects is not left open where
// nobody sees it: polly retires its token, has the host close the terminal,
// and draws the window itself.
func TestAWindowWhoseProxyNeverConnectsIsDrawnHere(t *testing.T) {
	r, fake, _ := paneTestREPL(t)
	shortConnect(t)
	plan := progressFixture()
	r.model.setPlanProgress(plan, plan.Todos)
	r.model.mu.Lock()
	r.openTodoPanel()
	r.model.mu.Unlock()
	p := r.panes.floatOf(floatChecklist)
	if p == nil || p.own {
		t.Fatal("the checklist was not asked of the host")
	}
	pumpUI(t, r, func() bool { return p.own })
	if r.panes.floatOf(floatChecklist) != p || !r.panes.floatless || p.blockID != "" || p.connectTimer != nil {
		t.Fatalf("the window was not taken over whole: floatless=%v block=%q", r.panes.floatless, p.blockID)
	}
	if r.panes.claim(p.token) {
		t.Fatal("a proxy that turns up late would still be admitted")
	}
	if text := notices(r); !strings.Contains(text, "Rex floating window unavailable, showing it here · "+errPaneConnectTimeout.Error()) {
		t.Fatalf("the user was not told why:\n%s", text)
	}
	fake.waitClosed(t, "pane-1")
	r.render()
	if p.rect.Empty() || len(r.panes.floats) != 1 {
		t.Fatalf("polly's own screen does not show the window: %v", p.rect)
	}
}

// The view stays in polly's frame until its pane has a screen to show it on,
// and for good when none ever connects.
func TestTheViewStaysInTheFrameUntilItsPaneConnects(t *testing.T) {
	r, fake, _ := paneTestREPL(t)
	shortConnect(t)
	r.openAttention()
	r.render()
	p := r.panes.pane
	if p == nil {
		t.Fatal("opening the view asked for no pane")
	}
	if r.viewDetached || !r.framed() || r.chrome.frame.Empty() {
		t.Fatal("the view left the frame before its pane had a screen")
	}
	pumpUI(t, r, func() bool { return r.panes.pane == nil })
	r.render()
	if !r.panes.floatless || r.viewDetached || !r.framed() || r.chrome.frame.Empty() || !r.workspace().inspector.open {
		t.Fatalf("the view did not stay in the frame: floatless=%v detached=%v", r.panes.floatless, r.viewDetached)
	}
	if r.panes.claim(p.token) {
		t.Fatal("a proxy that turns up late would still be admitted")
	}
	if text := notices(r); !strings.Contains(text, "Rex floating window unavailable, showing the view here · "+errPaneConnectTimeout.Error()) {
		t.Fatalf("the user was not told why:\n%s", text)
	}
	fake.waitClosed(t, "pane-1")
}

// A move the host fails once is asked for again on a later frame, with the
// window still polly's to place. A host that keeps failing is asked a few
// times and no more, and the window is then left where it is.
func TestAMoveTheHostFailsIsTriedAgain(t *testing.T) {
	r, fake, _ := paneTestREPL(t)
	every := arrangeRetryEvery
	arrangeRetryEvery = 0
	t.Cleanup(func() { arrangeRetryEvery = every })
	plan := progressFixture()
	r.model.setPlanProgress(plan, plan.Todos)
	todo := func() {
		r.model.mu.Lock()
		r.openTodoPanel()
		r.model.mu.Unlock()
	}
	r.openAttention()
	settleFloats(t, r)
	view := r.panes.pane
	alone := view.spot

	// The checklist opens above the view, which the host fails to move down.
	fake.failMoves(errors.New("rex is busy"))
	todo()
	settleFloats(t, r)
	if len(fake.moved()) != 1 || view.loose || view.spot != alone {
		t.Fatalf("after a failed move the view is at %v, loose=%v, in %d moves", view.spot, view.loose, len(fake.moved()))
	}
	fake.failMoves(nil)
	settleFloats(t, r)
	if list := r.panes.floatOf(floatChecklist); len(fake.moved()) != 2 || view.loose || view.spot[1] <= list.spot[1] || r.panes.arrangeFails != 0 {
		t.Fatalf("the move was not tried again: view at %v, loose=%v, in %d moves", view.spot, view.loose, len(fake.moved()))
	}

	// The checklist closes, and the host never gives the view its room back.
	fake.failMoves(errors.New("rex is busy"))
	todo()
	for range arrangeTries {
		settleFloats(t, r)
	}
	if len(fake.moved()) != 2+arrangeTries || !view.loose {
		t.Fatalf("a host that keeps failing was asked %d times, loose=%v", len(fake.moved())-2, view.loose)
	}
	settleFloats(t, r)
	if len(fake.moved()) != 2+arrangeTries {
		t.Fatal("a window the host would not move was moved again")
	}
}

// The checklist's window follows its list: tasks added while it is up make
// it taller. And the windows stay clear of a composer that grows.
func TestWindowsFollowTheChecklistAndTheComposer(t *testing.T) {
	r, fake, _ := paneTestREPL(t)
	plan := progressFixture()
	r.model.setPlanProgress(plan, plan.Todos)
	r.model.mu.Lock()
	r.openTodoPanel()
	r.model.mu.Unlock()
	p, _ := floatScreen(t, r, floatChecklist, 44, 10)
	settleFloats(t, r)
	opened := p.spot
	if len(fake.moved()) != 0 {
		t.Fatalf("the checklist alone was moved: %+v", fake.moved())
	}
	for n := range 6 {
		plan.Todos = append(plan.Todos, sessions.PlanTodo{Number: 10 + n, Text: fmt.Sprintf("Follow-up %d", n), State: sessions.TodoPending})
	}
	r.model.mu.Lock()
	r.model.setPlanProgress(plan, plan.Todos)
	r.model.mu.Unlock()
	settleFloats(t, r)
	moves := fake.moved()
	if len(moves) != 1 || moves[0].id != p.blockID || moves[0].to != p.spot || p.spot[3] < opened[3]+5.0/40 || p.spot[1] != opened[1] {
		t.Fatalf("six more tasks left the window at %v, opened at %v, after %+v", p.spot, opened, moves)
	}

	// The view fills the column under it, down to the rows the composer and
	// the status row take: more of them once the draft has more lines.
	r.openAttention()
	settleFloats(t, r)
	settleFloats(t, r)
	view := r.panes.pane
	if bottom := view.spot[1] + view.spot[3]; bottom > 1-float64(floatFooterRows)/40+1e-9 || bottom < 0.8 {
		t.Fatalf("the view ends at %v of the tab", bottom)
	}
	r.model.mu.Lock()
	r.model.ed.setText(strings.Repeat("a line of the draft\n", 9))
	r.model.mu.Unlock()
	settleFloats(t, r)
	settleFloats(t, r)
	if r.panes.footerRows <= floatFooterRows {
		t.Fatalf("a ten-line draft leaves the footer at %d rows", r.panes.footerRows)
	}
	if bottom := view.spot[1] + view.spot[3]; bottom > 1-float64(r.panes.footerRows)/40+1e-9 {
		t.Fatalf("the view ends at %v of the tab, over a footer of %d rows", bottom, r.panes.footerRows)
	}
}

// waitFocused waits for the host to have been asked to bring n terminals
// forward, and reports which.
func (f *fakePanes) waitFocused(t *testing.T, n int) []string {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		if got := f.focuses(); len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("the host was asked to focus %q, want %d terminals", f.focuses(), n)
		}
	}
}

// Inside a host the panels are its windows, and Tab makes the same round of
// them as on polly's own screen: the host brings each forward in turn, and
// polly's own terminal after the last.
func TestTabGoesRoundTheHostsWindows(t *testing.T) {
	r, fake, _ := paneTestREPL(t)
	r.panes.block = "block-of-polly"
	plan := progressFixture()
	r.model.setPlanProgress(plan, plan.Todos)
	r.model.ed.setText("half a thought")
	r.model.mu.Lock()
	r.openTodoPanel()
	r.model.mu.Unlock()
	todo, _ := floatScreen(t, r, floatChecklist, 44, 10)
	view, _ := openTestPane(t, r, fake)
	if todo.own || !r.viewDetached || len(fake.focuses()) != 0 {
		t.Fatalf("the panels are not the host's windows, or one was focused unasked: %q", fake.focuses())
	}

	// From the composer to the checklist's window: a host's window has the
	// keys of its own terminal, so nothing on polly's screen takes them.
	ownKey(r, "<Tab>")
	if got := fake.waitFocused(t, 1); got[0] != todo.blockID || r.focusedOwnWindow() != nil {
		t.Fatalf("Tab in the composer focused %q, want the checklist's %s", got, todo.blockID)
	}
	// There Tab chooses the window's action first, and then moves on.
	floatKey(r, todo, "<Tab>")
	if todo.panel.modal.selectedValue() != planViewAction || len(fake.focuses()) != 1 {
		t.Fatalf("Tab in the checklist's window chose %q and focused %q", todo.panel.modal.selectedValue(), fake.focuses())
	}
	floatKey(r, todo, "<Tab>")
	if got := fake.waitFocused(t, 2); got[1] != view.blockID {
		t.Fatalf("Tab in the checklist's window focused %q, want the view's %s", got, view.blockID)
	}
	// After the last window the keys are polly's own terminal's again.
	paneKey(r, view, "<Tab>")
	if got := fake.waitFocused(t, 3); got[2] != "block-of-polly" {
		t.Fatalf("Tab in the last window focused %q, want polly's own terminal", got)
	}
	if r.model.ed.text() != "half a thought" || r.panes.floatOf(floatChecklist) != todo || r.panes.pane != view {
		t.Fatalf("going round changed the draft to %q, or closed a window", r.model.ed.text())
	}

	// A command being typed keeps Tab to complete it.
	r.model.ed.setText("/mod")
	ownKey(r, "<Tab>")
	r.model.ed.setText("")
	ownKey(r, "!")
	if got := fake.focuses(); len(got) != 3 {
		t.Fatalf("Tab on a command focused a window: %q", got)
	}
}

// Alt-P in a window of the host's docks its panel beside polly, or floats it
// again: the window is opened the other way, with the same dialog and the
// keys, and which way is remembered for later launches.
func TestAltPDocksAWindowOfTheHosts(t *testing.T) {
	r, fake, _ := paneTestREPL(t)
	t.Cleanup(func() { _ = saveDockedPanels(nil) })
	saved := func() string {
		value, _ := userConfigValue(envVarPanes)
		return value
	}
	plan := progressFixture()
	r.model.setPlanProgress(plan, plan.Todos)
	r.model.mu.Lock()
	r.openTodoPanel()
	r.model.mu.Unlock()
	floating, _ := floatScreen(t, r, floatChecklist, 44, 10)
	dialog := floating.panel.modal

	floatKey(r, floating, "<M-p>")
	docked, _ := floatScreen(t, r, floatChecklist, 44, 10)
	if docked == floating || docked.own || !docked.docked || docked.panel.modal != dialog || len(fake.docksOpened()) != 1 || fake.floated() != 1 {
		t.Fatalf("Alt-P left the checklist in %d floating windows and %d panes, docked=%v", fake.floated(), len(fake.docksOpened()), docked.docked)
	}
	if saved() != "todo" || len(r.panes.floats) != 1 {
		t.Fatalf("docking saved %q with %d windows open", saved(), len(r.panes.floats))
	}
	if got := fake.waitFocused(t, 1); got[0] != docked.blockID {
		t.Fatalf("the pane opened for it did not take the keys: %q", got)
	}

	floatKey(r, docked, "<M-p>")
	again, _ := floatScreen(t, r, floatChecklist, 44, 10)
	if again == docked || again.own || again.docked || again.panel.modal != dialog || fake.floated() != 2 || saved() != "" {
		t.Fatalf("Alt-P again left the checklist docked=%v in %d floating windows, with %q saved", again.docked, fake.floated(), saved())
	}

	// The view's window docks the same way, opened again by the next frame.
	view, _ := openTestPane(t, r, fake)
	paneKey(r, view, "<M-p>")
	pane := r.panes.pane
	if pane == nil || pane == view {
		t.Fatal("Alt-P in the view's window did not open it again")
	}
	pumpUI(t, r, func() bool { return pane.blockID != "" })
	if !pane.docked || saved() != "agents" || len(fake.docksOpened()) != 2 || !r.workspace().inspector.open {
		t.Fatalf("Alt-P left the view docked=%v with %q saved, in %d panes", pane.docked, saved(), len(fake.docksOpened()))
	}
}
