package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/internal/scratch"
	"github.com/alexschlessinger/pollytool/sessions"
)

// serveUI runs r's event-loop work on a goroutine of its own until the test
// ends, for a polly that only has to answer another.
func serveUI(t *testing.T, r *managedREPL) {
	t.Helper()
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			select {
			case fn := <-r.uiTasks:
				fn()
			case <-done:
				return
			}
		}
	}()
	t.Cleanup(func() { close(done); <-stopped })
}

func (f *fakePanes) focuses() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.focused)
}

// peerPollys is two pollys under one pane host on one session database: one
// holding the session "held" and answering questions, and one to ask from.
func peerPollys(t *testing.T) (asker *managedREPL, fake *fakePanes) {
	t.Helper()
	_, asker, fake = peerPollysHolding(t, "held")
	return asker, fake
}

// peerPollysHolding is those two pollys, with the holder's sessions each in
// a tab of its own: the last named is the one on its screen.
func peerPollysHolding(t *testing.T, held ...string) (holder, asker *managedREPL, fake *fakePanes) {
	t.Helper()
	store := testOpenMemoryStore(t, nil)
	holder = newTabTestREPL(t, store, held...)
	asker = newTabTestREPL(t, store, "asking")
	hostPanes(t, holder)
	fake = hostPanes(t, asker)
	// Both run under one host, from one session database.
	holder.panes.block, holder.panes.store = "block-of-holder", "db"
	asker.panes.block, asker.panes.store = "block-of-asker", "db"
	if err := holder.listenPanes(); err != nil {
		t.Fatal(err)
	}
	// Two pollys are two processes, each with a socket named for its own.
	// Here they share one, so the holder's moves aside; it keeps listening.
	moved := filepath.Join(filepath.Dir(holder.panes.socket), "pane-holder.sock")
	if err := os.Rename(holder.panes.socket, moved); err != nil {
		t.Fatal(err)
	}
	holder.panes.socket = moved
	if err := asker.listenPanes(); err != nil {
		t.Fatal(err)
	}
	serveUI(t, holder)
	return holder, asker, fake
}

// shownTab asks r, on its own event loop, which session is on its screen and
// which tab it has been asked to show next, -1 for none.
func shownTab(t *testing.T, r *managedREPL) (name string, request int) {
	t.Helper()
	done := make(chan struct{})
	if !r.postUI(r.work.ctx, func() {
		name, request = r.visibleTab().name, r.showTabRequest
		close(done)
	}) {
		t.Fatal("the polly asked is not running")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the polly asked did not answer")
	}
	return name, request
}

func TestHeldSessionBringsItsPollyForward(t *testing.T) {
	asker, fake := peerPollys(t)
	asker.model.mu.Lock()
	asker.requestOpenLocked("held")
	asker.model.mu.Unlock()
	for deadline := time.Now().Add(5 * time.Second); len(fake.focuses()) == 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the polly holding the session was not brought forward")
		}
	}
	if got := fake.focuses(); !slices.Equal(got, []string{"block-of-holder"}) || fake.opens() != 0 {
		t.Fatalf("focused %q and opened %d tabs, want the holder's terminal alone", got, fake.opens())
	}
	if len(asker.tabs) != 1 || asker.opening != "" {
		t.Fatal("a held session was opened a second time")
	}

	// A session nobody has open gets a tab of its own, as before.
	asker.model.mu.Lock()
	asker.requestOpenLocked("saved")
	asker.model.mu.Unlock()
	pumpUI(t, asker, func() bool { return fake.opens() == 1 })
	if got := fake.focuses(); len(got) != 1 {
		t.Fatalf("an unheld session focused %q", got)
	}

	// A polly on another database has a session of the same name, not this one.
	asker.panes.store = "elsewhere"
	asker.model.mu.Lock()
	asker.requestOpenLocked("held")
	asker.model.mu.Unlock()
	pumpUI(t, asker, func() bool { return fake.opens() == 2 })
	if got := fake.focuses(); len(got) != 1 {
		t.Fatalf("another database's session focused %q", got)
	}
}

