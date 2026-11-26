package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/rex"
)

// saveTestSession leaves a closed session called name in store.
func saveTestSession(t *testing.T, r *managedREPL, name string) {
	t.Helper()
	if err := testAcquireSession(t, r.state.sessionStore, name).Close(); err != nil {
		t.Fatal(err)
	}
}

// choiceTitled finds the row a list shows under title.
func choiceTitled(choices []paneChoice, title string) (paneChoice, bool) {
	for _, choice := range choices {
		if choice.title == title {
			return choice, true
		}
	}
	return paneChoice{}, false
}

func notices(r *managedREPL) string {
	return plainStyledText(strings.Join(transcriptTexts(r.model), "\n"))
}

func TestSessionsAreListedByTheHost(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "root", "beside")
	fake := hostPanes(t, r)
	saveTestSession(t, r, "saved")
	fake.choose = func(choices []paneChoice) (string, error) {
		saved, _ := choiceTitled(choices, "saved")
		return saved.id, nil
	}

	r.model.mu.Lock()
	r.openSessionsPicker()
	modal := r.model.modal
	r.model.mu.Unlock()
	if modal != nil {
		t.Fatal("polly's own picker opened beside the host's list")
	}
	pumpUI(t, r, func() bool { return fake.opens() == 1 })

	// The list is every root session but the one on screen.
	list := fake.lists[0]
	if _, listed := choiceTitled(list, r.visibleTab().name); listed || len(list) != 2 {
		t.Fatalf("the host listed %+v, want the two sessions not on screen", list)
	}
	saved, _ := choiceTitled(list, "saved")
	if saved.id == "" || saved.icon != paneIconPast || !strings.Contains(saved.subtitle, "0 msgs") || saved.detail == "" {
		t.Fatalf("a saved session is listed as %+v", saved)
	}
	other := r.tabs[1-r.visibleTabIndex()].name
	if here, ok := choiceTitled(list, other); !ok || here.icon != paneIconLive || !strings.Contains(here.subtitle, "open here") {
		t.Fatalf("a session open in this polly is listed as %+v", here)
	}

	// The choice opens as a pick in the modal does: a tab of its own.
	if x := becomePane(t, fake); !slices.Equal(x.Argv[len(x.Argv)-2:], []string{"--context", "saved"}) {
		t.Fatalf("the chosen session's tab runs %q", x.Argv)
	}
	if !slices.Equal(fake.places, []panePlace{paneTab}) || len(r.tabs) != 2 || r.panes.asked != nil {
		t.Fatalf("the choice was placed at %v with %d tabs here", fake.places, len(r.tabs))
	}

	// A session open here is shown, not launched.
	fake.choose = func(choices []paneChoice) (string, error) {
		here, _ := choiceTitled(choices, other)
		return here.id, nil
	}
	r.model.mu.Lock()
	r.openSessionsPicker()
	r.model.mu.Unlock()
	pumpUI(t, r, func() bool { return r.showTabRequest != 0 })
	if fake.opens() != 1 {
		t.Fatal("a session open here was launched in a tab")
	}
}

func TestClosingTheHostsListChoosesNothing(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "root")
	fake := hostPanes(t, r)
	saveTestSession(t, r, "saved")
	answer := make(chan string)
	fake.choose = func([]paneChoice) (string, error) { return <-answer, nil }

	r.model.mu.Lock()
	r.openSessionsPicker()
	// Asked again while the list is up, polly does not ask twice.
	r.openSessionsPicker()
	r.model.mu.Unlock()
	answer <- ""
	pumpUI(t, r, func() bool { return r.panes.asked == nil })
	if fake.listed() != 1 || fake.opens() != 0 || r.model.modal != nil || r.panes.listless {
		t.Fatalf("a closed list left %d lists, %d tabs, modal %v", fake.listed(), fake.opens(), r.model.modal != nil)
	}

	// With nothing else to pick, nothing is asked.
	alone := newTabTestREPL(t, testOpenMemoryStore(t, nil), "only")
	lonely := hostPanes(t, alone)
	alone.model.mu.Lock()
	alone.openSessionsPicker()
	alone.model.mu.Unlock()
	if lonely.listed() != 0 || alone.model.modal != nil || !strings.Contains(notices(alone), "No other sessions") {
		t.Fatalf("a polly with one session listed %d times:\n%s", lonely.listed(), notices(alone))
	}
}

