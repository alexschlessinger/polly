package main

import (
	"context"
	"errors"
	"fmt"
	"image"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/headlessscreen"
	"github.com/alexschlessinger/pollytool/internal/scratch"
	"github.com/alexschlessinger/pollytool/swarm"
	ui "github.com/metaspartan/gotui/v5"
)

func TestPaneTtyRelaysInputAndSize(t *testing.T) {
	proxy, owner := net.Pipe()
	defer proxy.Close()
	term := newPaneTty(owner, paneSize{cols: 100, rows: 30})
	defer term.Close()
	resized := make(chan bool, 1)
	term.NotifyResize(resized)
	if size, _ := term.WindowSize(); size.Width != 100 || size.Height != 30 {
		t.Fatalf("hello size: %+v", size)
	}

	go func() {
		_ = writePaneFrame(proxy, paneFrameResize, paneSize{cols: 47, rows: 22, pixelW: 846, pixelH: 880}.encode())
		_ = writePaneFrame(proxy, paneFrameInput, []byte("\x1b[A"))
	}()
	buf := make([]byte, 16)
	n, err := term.Read(buf)
	if err != nil || string(buf[:n]) != "\x1b[A" {
		t.Fatalf("input: %q, %v", buf[:n], err)
	}
	select {
	case <-resized:
	case <-time.After(5 * time.Second):
		t.Fatal("resize was not announced")
	}
	if size, _ := term.WindowSize(); size.Width != 47 || size.Height != 22 || size.PixelWidth != 846 {
		t.Fatalf("resized: %+v", size)
	}

	// The proxy going away is what tells tcell the pane is lost.
	_ = proxy.Close()
	if _, err := term.Read(buf); err == nil {
		t.Fatal("read after the proxy closed succeeded")
	}
}

func TestPaneTokenIsSpentOnce(t *testing.T) {
	var h paneHost
	if h.claim("") || h.claim("a") {
		t.Fatal("claimed with no pane waiting")
	}
	h.await("secret")
	h.await("second")
	if h.claim("other") {
		t.Fatal("claimed with the wrong token")
	}
	if !h.claim("secret") || h.claim("secret") {
		t.Fatal("token must admit exactly one connection")
	}
	// Several panes can be awaited at once, each on its own token.
	h.forget("second")
	if h.claim("second") {
		t.Fatal("a forgotten pane was admitted")
	}
}

// fakePanes stands in for Rex: it records what it was asked to open and
// close, and the test plays the proxy by attaching a screen.
type fakePanes struct {
	mu      sync.Mutex
	opened  [][]string
	places  []panePlace
	closed  []string
	focused []string
	// titles are the names polly gave its tab, and named the name the tab
	// has: "" stands for the host's own.
	titles []string
	named  string
	// tiled are the walls opened, a command per pane, and grown the panes
	// split from one since, by the terminal they were split beside.
	tiled [][][]string
	grown []string
	err   error
	// arrangeErr fails a move alone: the windows still open.
	arrangeErr error
	// questions are those put to the user, and lists the choices of each.
	// answer answers a question, or choose picks from its choices by id, ""
	// for none; with neither the fake is busy, and polly asks in its own
	// screen.
	questions []paneQuestion
	lists     [][]paneChoice
	answer    func(context.Context, paneQuestion) (paneAnswer, error)
	choose    func([]paneChoice) (string, error)
	// floats are the windows floated, by the place in the tab each was given.
	floats [][4]float64
	// moves are the windows moved since, and held the ones the user has
	// taken hold of, by id: the fake leaves those where they are.
	moves []paneMove
	held  map[string]bool
	// docks are the panes opened in the tab, and panes the windows' current
	// dock state by id (missing entries report dockUnknown).
	docks []fakeDock
	panes map[string]dockState
}

// fakeDock is a pane the host was asked for: under a terminal, or beside
// polly with a share of its width.
type fakeDock struct {
	under string
	share float64
}

