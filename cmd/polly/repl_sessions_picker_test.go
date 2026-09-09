package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/swarm"
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
	md, err := saved.GetMetadata(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	md.Description = "needle"
	if err := saved.SetMetadata(context.Background(), md); err != nil {
		t.Fatal(err)
	}
	if err := saved.Close(); err != nil {
		t.Fatal(err)
	}
	r := newTabTestREPL(t, store, "root")
	r.model.mu.Lock()
	r.openSessionsPickerSelected("saved")
	m := r.model.modal
	m.input.setText("needle")
	m.selected = 0
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

func TestSessionsPickerAndInspectorRouteQueuedRuntimeApprovalByIdentity(t *testing.T) {
	r := newSwarmTestREPL(t, integrationModel(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		for _, msg := range req.Messages {
			if msg.Role == messages.MessageRoleTool {
				return spawnTestReply("checked")
			}
		}
		return spawnTestToolCall("list_agents", `{}`)
	}), nil)
	r.config.Confirm = true
	defer r.releaseApprovals()
	for range 2 {
		r.runTabCommand("/spawn --read-only inspect")
		runUITask(t, r)
	}
	deadline := time.Now().Add(5 * time.Second)
	var first, queued *approvalState
	for time.Now().Before(deadline) {
		r.model.mu.Lock()
		if r.model.approval != nil && len(r.model.approvalQueue) == 1 {
			first, queued = r.model.approval, r.model.approvalQueue[0]
		}
		r.model.mu.Unlock()
		if queued != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if queued == nil {
		t.Fatal("two members did not reach approvals")
	}
	refreshPickerSwarm(t, r)
	r.model.mu.Lock()
	r.openSessionsPickerSelected(queued.requester)
	m := r.model.modal
	if pickerSelection(m) != queued.requester {
		t.Fatal("picker did not target queued member by stable ID")
	}
	if item := pickerItem(t, m, queued.requester); !strings.Contains(item.label, "approval needed") {
		t.Fatalf("queued member: %+v", item)
	}
	if text, _ := r.agentsStatus(); text != "2 need approval" {
		t.Fatalf("approval count: %s", text)
	}
	r.closeModal()
	r.model.mu.Unlock()
	info, err := r.state.sessionStore.(sessions.ViewStore).ReadView(context.Background(), sessions.ViewTarget{ID: queued.requester}, "")
	if err != nil {
		t.Fatal(err)
	}
	target := viewTarget{session: sessions.ViewTarget{ID: info.ID, Name: info.Metadata.Name}}
	r.inspect(target)
	waitInspector(t, r, 160)
	header := r.inspectorHeader(100, 20, 0, 0)
	if !strings.Contains(plainStyledText(header.text), "approval needed") || headerButton(header.buttons, "review").Empty() {
		t.Fatalf("runtime approval header: %s", header.text)
	}
	r.openAgentApproval(target)
	dialog := r.model.modal
	if dialog == nil || !strings.Contains(dialog.title, info.Metadata.Name) {
		t.Fatal("inspector did not open member's parent-owned approval")
	}
	dialog.onSubmit("y")
	r.applyTabRequests()
	r.model.mu.Lock()
	if r.model.approval != first || r.model.hasApprovalRequest(queued) {
		t.Fatal("answer consumed another member's approval")
	}
	r.model.answerApprovalRequest(first, 'n')
	r.model.mu.Unlock()
	waitSwarmIdle(t, r.state.swarm)
	refreshPickerSwarm(t, r)
	header = r.inspectorHeader(100, 20, 0, 0)
	if !strings.Contains(plainStyledText(header.text), "awaiting review") || !headerButton(header.buttons, "stop").Empty() {
		t.Fatalf("completed member header: %s", header.text)
	}
	if len(r.tabs) != 1 {
		t.Fatal("inspection acquired a member tab")
	}
	// A delayed answer cannot resolve a later request after the captured one ended.
	dialog.onSubmit("y")
	r.applyTabRequests()
}

func TestHistoricalSwarmCompletionDoesNotAnnounceOnFirstPoll(t *testing.T) {
	r := newSwarmTestREPL(t, integrationModel(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return spawnTestReply("done") }), nil)
	_, err := r.state.swarm.Agent(context.Background(), "", swarm.AgentRequest{Task: "old review", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	r.model.mu.Lock()
	before := r.model.fullTranscript()
	r.model.mu.Unlock()
	refreshPickerSwarm(t, r)
	r.model.mu.Lock()
	after := r.model.fullTranscript()
	r.model.mu.Unlock()
	if after != before {
		t.Fatalf("historical completion announced: %q", after)
	}
}