func TestOnlyTheHolderAnswers(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "held")
	hostPanes(t, r)
	r.panes.block, r.panes.store = "block", "db"
	id := r.visibleTab().viewID()
	if id == "" {
		t.Fatal("the test session has no identity")
	}
	for _, c := range []struct {
		store, id, name, want string
	}{
		{"db", "", "held", "block"},
		{"db", id, "renamed-since", "block"}, // identity wins over a stale name
		{"db", "", "other", ""},
		{"db", "not-" + id, "held", ""},
		{"elsewhere", id, "held", ""},
	} {
		if got := r.holderBlock(c.store, c.id, c.name); got != c.want {
			t.Fatalf("holderBlock(%q, %q, %q) = %q, want %q", c.store, c.id, c.name, got, c.want)
		}
	}
	// Outside a pane host there is no terminal to bring forward.
	r.panes.block = ""
	if got := r.holderBlock("db", id, "held"); got != "" {
		t.Fatalf("a polly with no terminal answered %q", got)
	}
}

func TestPickingAHeldSessionBringsItsPollyForward(t *testing.T) {
	asker, fake := peerPollys(t)
	asker.model.mu.Lock()
	asker.openSessionsPickerSelected("held")
	picker := asker.model.modal
	asker.model.mu.Unlock()
	if picker == nil || picker.canSubmit("held") {
		t.Fatal("a session another polly holds was offered for opening here")
	}
	pumpUI(t, asker, func() bool { return len(fake.focuses()) == 1 })
	if got := fake.focuses(); got[0] != "block-of-holder" || fake.opens() != 0 || asker.opening != "" {
		t.Fatalf("picking a held session focused %q and opened %d tabs", got, fake.opens())
	}
	// The reader has gone to the other tab; the picker does not wait here.
	if asker.model.modal != nil {
		t.Fatal("the picker stayed open after the jump")
	}

	// With nobody to answer, the row refuses as it always has.
	asker.panes.store = "elsewhere"
	asker.model.mu.Lock()
	asker.openSessionsPickerSelected("held")
	picker = asker.model.modal
	asker.model.mu.Unlock()
	if picker.canSubmit("held") {
		t.Fatal("a held session was offered for opening here")
	}
	pumpUI(t, asker, func() bool { return !asker.sessionsPicker.flashUntil.IsZero() })
	if asker.model.modal != picker || len(fake.focuses()) != 1 {
		t.Fatal("a held session nobody answered for did not just refuse")
	}
}

// Asking who holds a session changes nothing in the polly that answers. Its
// tab for the session comes forward only once the host has brought that
// polly's terminal forward: a host that cannot reach it leaves it as it was.
func TestAskingWhoHoldsASessionLeavesItsPollyAlone(t *testing.T) {
	holder, asker, fake := peerPollysHolding(t, "held", "beside")
	if name, _ := shownTab(t, holder); name != "beside" {
		t.Fatalf("the holder shows %q, want the session beside the one asked about", name)
	}
	target := sessions.ViewTarget{Name: "held"}
	root, err := scratch.EnsureRoot()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// The question alone is answered, and nothing moves.
	match := askPanes(ctx, root, asker.panes.socket, asker.panes.block, asker.panes.store, target)
	if match.block != "block-of-holder" || match.path != holder.panes.socket {
		t.Fatalf("the holder answered %+v", match)
	}
	if name, request := shownTab(t, holder); name != "beside" || request != -1 {
		t.Fatalf("being asked moved the holder to %q (asked to show tab %d)", name, request)
	}

	// A host that cannot bring the holder's terminal forward leaves it alone.
	jump := func() bool {
		t.Helper()
		var jumped, done bool
		if !asker.jumpToHolder(target, func(ok bool) { jumped, done = ok, true }) {
			t.Fatal("there was no host to ask")
		}
		pumpUI(t, asker, func() bool { return done })
		return jumped
	}
	fake.mu.Lock()
	fake.err = errors.New("no such terminal")
	fake.mu.Unlock()
	if jump() {
		t.Fatal("a holder the host could not focus counted as found")
	}
	if name, request := shownTab(t, holder); name != "beside" || request != -1 {
		t.Fatalf("a failed jump moved the holder to %q (asked to show tab %d)", name, request)
	}

	// One that can has the holder show the session's tab.
	fake.mu.Lock()
	fake.err = nil
	fake.mu.Unlock()
	if !jump() {
		t.Fatal("the holder was not found")
	}
	if _, request := shownTab(t, holder); request != 0 {
		t.Fatalf("the holder was asked to show tab %d, want the held session's", request)
	}
}