func (f *fakePanes) ask(ctx context.Context, q paneQuestion) (paneAnswer, error) {
	f.mu.Lock()
	f.questions = append(f.questions, q)
	f.lists = append(f.lists, q.choices)
	answer, choose := f.answer, f.choose
	f.mu.Unlock()
	switch {
	case answer != nil:
		return answer(ctx, q)
	case choose != nil:
		id, err := choose(q.choices)
		return paneAnswer{given: id != "" && err == nil, id: id}, err
	}
	return paneAnswer{}, errAskLater
}

func (f *fakePanes) words() hostWords { return hostWords{name: "Rex", tab: "tab"} }

func (f *fakePanes) listed() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.lists)
}

// asked is the question put to the user last.
func (f *fakePanes) asked() paneQuestion {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.questions) == 0 {
		return paneQuestion{}
	}
	return f.questions[len(f.questions)-1]
}

// The fake's own terms for a place are polly's: a window is where it was put.
func (f *fakePanes) float(_ context.Context, argv []string, _ string, spot [4]float64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened = append(f.opened, argv)
	f.floats = append(f.floats, spot)
	id := fmt.Sprintf("pane-%d", len(f.opened))
	if f.err == nil {
		if f.panes == nil {
			f.panes = map[string]dockState{}
		}
		f.panes[id] = dockFloating
	}
	return id, f.err
}

func (f *fakePanes) dock(_ context.Context, argv []string, _, under string, share float64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened = append(f.opened, argv)
	f.docks = append(f.docks, fakeDock{under: under, share: share})
	id := fmt.Sprintf("pane-%d", len(f.opened))
	if f.err == nil {
		if f.panes == nil {
			f.panes = map[string]dockState{}
		}
		f.panes[id] = dockDocked
	}
	return id, f.err
}

func (f *fakePanes) docked(_ context.Context, ids []string) ([]dockState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	states := make([]dockState, len(ids))
	for n, id := range ids {
		states[n] = f.panes[id]
	}
	return states, nil
}

// redock plays the user docking the window id into the tab, or floating it
// again.
func (f *fakePanes) redock(id string, docked bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.panes == nil {
		f.panes = map[string]dockState{}
	}
	if docked {
		f.panes[id] = dockDocked
	} else {
		f.panes[id] = dockFloating
	}
}

// vanish removes id from the host as if the user closed its pane before the
// proxy's EOF reached polly.
func (f *fakePanes) vanish(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.panes, id)
}

func (f *fakePanes) docksOpened() []fakeDock {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.docks)
}

func (f *fakePanes) arrange(_ context.Context, moves []paneMove) ([]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	moved := make([]bool, len(moves))
	for n, move := range moves {
		f.moves = append(f.moves, move)
		moved[n] = !f.held[move.id] && f.panes[move.id] != dockDocked
	}
	if f.arrangeErr != nil {
		return moved, f.arrangeErr
	}
	return moved, f.err
}

func (f *fakePanes) floated() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.floats)
}

func (f *fakePanes) focus(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.focused = append(f.focused, id)
	return f.err
}

func (f *fakePanes) title(_ context.Context, was, now string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.named == "" || f.named == paneTabLabel || f.named == was {
		f.named = now
		f.titles = append(f.titles, now)
	}
	return f.err
}

func (f *fakePanes) tile(_ context.Context, _ string, argvs [][]string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tiled = append(f.tiled, argvs)
	ids := make([]string, len(argvs))
	for n := range ids {
		ids[n] = fmt.Sprintf("wall-%d-%d", len(f.tiled), n)
	}
	return ids, f.err
}

func (f *fakePanes) grow(_ context.Context, beside string, _ bool, _ []string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.grown = append(f.grown, beside)
	return fmt.Sprintf("grown-%d", len(f.grown)), f.err
}

func (f *fakePanes) walls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.tiled)
}

func (f *fakePanes) open(_ context.Context, argv []string, place panePlace, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened = append(f.opened, argv)
	f.places = append(f.places, place)
	return "pane-1", f.err
}

func (f *fakePanes) close(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, id)
	return nil
}

func (f *fakePanes) opens() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.opened)
}