func TestPollysOwnPickerTakesOverFromAHostThatCannotList(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "root")
	fake := hostPanes(t, r)
	saveTestSession(t, r, "saved")
	open := func() {
		t.Helper()
		r.model.mu.Lock()
		r.closeModal()
		r.openSessionsPicker()
		r.model.mu.Unlock()
	}

	// A host that is only busy is asked again the next time.
	open()
	pumpUI(t, r, func() bool { return r.model.modal != nil })
	if r.model.modal.title != "Sessions" || r.panes.listless || strings.Contains(notices(r), "unavailable") {
		t.Fatalf("a busy host left modal %q, listless %v:\n%s", r.model.modal.title, r.panes.listless, notices(r))
	}
	fake.choose = func([]paneChoice) (string, error) { return "", errors.New("no app can show lists") }
	open()
	pumpUI(t, r, func() bool { return r.model.modal != nil })
	if fake.listed() != 2 || !r.panes.listless || !strings.Contains(notices(r), "Rex could not ask, asking here from now on · no app can show lists") {
		t.Fatalf("a host that cannot list was asked %d times:\n%s", fake.listed(), notices(r))
	}

	// After that the modal opens at once, and the host is left alone.
	open()
	if r.model.modal == nil || fake.listed() != 2 {
		t.Fatalf("a host that cannot list was asked again (%d lists)", fake.listed())
	}
}

func TestChoosingAHeldSessionInTheHostsListBringsItsPollyForward(t *testing.T) {
	asker, fake := peerPollys(t)
	fake.choose = func(choices []paneChoice) (string, error) {
		held, ok := choiceTitled(choices, "held")
		if !ok || held.icon != paneIconLive || !strings.Contains(held.subtitle, "open in another polly") {
			t.Errorf("a session another polly holds is listed as %+v", held)
		}
		return held.id, nil
	}
	asker.model.mu.Lock()
	asker.openSessionsPicker()
	asker.model.mu.Unlock()
	pumpUI(t, asker, func() bool { return len(fake.focuses()) == 1 })
	if got := fake.focuses(); got[0] != "block-of-holder" || fake.opens() != 0 || len(asker.tabs) != 1 {
		t.Fatalf("choosing a held session focused %q and opened %d tabs", got, fake.opens())
	}

	// Held by a polly nobody can find, it is refused rather than opened twice.
	asker.panes.store = "elsewhere"
	asker.model.mu.Lock()
	asker.openSessionsPicker()
	asker.model.mu.Unlock()
	pumpUI(t, asker, func() bool { return strings.Contains(notices(asker), "held is open in another polly") })
	if fake.opens() != 0 || len(fake.focuses()) != 1 {
		t.Fatal("a held session nobody answered for was opened or focused")
	}
}

