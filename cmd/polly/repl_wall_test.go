package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/headlessscreen"
	"github.com/alexschlessinger/pollytool/swarm"
)

// openTestWall asks for the wall, lets the host answer, and connects an
// 80x24 terminal to each pane as the proxies would.
func openTestWall(t *testing.T, r *managedREPL, fake *fakePanes) []*headlessscreen.Screen {
	t.Helper()
	r.runCommand("/wall")
	r.render()
	wall := r.panes.wall
	if wall == nil {
		t.Fatalf("/wall opened no wall:\n%s", plainStyledText(strings.Join(transcriptTexts(r.model), "\n")))
	}
	pumpUI(t, r, func() bool { return wall.panes[len(wall.panes)-1].blockID != "" })
	screens := make([]*headlessscreen.Screen, len(wall.panes))
	for n, p := range wall.panes {
		screens[n] = attachTestScreen(t, r, p)
	}
	r.render()
	return screens
}

func attachTestScreen(t *testing.T, r *managedREPL, p *satellitePane) *headlessscreen.Screen {
	t.Helper()
	screen, err := headlessscreen.New(80, 24)
	if err != nil {
		t.Fatal(err)
	}
	r.attachPane(p.token, screen, nil)
	return screen
}

// finishTestAgent settles an agent's work, leaving nothing for the user.
func finishTestAgent(r *managedREPL, id string) {
	s := r.visibleTab().swarmSnapshot
	s.Executions[id].Status, s.Tasks[id].Status = "completed", "done"
}

// startTestAgent adds a running agent to the workspace's swarm.
func startTestAgent(r *managedREPL, id, name string) {
	s := r.visibleTab().swarmSnapshot
	s.Members[id] = &swarm.Member{ID: id, Name: name, Label: name, Execution: id, Task: id}
	s.Executions[id] = &swarm.Execution{ID: id, Member: id, Status: "running"}
	s.Tasks[id] = &swarm.Task{ID: id, Owner: id, Status: "running", Execution: id}
}

func wallMembers(r *managedREPL) []string {
	var members []string
	if wall := r.panes.wall; wall != nil {
		for _, p := range wall.panes {
			members = append(members, p.member)
		}
	}
	return members
}

func TestWallShowsEachRunningAgentInItsOwnPane(t *testing.T) {
	r, fake, ids := paneTestREPL(t)
	screens := openTestWall(t, r, fake)
	wall := r.panes.wall
	tiled := fake.getTiled()
	if fake.walls() != 1 || len(tiled[0]) != 2 || len(wall.panes) != 2 {
		t.Fatalf("the host tiled %d walls of %v panes", fake.walls(), tiled)
	}
	for n, p := range wall.panes {
		argv := tiled[0][n]
		if len(argv) != 4 || argv[1] != "pane" || argv[2] != r.panes.socket || argv[3] != p.token {
			t.Fatalf("pane %d runs %q", n, argv)
		}
		i := &p.view.inspector
		if !i.open || i.target.kind != conversationViewKind || i.target.session.ID != ids[n] || p.member != ids[n] {
			t.Fatalf("pane %d shows %+v for %q, want %s", n, i.target, p.member, ids[n])
		}
		if p.surface.inner.Dx() != 79 || p.surface.inner.Dy() != 24 {
			t.Fatalf("pane %d's surface is %v, want its own screen", n, p.surface.inner)
		}
	}
	// Each pane names its own agent, and the main screen shows neither view.
	for n, name := range []string{"Builder", "Reviewer"} {
		if text := screenText(t, screens[n]); !strings.Contains(text, name) {
			t.Fatalf("pane %d does not show %s:\n%s", n, name, text)
		}
	}
	if r.workspace().inspector.open || r.viewDetached || r.framed() || r.scoped != nil {
		t.Fatal("the wall opened the workspace's own view")
	}

	// A pane's keys move that pane alone.
	first, second := wall.panes[0], wall.panes[1]
	paneKey(r, first, "<Home>")
	if first.view.viewState(first.view.inspector.target).follow || !second.view.viewState(second.view.inspector.target).follow {
		t.Fatal("scrolling one pane did not stay in that pane")
	}

	// Work a pane's handler leaves for later still acts on that pane's view,
	// and is dropped once the pane has gone.
	var later func(func())
	r.within(second, func() { later = r.viewScope() })
	var seen *sessionWorkspace
	later(func() { seen = r.workspace() })
	if seen != second.view || r.workspace() == second.view {
		t.Fatal("deferred work lost the pane it came from")
	}

	// Escape closes that pane's view, and the pane leaves the wall.
	paneKey(r, first, "<Escape>")
	if got := wallMembers(r); len(got) != 1 || got[0] != ids[1] || r.panes.has(first) {
		t.Fatalf("after Escape the wall holds %q", got)
	}
	// Its terminal closing does the same, and the wall ends with its last pane.
	if !r.handlePaneEvent(paneEvent{pane: second, lost: true}) || r.panes.wall != nil {
		t.Fatal("the wall outlived its last pane")
	}
	later(func() { t.Fatal("deferred work ran for a pane that is gone") })
}

