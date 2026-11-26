package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// fakeZellij plays a Zellij session: it records the command lines it is
// given, lists panes, and names each pane or tab it is asked to make.
type fakeZellij struct {
	calls [][]string
	panes []zellijPane
	next  int
	fail  map[string]error
}

func (f *fakeZellij) run(_ context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, args)
	action := args[1]
	if err := f.fail[action]; err != nil {
		return "", err
	}
	switch action {
	case "list-panes":
		out, _ := json.Marshal(f.panes)
		return string(out), nil
	case "new-pane":
		f.next++
		return fmt.Sprintf("terminal_%d", f.next), nil
	case "new-tab":
		// A tab of its own, numbered after the pane it opens with.
		f.next++
		f.panes = append(f.panes, zellijPane{ID: f.next, TabID: 100 + f.next, TabName: "new"})
		return fmt.Sprint(100 + f.next), nil
	}
	return "", nil
}

// ran reports the recorded command lines for one action, without "action".
func (f *fakeZellij) ran(action string) [][]string {
	var lines [][]string
	for _, call := range f.calls {
		if call[1] == action {
			lines = append(lines, call[1:])
		}
	}
	return lines
}

func testZellij() (*zellijPanes, *fakeZellij) {
	fake := &fakeZellij{next: 10, fail: map[string]error{}, panes: []zellijPane{
		{ID: 0, Plugin: true, Columns: 200, Rows: 1},
		{ID: 3, X: 100, Y: 1, Columns: 100, Rows: 50, TabID: 2, TabName: "Tab #1"},
	}}
	host := newZellijPanes("3", "work")
	host.run = fake.run
	return host, fake
}

func TestZellijFloatsAPinnedPaneOverPollysOwn(t *testing.T) {
	host, fake := testZellij()
	fake.panes = append(fake.panes, zellijPane{ID: 11, Floating: true, X: 150, Y: 6, Columns: 50, Rows: 10, TabID: 2})
	argv := []string{"/bin/polly", "pane", "sock", "token"}
	id, err := host.float(context.Background(), argv, "polly todo", [4]float64{0.5, 0.1, 0.5, 0.2})
	if err != nil || id != "terminal_11" {
		t.Fatalf("float = %q, %v", id, err)
	}
	// Over polly's own pane, in the tab's cells, pinned and without the keys.
	want := append([]string{"new-pane", "--close-on-exit", "--no-focus", "--floating", "--pinned", "true", "--name=polly todo", "--x", "150", "--y", "6", "--width", "50", "--height", "10", "--"}, argv...)
	if got := fake.ran("new-pane"); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Fatalf("zellij was asked for %v\nwant %v", got, want)
	}
	if host.anchor != "terminal_3" || host.block() != "zellij:work:terminal_3" {
		t.Fatalf("polly names its pane %q, and %q to others", host.anchor, host.block())
	}

	// A Zellij that cannot list its panes cannot be asked for anything.
	fake.fail["list-panes"] = errors.New("unrecognized subcommand 'list-panes'")
	if _, _, err := host.home(context.Background()); err == nil {
		t.Fatal("a Zellij without a list of panes was taken for one polly can use")
	}
}

func TestZellijMovesOnlyThePanesPollyPlaced(t *testing.T) {
	host, fake := testZellij()
	ctx := context.Background()
	fake.panes = append(fake.panes, zellijPane{ID: 11, Floating: true, X: 150, Y: 6, Columns: 50, Rows: 10, TabID: 2})
	float := &fake.panes[2]
	id, _ := host.float(ctx, []string{"/bin/polly"}, "polly todo", [4]float64{0.5, 0.1, 0.5, 0.2})
	move := []paneMove{{id: id, to: [4]float64{0, 0.5, 0.5, 0.2}}}
	moves := func() int { return len(fake.ran("change-floating-pane-coordinates")) }

	// Where polly left it, the pane moves and takes its new size.
	if moved, err := host.arrange(ctx, move); err != nil || !moved[0] || moves() != 1 {
		t.Fatalf("arrange = %+v, %v after %d moves", moved, err, moves())
	}
	want := []string{"change-floating-pane-coordinates", "--pane-id", "terminal_11", "--x", "100", "--y", "26", "--width", "50", "--height", "10"}
	if got := fake.ran("change-floating-pane-coordinates")[0]; !reflect.DeepEqual(got, want) {
		t.Fatalf("the pane was moved with %v\nwant %v", got, want)
	}

	// The user moved it: it is theirs, and stays.
	float.X, float.Y = 20, 20
	if moved, _ := host.arrange(ctx, move); moved[0] || moves() != 1 {
		t.Fatalf("a pane the user moved was moved back: %+v", moved)
	}

	// Polly's own pane changed: the tab was laid out again, and the pane is
	// still polly's to place.
	fake.panes[1].Columns = 80
	if moved, _ := host.arrange(ctx, move); !moved[0] || moves() != 2 {
		t.Fatalf("a pane moved by a new layout was left there: %+v", moved)
	}

	// Embedded in the tab's layout, it is the user's again.
	float.Floating = false
	if moved, _ := host.arrange(ctx, move); moved[0] || moves() != 2 {
		t.Fatalf("an embedded pane was moved: %+v", moved)
	}
	if docked, _ := host.docked(ctx, []string{"terminal_11", "terminal_3", "terminal_99"}); !reflect.DeepEqual(docked, []dockState{dockDocked, dockDocked, dockUnknown}) {
		t.Fatalf("docked = %v", docked)
	}
}