func TestATabTheHostLeavesBehindIsPointedOut(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "root")
	fake := hostPanes(t, r)
	fake.err = &paneBehindError{cause: errors.New("turn on Remote Control")}

	r.model.mu.Lock()
	r.requestOpenLocked("saved")
	r.model.mu.Unlock()
	pumpUI(t, r, func() bool { return strings.Contains(notices(r), "behind this one") })
	if !strings.Contains(notices(r), "saved opened in a Rex tab behind this one · turn on Remote Control") {
		t.Fatalf("a tab left behind was reported as:\n%s", notices(r))
	}
	// The session did open there: its pane still gets its command, nothing
	// opens here, and the host is asked again next time.
	if x := becomePane(t, fake); !slices.Contains(x.Argv, "saved") {
		t.Fatalf("the tab left behind runs %q", x.Argv)
	}
	if r.panes.tabless || len(r.tabs) != 1 || r.opening != "" {
		t.Fatalf("a tab left behind was treated as a failure: tabless=%v tabs=%d", r.panes.tabless, len(r.tabs))
	}

	// The reason is given once.
	r.model.mu.Lock()
	r.requestNewTabLocked()
	r.model.mu.Unlock()
	pumpUI(t, r, func() bool {
		return strings.Contains(notices(r), "The new session opened in a Rex tab behind this one")
	})
	if strings.Count(notices(r), "turn on Remote Control") != 1 {
		t.Fatalf("the host's reason was repeated:\n%s", notices(r))
	}
}

func TestAHolderTheHostLeavesBehindIsStillFound(t *testing.T) {
	asker, fake := peerPollys(t)
	fake.err = &paneBehindError{cause: errors.New("turn on Remote Control")}
	asker.model.mu.Lock()
	asker.requestOpenLocked("held")
	asker.model.mu.Unlock()
	pumpUI(t, asker, func() bool { return strings.Contains(notices(asker), "another Rex tab") })
	if !strings.Contains(notices(asker), "held is open in another Rex tab · turn on Remote Control") {
		t.Fatalf("a holder left behind was reported as:\n%s", notices(asker))
	}
	if fake.opens() != 0 || len(asker.tabs) != 1 || asker.opening != "" {
		t.Fatal("a session whose holder was found was opened a second time")
	}
}

func TestAWallTheHostLeavesBehindStaysUp(t *testing.T) {
	r, fake, _ := paneTestREPL(t)
	fake.err = &paneBehindError{cause: errors.New("turn on Remote Control")}
	openTestWall(t, r, fake)
	pumpUI(t, r, func() bool { return strings.Contains(notices(r), "behind this one") })
	if wall := r.panes.wall; wall == nil || len(wall.panes) != 2 || r.panes.wallless || r.workspace().inspector.open {
		t.Fatalf("a wall left behind was taken down:\n%s", notices(r))
	}
	if !strings.Contains(notices(r), "The agent wall opened in a Rex tab behind this one · turn on Remote Control") {
		t.Fatalf("a wall left behind was reported as:\n%s", notices(r))
	}
}

func TestTheHostsReasonIsReadWithoutItsWrapping(t *testing.T) {
	refusal := &rex.APIError{Method: "client.perform", Message: `stream response "control-1" failed: Rex.app is not accepting actions. Turn on Remote Control. (status=403 code=refused)`}
	if got, want := rexBehindError(refusal).Error(), "Rex.app is not accepting actions. Turn on Remote Control."; got != want {
		t.Fatalf("the reason reads %q, want %q", got, want)
	}
	if got := (&paneBehindError{cause: errors.New("plain")}).Error(); got != "plain" {
		t.Fatalf("a plain reason reads %q", got)
	}
	if !paneBehind(fmt.Errorf("wrapped: %w", rexBehindError(refusal))) || paneBehind(refusal) {
		t.Fatal("paneBehind does not tell a tab left behind from a failure")
	}
}

// The action the docs tell the user to add is the one polly performs.
func TestTheDocsGiveTheActionRexNeeds(t *testing.T) {
	docs, err := os.ReadFile(filepath.Join("..", "..", "docs", "CLI.md"))
	if err != nil {
		t.Fatal(err)
	}
	var indented strings.Builder
	for line := range strings.Lines(rex.AskActionLua(rexAskAction, rexAskTitle)) {
		indented.WriteString("  " + line)
	}
	if !strings.Contains(string(docs), indented.String()) {
		t.Fatalf("docs/CLI.md does not give this action for init.lua:\n%s", indented.String())
	}
}