// hostPanes gives r a pane host that records what it is asked for, with the
// socket in a scratch root short enough for a Unix socket path, which the
// test's own temp directory is not.
func hostPanes(t *testing.T, r *managedREPL) *fakePanes {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "polly-pane-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	t.Setenv(scratch.RootEnv, root)
	fake := &fakePanes{panes: map[string]dockState{}}
	r.panes.opener = fake
	t.Cleanup(r.closePanes)
	return fake
}

// paneTestREPL is a REPL on a 140x40 screen with a pane host and two running
// agents, Builder then Reviewer in list order.
func paneTestREPL(t *testing.T) (*managedREPL, *fakePanes, []string) {
	t.Helper()
	withDisplayTTY(t)
	r, screen := chromeTestREPL(t)
	screen.SetSize(140, 40)
	fake := hostPanes(t, r)
	s := &swarm.State{Members: map[string]*swarm.Member{}, Executions: map[string]*swarm.Execution{}, Tasks: map[string]*swarm.Task{}}
	ids := []string{"member-builder", "member-reviewer"}
	for n, name := range []string{"Builder", "Reviewer"} {
		id := ids[n]
		s.Members[id] = &swarm.Member{ID: id, Name: name, Label: name, Execution: id, Task: id}
		s.Executions[id] = &swarm.Execution{ID: id, Member: id, Status: "running"}
		s.Tasks[id] = &swarm.Task{ID: id, Owner: id, Status: "running", Execution: id}
	}
	r.visibleTab().swarmSnapshot = s
	return r, fake, ids
}

// pumpUI runs what background work hands the event loop until done.
func pumpUI(t *testing.T, r *managedREPL, done func() bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for !done() {
		select {
		case fn := <-r.uiTasks:
			fn()
		case <-deadline:
			t.Fatal("timed out waiting for the event loop's work")
		}
	}
}

// openTestPane opens the agents view, lets the host answer, and connects a
// 60x20 terminal as the proxy would.
func openTestPane(t *testing.T, r *managedREPL, fake *fakePanes) (*satellitePane, *headlessscreen.Screen) {
	t.Helper()
	r.openAttention()
	r.render()
	p := r.panes.pane
	if p == nil {
		t.Fatal("opening the view asked for no pane")
	}
	pumpUI(t, r, func() bool { return p.blockID != "" })
	screen, err := headlessscreen.New(60, 20)
	if err != nil {
		t.Fatal(err)
	}
	r.attachPane(p.token, screen, nil)
	r.render()
	return p, screen
}

