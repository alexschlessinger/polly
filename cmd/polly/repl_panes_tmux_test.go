package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// fakeTmux plays a tmux server: it records the command lines it is given and
// answers the ones that ask about a pane from panes, which holds what tmux
// would print for each. A command that creates a pane prints the next ID.
type fakeTmux struct {
	calls  [][]string
	panes  map[string]string
	window string
	next   int
	fail   map[string]error
}

func (f *fakeTmux) run(_ context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, args)
	if err := f.fail[args[0]]; err != nil {
		return "", err
	}
	switch args[0] {
	case "display-message":
		pane, format := args[3], args[4]
		if strings.Contains(format, "window_id") {
			return f.window, nil
		}
		if format == "#{session_id}" {
			return "$3", nil
		}
		place, ok := f.panes[pane]
		if !ok {
			return "", errors.New("can't find pane: " + pane)
		}
		return place, nil
	case "new-pane", "new-window", "split-window":
		f.next++
		return fmt.Sprintf("%%%d", f.next), nil
	}
	return "", nil
}

// ran reports the recorded command lines that start with name.
func (f *fakeTmux) ran(name string) [][]string {
	var lines [][]string
	for _, call := range f.calls {
		if call[0] == name {
			lines = append(lines, call)
		}
	}
	return lines
}

func testTmux() (*tmuxPanes, *fakeTmux) {
	fake := &fakeTmux{next: 10, panes: map[string]string{"%0": "0 0 0 200 50 200 50"}, fail: map[string]error{}}
	host := newTmuxPanes("%0", "/tmp/tmux-501/default,4242,0")
	host.run = fake.run
	return host, fake
}

func TestTmuxFloatsAPaneOverPollysOwn(t *testing.T) {
	host, fake := testTmux()
	// Polly has the right half of a window 200 cells wide.
	fake.panes["%0"] = "0 100 0 100 50 200 50"
	fake.panes["%11"] = "1 151 6 48 8 200 50"
	id, err := host.float(context.Background(), []string{"/bin/polly", "pane", "sock", "token"}, "polly todo", [4]float64{0.5, 0.1, 0.5, 0.2})
	if err != nil || id != "%11" {
		t.Fatalf("float = %q, %v", id, err)
	}
	// The spot is taken over polly's own pane, in the window's cells, and the
	// keys go back to the pane that had them.
	want := []string{"new-pane", "-A", "-t", "%0", "-T", "polly todo", "-x", "50", "-y", "10", "-X", "150", "-Y", "5", "-P", "-F", "#{pane_id}", "--", "/bin/polly", "pane", "sock", "token", ";", "last-pane", "-t", "%0"}
	if got := fake.ran("new-pane"); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Fatalf("tmux was asked for %v\nwant %v", got, want)
	}

	// A tmux that cannot float says so, and polly's own screen takes over.
	fake.fail["new-pane"] = errors.New("unknown command: new-pane")
	if _, err := host.float(context.Background(), []string{"/bin/polly"}, "polly todo", [4]float64{0.5, 0.1, 0.5, 0.2}); err == nil {
		t.Fatal("a tmux without floating panes was not reported")
	}
	fake.fail["list-commands"] = errors.New("unknown command: new-pane")
	if host.floats(context.Background()) {
		t.Fatal("a tmux without floating panes was taken for one with them")
	}
}

func TestTmuxMovesOnlyThePanesPollyPlaced(t *testing.T) {
	host, fake := testTmux()
	ctx := context.Background()
	fake.panes["%11"] = "1 141 3 58 8 200 50"
	id, _ := host.float(ctx, []string{"/bin/polly"}, "polly todo", [4]float64{0.7, 0.04, 0.3, 0.2})
	move := []paneMove{{id: id, to: [4]float64{0.5, 0.5, 0.25, 0.2}}}
	moves := func() int { return len(fake.ran("move-pane")) }

	// Where polly left it, the pane moves and takes its new size.
	moved, err := host.arrange(ctx, move)
	if err != nil || !moved[0] || moves() != 1 {
		t.Fatalf("arrange = %+v, %v after %d moves", moved, err, moves())
	}
	if got := fake.ran("resize-pane"); len(got) != 1 || !reflect.DeepEqual(got[0], []string{"resize-pane", "-t", "%11", "-x", "50", "-y", "10"}) {
		t.Fatalf("the pane was resized with %v", got)
	}

	// The user moved it: it is theirs, and stays.
	fake.panes["%11"] = "1 20 20 58 8 200 50"
	if moved, _ := host.arrange(ctx, move); moved[0] || moves() != 1 {
		t.Fatalf("a pane the user moved was moved back: %+v", moved)
	}

	// tmux moved it, to keep it inside a window that shrank: still polly's.
	fake.panes["%0"], fake.panes["%11"] = "0 0 0 150 40 150 40", "1 91 3 58 8 150 40"
	if moved, _ := host.arrange(ctx, move); !moved[0] || moves() != 2 {
		t.Fatalf("a pane tmux moved for a smaller window was left there: %+v", moved)
	}

	// Docked into the window's layout, it is the user's again.
	fake.panes["%11"] = "0 100 0 50 40 150 40"
	if moved, _ := host.arrange(ctx, move); moved[0] || moves() != 2 {
		t.Fatalf("a docked pane was moved: %+v", moved)
	}
	if docked, _ := host.docked(ctx, []string{"%11", "%0", "%99"}); !reflect.DeepEqual(docked, []dockState{dockDocked, dockDocked, dockUnknown}) {
		t.Fatalf("docked = %v", docked)
	}
}