func TestWallFollowsTheAgentsThatRun(t *testing.T) {
	r, fake, ids := paneTestREPL(t)
	openTestWall(t, r, fake)
	wall := r.panes.wall

	// An agent that starts gets a pane split from one already there.
	startTestAgent(r, "member-tester", "Tester")
	r.render()
	if len(wall.panes) != 3 || !wall.growing {
		t.Fatalf("a third agent left the wall at %q", wallMembers(r))
	}
	third := wall.panes[2]
	pumpUI(t, r, func() bool { return third.blockID != "" })
	if grown := fake.getGrown(); len(grown) != 1 || !strings.HasPrefix(grown[0], "wall-1-") || third.member != "member-tester" {
		t.Fatalf("grown beside %q for %q", grown, third.member)
	}
	attachTestScreen(t, r, third)
	r.render()

	// One that finishes leaves its pane showing what it did, until another
	// agent needs the room: then that pane is reused rather than a new one
	// opened.
	finishTestAgent(r, ids[1])
	r.render()
	if got := wallMembers(r); len(got) != 3 || got[1] != ids[1] {
		t.Fatalf("a finished agent's pane was taken away: %q", got)
	}
	startTestAgent(r, "member-writer", "Writer")
	r.render()
	if got, grown := wallMembers(r), fake.getGrown(); len(got) != 3 || got[1] != "member-writer" || len(grown) != 1 {
		t.Fatalf("the wall holds %q after growing %d times, want the finished pane reused", got, len(grown))
	}
	if i := &wall.panes[1].view.inspector; i.target.session.ID != "member-writer" || len(i.history) != 1 {
		t.Fatalf("the reused pane shows %+v with %d views behind it", i.target, len(i.history))
	}

	// A finished pane its reader scrolled is theirs to keep.
	finishTestAgent(r, ids[0])
	paneKey(r, wall.panes[0], "<Home>")
	startTestAgent(r, "member-planner", "Planner")
	r.render()
	if got := wallMembers(r); got[0] != ids[0] {
		t.Fatalf("a pane being read was reused: %q", got)
	}

	// /wall again closes it.
	r.runCommand("/wall")
	r.render()
	if r.panes.wall != nil || r.panes.has(third) {
		t.Fatal("/wall did not close the open wall")
	}
}

func TestWallWithoutAHostShowsTheAgentsHere(t *testing.T) {
	r, fake, _ := paneTestREPL(t)
	r.panes.opener = nil
	r.runCommand("/wall")
	r.render()
	i := &r.workspace().inspector
	if r.panes.wall != nil || !i.open || i.target.kind != agentsViewKind || !r.framed() || fake.walls() != 0 {
		t.Fatalf("without a host: wall=%v open=%v target=%+v framed=%v", r.panes.wall != nil, i.open, i.target, r.framed())
	}
	if text := plainStyledText(strings.Join(transcriptTexts(r.model), "\n")); !strings.Contains(text, "needs Rex") {
		t.Fatalf("the fallback was not explained: %s", text)
	}
}

func TestWallFallsBackWhenTheHostFails(t *testing.T) {
	r, fake, _ := paneTestREPL(t)
	fake.setErr(errors.New("no rex here"))
	r.runCommand("/wall")
	r.render()
	pumpUI(t, r, func() bool { return r.panes.wallless })
	r.render()
	// Because fake fails every call, floating the fallback agents view also
	// degrades to Polly's own screen.
	pumpUI(t, r, func() bool { return r.panes.floatless })
	r.render()
	i := &r.workspace().inspector
	if r.panes.wall != nil || !i.open || i.target.kind != agentsViewKind || !r.framed() {
		t.Fatalf("after a failed wall: wall=%v open=%v framed=%v", r.panes.wall != nil, i.open, r.framed())
	}
	if text := plainStyledText(strings.Join(transcriptTexts(r.model), "\n")); !strings.Contains(text, "Rex tab unavailable") {
		t.Fatalf("the failure was not reported: %s", text)
	}
}

func TestWallNeedsRunningAgents(t *testing.T) {
	r, fake, ids := paneTestREPL(t)
	for _, id := range ids {
		finishTestAgent(r, id)
	}
	r.runCommand("/wall")
	r.render()
	if r.panes.wall != nil || fake.walls() != 0 || r.workspace().inspector.open {
		t.Fatal("a wall opened with nothing running")
	}
	if text := plainStyledText(strings.Join(transcriptTexts(r.model), "\n")); !strings.Contains(text, "No agents are running") {
		t.Fatalf("the user was not told why: %s", text)
	}
}
