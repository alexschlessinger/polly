package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

func refreshPickerSwarm(t *testing.T, r *managedREPL) {
	t.Helper()
	tab := r.visibleTab()
	tab.swarmRefreshAt = time.Time{}
	r.refreshSwarmActivities()
	for tab.swarmLoading {
		runUITask(t, r)
	}
}

func pickerItem(t *testing.T, m *replModal, id string) replModalItem {
	t.Helper()
	for _, item := range m.items {
		if item.identity == id {
			return item
		}
	}
	t.Fatalf("missing picker identity %s", id)
	return replModalItem{}
}

func TestSessionsPickerTracksRuntimeWithoutChildTabs(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	r := newSwarmTestREPL(t, integrationModel(func(ctx context.Context, _ *llm.CompletionRequest) messages.ChatMessage {
		close(started)
		select {
		case <-finish:
		case <-ctx.Done():
		}
		return spawnTestReply("review complete")
	}), nil)
	parent := r.visibleTab()
	r.runTabCommand("/spawn --read-only inspect portable fixture")
	runUITask(t, r)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("member did not start")
	}
	refreshPickerSwarm(t, r)
	var id string
	for member := range parent.swarmSnapshot.Members {
		id = member
	}
	r.model.mu.Lock()
	r.openSessionsPicker()
	m := r.model.modal
	item := pickerItem(t, m, id)
	if !strings.Contains(item.label, "running") || len(r.tabs) != 1 || !r.pickerExpanded[parent.name] {
		t.Fatalf("live picker: %+v, tabs=%d", item, len(r.tabs))
	}
	if text, _ := r.agentsStatus(); text != "1 agent running" {
		t.Fatalf("status: %s", text)
	}
	// Keep the member selected while its durable lease flag in this listing is
	// still true; only the runtime snapshot is refreshed after completion.
	for i, item := range m.filteredItems() {
		if item.identity == id {
			m.selected = i
		}
	}
	r.model.mu.Unlock()
	close(finish)
	waitSwarmIdle(t, parent.state.swarm)
	refreshPickerSwarm(t, r)
	r.model.mu.Lock()
	m.refresh()
	item = pickerItem(t, m, id)
	if !strings.Contains(item.label, "awaiting review") || strings.Contains(item.label, "running") || pickerSelection(m) != id {
		t.Fatalf("stale picker after completion: %+v", item)
	}
	if text, _ := r.agentsStatus(); text != "" {
		t.Fatalf("settled member counted as active: %s", text)
	}
	r.model.renderPendingMarkdown()
	notice := parent.swarmSnapshot.Members[id].Name + " · awaiting review"
	transcript := plainStyledText(r.model.fullTranscript())
	r.model.mu.Unlock()
	if strings.Count(transcript, notice) != 1 {
		t.Fatalf("completion notice: %q", transcript)
	}
	refreshPickerSwarm(t, r)
	r.model.mu.Lock()
	r.model.renderPendingMarkdown()
	transcript = plainStyledText(r.model.fullTranscript())
	r.model.mu.Unlock()
	if strings.Count(transcript, notice) != 1 || parent.turnDone != nil {
		t.Fatalf("completion duplicated or started parent turn: %q", transcript)
	}
	// Reopening cannot bring back a running label from a former picker cache.
	r.model.mu.Lock()
	r.closeModal()
	r.openSessionsPicker()
	if item := pickerItem(t, r.model.modal, id); !strings.Contains(item.label, "awaiting review") {
		t.Fatalf("reopened picker: %+v", item)
	}
	r.model.mu.Unlock()
}

type pickerListingStore struct {
	sessions.SessionStore
	list func(context.Context) ([]sessions.SessionSummary, error)
}

func (s pickerListingStore) ListSummaries(ctx context.Context) ([]sessions.SessionSummary, error) {
	return s.list(ctx)
}