func TestTmuxDocksPanesAndOpensWindows(t *testing.T) {
	host, fake := testTmux()
	ctx := context.Background()
	argv := []string{"/bin/polly", "pane", "sock", "token"}
	if id, err := host.dock(ctx, argv, "polly view", "", 0.4); err != nil || id != "%11" {
		t.Fatalf("dock = %q, %v", id, err)
	}
	if id, err := host.dock(ctx, argv, "polly todo", "%11", 0.3); err != nil || id != "%12" {
		t.Fatalf("dock under = %q, %v", id, err)
	}
	splits := fake.ran("split-window")
	beside := append([]string{"split-window", "-d", "-h", "-t", "%0", "-l", "40%", "-P", "-F", "#{pane_id}", "--"}, argv...)
	under := append([]string{"split-window", "-d", "-v", "-t", "%11", "-P", "-F", "#{pane_id}", "--"}, argv...)
	if len(splits) != 2 || !reflect.DeepEqual(splits[0], beside) || !reflect.DeepEqual(splits[1], under) {
		t.Fatalf("tmux split with %v", splits)
	}

	// Another session is a window of polly's own tmux session.
	if id, err := host.open(ctx, argv, paneTab, "notes"); err != nil || id != "%13" {
		t.Fatalf("open = %q, %v", id, err)
	}
	if got := fake.ran("new-window"); len(got) != 1 || !reflect.DeepEqual(got[0][:5], []string{"new-window", "-t", "$3:", "-n", "notes"}) {
		t.Fatalf("tmux opened a window with %v", got)
	}

	// The wall is one window, split once per agent and evened out.
	ids, err := host.tile(ctx, "polly agents", [][]string{argv, argv, argv})
	if err != nil || !reflect.DeepEqual(ids, []string{"%14", "%15", "%16"}) || len(fake.ran("select-layout")) != 3 {
		t.Fatalf("tile = %v, %v", ids, err)
	}
}

func TestTmuxFocusesPanesOfItsOwnServerOnly(t *testing.T) {
	host, fake := testTmux()
	ctx := context.Background()
	if got := host.block(); got != "tmux:/tmp/tmux-501/default:%0" {
		t.Fatalf("polly names its pane %q", got)
	}
	if err := host.focus(ctx, "tmux:/tmp/tmux-501/default:%7"); err != nil || !reflect.DeepEqual(fake.ran("select-window"), [][]string{{"select-window", "-t", "%7"}}) || len(fake.ran("select-pane")) != 1 {
		t.Fatalf("focus = %v after %v", err, fake.calls)
	}
	for _, other := range []string{"tmux:/tmp/tmux-501/work:%7", "block:01a2", ""} {
		if err := host.focus(ctx, other); err == nil {
			t.Fatalf("focus of %q, a terminal of another host, did not fail", other)
		}
	}
	if len(fake.ran("select-window")) != 1 {
		t.Fatalf("tmux was asked to select a pane that is not its own: %v", fake.calls)
	}
}

func TestTmuxNamesOnlyAWindowNobodyNamed(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		about, window, was, now string
		want                    []string
	}{
		{"tmux's own name gives way", "@1\t1\tzsh", "", "notes", []string{"rename-window", "-t", "@1", "--", "notes"}},
		{"polly's last name gives way", "@1\t0\tnotes", "notes", "plans", []string{"rename-window", "-t", "@1", "--", "plans"}},
		{"polly's placeholder gives way", "@1\t0\tpolly", "", "notes", []string{"rename-window", "-t", "@1", "--", "notes"}},
		{"the user's name stays", "@1\t0\tmine", "notes", "plans", nil},
		{"the same name is not set twice", "@1\t0\tnotes", "notes", "notes", nil},
		{"polly's name is given back", "@1\t0\tnotes", "notes", "", []string{"set-option", "-w", "-u", "-t", "@1", "automatic-rename"}},
		{"a name it never gave is not given back", "@1\t0\tmine", "notes", "", nil},
	} {
		host, fake := testTmux()
		fake.window = tc.window
		if err := host.title(ctx, tc.was, tc.now); err != nil {
			t.Fatalf("%s: %v", tc.about, err)
		}
		var got []string
		if calls := append(fake.ran("rename-window"), fake.ran("set-option")...); len(calls) > 0 {
			got = calls[0]
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s: tmux was told %v, want %v", tc.about, got, tc.want)
		}
	}
}

func TestAHostWithoutFloatingWindowsStillDocks(t *testing.T) {
	r, fake, _ := paneTestREPL(t)
	t.Cleanup(func() { _ = saveDockedPanels(nil) })
	// As in a tmux too old to float panes.
	r.panes.floatless = true
	plan := progressFixture()
	r.model.setPlanProgress(plan, plan.Todos)

	// A panel that floats does so on polly's own screen, with no word of it.
	r.model.mu.Lock()
	r.openTodoPanel()
	r.model.mu.Unlock()
	if p := r.panes.floatOf(floatChecklist); p == nil || !p.own || fake.floated() != 0 || strings.Contains(notices(r), "unavailable") {
		t.Fatalf("the checklist did not float on polly's own screen: %v\n%s", p, notices(r))
	}
	r.model.mu.Lock()
	r.openTodoPanel()
	r.model.mu.Unlock()

	// One the user keeps docked is still a pane of the host's.
	r.panes.docks = map[floatKind]bool{floatChecklist: true}
	r.model.mu.Lock()
	r.openTodoPanel()
	r.model.mu.Unlock()
	pumpUI(t, r, func() bool { return len(fake.docksOpened()) == 1 })
	if p := r.panes.floatOf(floatChecklist); p == nil || p.own || !p.docked {
		t.Fatalf("the docked checklist is not a pane of the host's: %+v", p)
	}
}