func TestZellijDocksPanesAndOpensTabs(t *testing.T) {
	host, fake := testZellij()
	ctx := context.Background()
	argv := []string{"/bin/polly", "pane", "sock", "token"}
	if id, err := host.dock(ctx, argv, "polly view", "", 0.4); err != nil || id != "terminal_11" {
		t.Fatalf("dock = %q, %v", id, err)
	}
	want := append([]string{"new-pane", "--close-on-exit", "--no-focus", "--direction", "right", "--name=polly view", "--"}, argv...)
	if got := fake.ran("new-pane"); !reflect.DeepEqual(got[0], want) {
		t.Fatalf("zellij split with %v\nwant %v", got[0], want)
	}

	// Another session is a tab, in front, that closes with its polly.
	if id, err := host.open(ctx, argv, paneTab, "notes"); err != nil || id != "terminal_12" {
		t.Fatalf("open = %q, %v", id, err)
	}
	if got := fake.ran("new-tab"); len(got) != 1 || !reflect.DeepEqual(got[0], append([]string{"new-tab", "--name=notes", "--close-on-exit", "--"}, argv...)) {
		t.Fatalf("zellij opened a tab with %v", got)
	}

	// The wall is one tab, with a pane added per agent.
	ids, err := host.tile(ctx, "polly agents", [][]string{argv, argv, argv})
	if err != nil || len(ids) != 3 || ids[1] != "terminal_14" || ids[2] != "terminal_15" {
		t.Fatalf("tile = %v, %v", ids, err)
	}
	if added := fake.ran("new-pane"); !reflect.DeepEqual(added[len(added)-1][:5], []string{"new-pane", "--close-on-exit", "--no-focus", "--tab-id", "113"}) {
		t.Fatalf("the wall's panes were added with %v", added[len(added)-1])
	}
}

func TestZellijFocusesPanesOfItsOwnSessionOnly(t *testing.T) {
	host, fake := testZellij()
	ctx := context.Background()
	if err := host.focus(ctx, "zellij:work:terminal_3"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fake.ran("go-to-tab-by-id"), [][]string{{"go-to-tab-by-id", "2"}}) || !reflect.DeepEqual(fake.ran("focus-pane-id"), [][]string{{"focus-pane-id", "terminal_3"}}) {
		t.Fatalf("zellij was told %v", fake.calls)
	}
	for _, other := range []string{"zellij:play:terminal_3", "tmux:/tmp/default:%3", "block:01a2", "terminal_99"} {
		if err := host.focus(ctx, other); err == nil {
			t.Fatalf("focus of %q did not fail", other)
		}
	}
	if len(fake.ran("focus-pane-id")) != 1 {
		t.Fatalf("zellij was asked to focus a pane that is not its own: %v", fake.calls)
	}
}

func TestZellijNamesOnlyATabNobodyNamed(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		about, name, was, now string
		want                  []string
	}{
		{"Zellij's own name gives way", "Tab #4", "", "notes", []string{"rename-tab", "--tab-id", "2", "--", "notes"}},
		{"hyphen-prefixed title is escaped", "Tab #4", "", "--bad-flag", []string{"rename-tab", "--tab-id", "2", "--", "--bad-flag"}},
		{"polly's last name gives way", "notes", "notes", "plans", []string{"rename-tab", "--tab-id", "2", "--", "plans"}},
		{"polly's placeholder gives way", "polly", "", "notes", []string{"rename-tab", "--tab-id", "2", "--", "notes"}},
		{"the user's name stays", "mine", "notes", "plans", nil},
		{"the same name is not set twice", "notes", "notes", "notes", nil},
		{"polly's name is given back", "notes", "notes", "", []string{"undo-rename-tab", "--tab-id", "2"}},
		{"a name it never gave is not given back", "mine", "notes", "", nil},
	} {
		host, fake := testZellij()
		fake.panes[1].TabName = tc.name
		if err := host.title(ctx, tc.was, tc.now); err != nil {
			t.Fatalf("%s: %v", tc.about, err)
		}
		var got []string
		if calls := append(fake.ran("rename-tab"), fake.ran("undo-rename-tab")...); len(calls) > 0 {
			got = calls[0]
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s: zellij was told %v, want %v", tc.about, strings.Join(got, " "), tc.want)
		}
	}
}

func TestZellijArrangeAndDockedPreserveUnrelatedPlacedPanes(t *testing.T) {
	host, _ := testZellij()
	ctx := context.Background()
	// Simulate a pane placed concurrently by float() that is not in this
	// arrange/docked snapshot yet.
	host.mu.Lock()
	host.placed["terminal_99"] = zellijPlaced{at: [4]float64{10, 10, 40, 10}, home: [4]float64{100, 1, 100, 50}}
	host.mu.Unlock()

	if _, err := host.arrange(ctx, []paneMove{{id: "terminal_11", to: [4]float64{0, 0, 0.5, 0.5}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := host.docked(ctx, []string{"terminal_11"}); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	_, kept := host.placed["terminal_99"]
	host.mu.Unlock()
	if !kept {
		t.Fatal("arrange/docked wiped an unrelated concurrently placed pane")
	}
}