func screenText(t *testing.T, screen *headlessscreen.Screen) string {
	t.Helper()
	frame := screenSnapshot(t, screen)
	width, height := frame.Size()
	var b strings.Builder
	for y := range height {
		for x := range width {
			text, _, _ := frame.Get(x, y)
			b.WriteString(text)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func paneKey(r *managedREPL, p *satellitePane, id string) {
	if r.handlePaneEvent(paneEvent{pane: p, ev: ui.Event{Type: ui.KeyboardEvent, ID: id}}) {
		r.render()
	}
}

func TestViewOpensInAPaneAndTakesItsInput(t *testing.T) {
	r, fake, ids := paneTestREPL(t)
	p, pane := openTestPane(t, r, fake)
	if argv := fake.opened[0]; len(argv) != 4 || argv[1] != "pane" || argv[2] != r.panes.socket || argv[3] != p.token {
		t.Fatalf("the host was asked to run %q", argv)
	}
	i := &r.workspace().inspector
	if !r.viewDetached || r.framed() || !i.open || !r.chrome.frame.Empty() || r.chrome.main.Dx() != 140 {
		t.Fatalf("the view is not on the pane alone: detached=%v open=%v chrome=%+v", r.viewDetached, i.open, r.chrome)
	}
	if want := image.Rect(0, 0, 59, 20); r.surface.inner != want || r.surface.track != image.Rect(59, 0, 60, 20) {
		t.Fatalf("surface = %v track %v, want the pane's screen", r.surface.inner, r.surface.track)
	}
	text := screenText(t, pane)
	if !strings.Contains(text, "Builder") || !strings.Contains(text, "Reviewer") {
		t.Fatalf("the pane does not show the agents:\n%s", text)
	}

	// The pane's screen has the view's keys without any focus to take.
	paneKey(r, p, "<Down>")
	if text := screenText(t, pane); !strings.Contains(text, "› Builder") {
		t.Fatalf("Down did not select the first agent:\n%s", text)
	}
	paneKey(r, p, "<Enter>")
	if i.target.kind != conversationViewKind || i.target.session.ID != ids[0] {
		t.Fatalf("Enter opened %+v, want %s in the same pane", i.target, ids[0])
	}
	paneKey(r, p, "<Backspace>")
	if i.target.kind != agentsViewKind || r.panes.pane != p || fake.opens() != 1 {
		t.Fatalf("Backspace did not return to the list in the same pane: %+v", i.target)
	}

	// Clicks land on the surface's own hitboxes, at the pane's coordinates.
	row := headerButton(r.surface.buttons, "agents-open:"+ids[1])
	if row.Empty() || !row.In(r.surface.inner) {
		t.Fatalf("the list's rows are not clickable on the pane: %+v", r.surface.buttons)
	}
	if !r.handlePaneEvent(paneEvent{pane: p, ev: mouseEvent("<MouseLeft>", row.Min)}) || i.target.session.ID != ids[1] {
		t.Fatalf("a click on Reviewer opened %+v", i.target)
	}
	paneKey(r, p, "<Backspace>")

	// Escape closes the view, and the pane goes with it.
	paneKey(r, p, "<Escape>")
	if i.open || r.panes.pane != nil || r.viewDetached {
		t.Fatalf("Escape left open=%v pane=%v detached=%v", i.open, r.panes.pane != nil, r.viewDetached)
	}
	// Events still queued for the closed pane are dropped.
	if r.handlePaneEvent(paneEvent{pane: p, ev: ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"}}) || i.open {
		t.Fatal("a closed pane handled a key")
	}
}

func TestPaneFollowsTheView(t *testing.T) {
	r, fake, _ := paneTestREPL(t)
	p, _ := openTestPane(t, r, fake)
	// The badge closes the view; nothing is left for the pane to show.
	r.openAttention()
	r.render()
	if r.panes.pane != nil || r.viewDetached {
		t.Fatal("the pane outlived its view")
	}

	// A terminal closed from outside closes the view it showed.
	p, _ = openTestPane(t, r, fake)
	if !r.handlePaneEvent(paneEvent{pane: p, lost: true}) {
		t.Fatal("losing the pane must repaint")
	}
	r.render()
	if r.workspace().inspector.open || r.panes.pane != nil || fake.opens() != 2 {
		t.Fatalf("a lost pane left open=%v pane=%v opens=%d", r.workspace().inspector.open, r.panes.pane != nil, fake.opens())
	}
}

func TestViewFallsBackToTheFrameWhenNoPaneOpens(t *testing.T) {
	r, fake, _ := paneTestREPL(t)
	fake.err = errors.New("no rex here")
	r.openAttention()
	r.render()
	// Only the host's floating windows are given up: it may still open tabs.
	pumpUI(t, r, func() bool { return r.panes.floatless })
	r.render()
	if r.viewDetached || !r.framed() || r.chrome.frame.Empty() || !r.workspace().inspector.open || r.panes.tabless || r.panes.wallless {
		t.Fatalf("the view did not return to the frame: detached=%v chrome=%+v tabless=%v wallless=%v", r.viewDetached, r.chrome, r.panes.tabless, r.panes.wallless)
	}
	if text := plainStyledText(strings.Join(transcriptTexts(r.model), "\n")); !strings.Contains(text, "Rex floating window unavailable") || !strings.Contains(text, "no rex here") {
		t.Fatalf("the failure was not reported: %s", text)
	}
	// The frame keeps the view for the rest of the run.
	r.openAttention()
	r.openAttention()
	r.render()
	if fake.opens() != 1 || !r.framed() {
		t.Fatalf("panes were retried after failing: opens=%d framed=%v", fake.opens(), r.framed())
	}
}