func TestSessionsPickerRelistRefreshesImmediatelyAndPinsSelection(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	saved := testAcquireSession(t, store, "saved")
	savedID := saved.(sessions.ViewIdentity).ViewID()
	if err := saved.Close(); err != nil {
		t.Fatal(err)
	}
	r := newTabTestREPL(t, store, "root")
	r.model.mu.Lock()
	r.openSessionsPickerSelected("saved")
	m := r.model.modal
	r.model.mu.Unlock()
	entered, release := make(chan struct{}), make(chan struct{})
	r.state.sessionStore = pickerListingStore{SessionStore: store, list: func(ctx context.Context) ([]sessions.SessionSummary, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return store.ListSummaries(ctx)
	}}
	r.model.mu.Lock()
	m.picker.listedAt = time.Time{}
	m.refresh()
	r.model.mu.Unlock()
	<-entered
	renamed := testAcquireSession(t, store, "saved")
	if err := renamed.Rename(context.Background(), "renamed"); err != nil {
		t.Fatal(err)
	}
	if err := renamed.Close(); err != nil {
		t.Fatal(err)
	}
	replacement := testAcquireSession(t, store, "saved")
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
	close(release)
	for m.picker.listing {
		runUITask(t, r)
	}
	// No second paint/refresh is needed to consume the newly merged rows.
	item := pickerItem(t, m, savedID)
	if item.value != "renamed" || pickerSelection(m) != savedID {
		t.Fatalf("relist lost identity: item=%+v selected=%s", item, pickerSelection(m))
	}
	if len(m.items) != 3 {
		t.Fatalf("relist did not update items: %+v", m.items)
	}
}

func TestSessionsPickerStaleSelectionOpensExactIdentity(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	if err := testAcquireSession(t, store, "root").Close(); err != nil {
		t.Fatal(err)
	}
	child, err := store.Acquire(context.Background(), "agent", sessions.AcquireOptions{Parent: "root"})
	if err != nil {
		t.Fatal(err)
	}
	id := child.(sessions.ViewIdentity).ViewID()
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	r := newTabTestREPL(t, store, "root")
	r.openSessionsPickerSelected("agent")
	m := r.model.modal
	renamed := testAcquireSession(t, store, "agent")
	if err := renamed.Rename(context.Background(), "renamed-agent"); err != nil {
		t.Fatal(err)
	}
	if err := renamed.Close(); err != nil {
		t.Fatal(err)
	}
	replacement := testAcquireSession(t, store, "agent")
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
	m.onSubmit("agent")
	r.finishOpen(<-r.openDone)
	target := r.workspace().inspector.target.session
	if target.ID != id || target.Name != "renamed-agent" || len(r.tabs) != 1 {
		t.Fatalf("stale picker redirected: %+v tabs=%d", target, len(r.tabs))
	}
}

func TestSessionsPickerMergeUsesParentIdentityRegardlessOfListingOrder(t *testing.T) {
	p := &sessionsPicker{infos: map[string]sessions.SessionSummary{}}
	root := sessions.SessionSummary{ID: "root-id", Metadata: &sessions.Metadata{Name: "root"}}
	child := sessions.SessionSummary{ID: "child-id", ParentID: root.ID, Metadata: &sessions.Metadata{Name: "child", Parent: "root"}}
	p.merge([]sessions.SessionSummary{child, root}, nil)
	if len(p.rows) != 2 || p.rows[0].id != root.ID || p.rows[1].depth != 1 {
		t.Fatalf("child did not follow parent: %+v", p.rows)
	}
	replacement := sessions.SessionSummary{ID: "replacement", Metadata: &sessions.Metadata{Name: "root"}}
	child.ParentID = "" // Canonical store state after the original parent is deleted.
	p.merge([]sessions.SessionSummary{child, replacement}, nil)
	if p.rows[0].id != child.ID || p.rows[0].depth != 0 {
		t.Fatalf("orphan attached to reused parent handle: %+v", p.rows)
	}
}
