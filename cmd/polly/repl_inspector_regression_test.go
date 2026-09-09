package main

import (
	"context"
	"errors"
	"image"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	ui "github.com/metaspartan/gotui/v5"
)

func settleInspectorWork(r *managedREPL) { r.work.wg.Wait(); drainUITasks(r) }

func TestInspectorLiveThoughtClock(t *testing.T) {
	withDisplayTTY(t)
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root", "child")
	child := r.visibleTab()
	child.model.beginTurn("think")
	child.model.appendThinking("first thought")
	child.model.thinkingSegmentStart = time.Now().Add(-2 * time.Second)
	r.showTab(0)
	r.inspect(tabViewTarget(child))
	v := waitInspector(t, r, 140)
	readElapsed := func() time.Duration {
		t.Helper()
		text := strings.Join(transcriptRowsText(v.view.Rows(v.model, 69)), "\n")
		_, suffix, ok := strings.Cut(text, "thought ")
		if !ok || len(strings.Fields(suffix)) == 0 {
			t.Fatalf("thought clock missing: %q", text)
		}
		elapsed, err := time.ParseDuration(strings.Fields(suffix)[0])
		if err != nil {
			t.Fatalf("invalid thought clock: %q", text)
		}
		return elapsed
	}
	before := readElapsed()
	if before < 2*time.Second {
		t.Fatalf("live snapshot omitted elapsed thinking time: %v", before)
	}
	// Advance only the display clock: content, source revision, and the
	// expensive projection remain unchanged between these frame reads.
	model := v.model
	v.model.thinkingSegmentStart = v.model.thinkingSegmentStart.Add(-5 * time.Second)
	if after := readElapsed(); after < before+5*time.Second || v.model != model {
		t.Fatalf("cached view did not advance its clock: %v -> %v", before, after)
	}
	child.model.pauseThinkingSegment()
	v = waitInspector(t, r, 140)
	paused := readElapsed()
	v.model.thinkingSegmentStart = time.Now().Add(-time.Hour)
	if readElapsed() != paused {
		t.Fatal("paused thought kept counting tool execution time")
	}
	child.model.appendThinking("more thought")
	child.model.thinkingSegmentStart = time.Now().Add(-3 * time.Second)
	v = waitInspector(t, r, 140)
	if resumed := readElapsed(); resumed < paused+3*time.Second {
		t.Fatalf("resumed thinking clock lost banked time: %v -> %v", paused, resumed)
	}
	child.model.completeThinkingTurn(false)
	v = waitInspector(t, r, 140)
	finished := readElapsed()
	v.model.thinkingSegmentStart = time.Now().Add(-time.Hour)
	if readElapsed() != finished {
		t.Fatal("completed thought timer did not freeze")
	}
}

func TestInspectorRegressionReopenDuringLoad(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root")
	r.model.appendThinking("thought")
	r.inspectCommand("thoughts")
	r.refreshInspector(140)
	r.inspectCommand("")
	settleInspectorWork(r)
	r.refreshInspector(140)
	i := &r.workspace().inspector
	if i.current.loading && len(r.uiTasks) == 0 {
		t.Fatal("view remains loading after its only worker result was discarded")
	}
}

func TestInspectorRegressionMissingTargetRetry(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root")
	r.inspect(viewTarget{session: sessions.ViewTarget{ID: "0123456789abcdef0123456789abcdef"}})
	for n := 0; n < 3; n++ {
		r.refreshInspector(140)
		settleInspectorWork(r)
	}
	r.refreshInspector(140)
	if r.workspace().inspector.current.loading {
		t.Error("fourth immediate retry scheduled for unchanged missing session")
	}
	settleInspectorWork(r)
}

type countedInspectorArtifacts struct {
	artifacts.Store
	reads atomic.Int32
}

func (s *countedInspectorArtifacts) Open(ctx context.Context, id string) (io.ReadCloser, error) {
	s.reads.Add(1)
	return s.Store.Open(ctx, id)
}

func TestInspectorRegressionCompletedToolReuse(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root")
	store := &countedInspectorArtifacts{Store: r.state.artifactStore}
	ref, err := store.Put(context.Background(), artifacts.Blob{Kind: artifacts.KindText, MIMEType: "text/plain", Data: []byte(strings.Repeat("result line\n", 3000))})
	if err != nil {
		t.Fatal(err)
	}
	r.model.artifactStore = store
	call := messages.ChatMessageToolCall{ID: "done", Name: "bash"}
	r.model.appendToolCallStart(call)
	r.model.inspections.setResult(call, messages.ChatMessage{Role: messages.MessageRoleTool, Parts: []messages.ContentPart{{Type: "artifact", Artifact: &ref}}})
	r.inspectCommand("tools")
	waitInspector(t, r, 140)
	before := store.reads.Load()
	for n := 0; n < 3; n++ {
		r.model.appendLine("unrelated assistant output")
		waitInspector(t, r, 140)
	}
	if after := store.reads.Load(); after != before {
		t.Fatalf("unchanged completed artifact read %d additional times after unrelated output", after-before)
	}
	model := r.workspace().inspector.current.model
	r.model.appendToolCallStart(messages.ChatMessageToolCall{ID: "next", Name: "read_file"})
	v := waitInspector(t, r, 140)
	index, total, _, _ := inspectorSequencePosition(&r.workspace().inspector)
	if v.model != model || store.reads.Load() != before || index != 1 || total != 2 {
		t.Fatal("navigation update reprojected the completed tool or lost its position")
	}
	if waitInspector(t, r, 240).model == model || store.reads.Load() != before+1 {
		t.Fatal("width change failed to rebuild the selected result")
	}
}

func TestInspectorRegressionEndFollowsInspector(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root")
	r.model.appendThinking(strings.Repeat("thought\n", 100))
	r.inspectCommand("thoughts")
	waitInspector(t, r, 140)
	s := r.workspace().viewState(r.workspace().inspector.target)
	s.follow = false
	s.top = 0
	s.lastRows = 1
	r.chrome.inner = image.Rect(70, 0, 140, 30)
	r.workspace().inspector.focused = true
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<End>"})
	if !s.follow {
		t.Fatal("advertised End to follow leaves inspector scrolled away")
	}
}

func TestInspectorRegressionGrandchildCloseProtection(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root", "other")
	r.showTab(0)
	root := r.visibleTab()
	child := &replTab{name: "child", parent: root, parentName: root.name, model: newReplModel()}
	grandchild := &replTab{name: "grandchild", parent: child, parentName: child.name, model: newReplModel(), turnDone: make(chan error)}
	r.tabs = append(r.tabs, child, grandchild)
	defer func() { grandchild.turnDone = nil }()
	r.model.mu.Lock()
	r.requestCloseTabLocked()
	r.model.mu.Unlock()
	if r.closeTabRequest {
		t.Fatal("root workspace may close while its grandchild is running")
	}
}

func TestInspectorRegressionLaunchRejectsRecreatedParent(t *testing.T) {
	ctx := context.Background()
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "root")
	old := testAcquireSession(t, store, "parent")
	oldID := old.(sessions.ViewIdentity).ViewID()
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "parent"); err != nil {
		t.Fatal(err)
	}
	replacement := testAcquireSession(t, store, "parent")
	defer replacement.Close()
	child, err := store.Acquire(ctx, "replacement-child", sessions.AcquireOptions{Parent: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	md, err := child.GetMetadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	md.SpawnCallID = "launch"
	if err := child.SetMetadata(ctx, md); err != nil {
		t.Fatal(err)
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	r.inspectLaunchedAgent(viewTarget{session: sessions.ViewTarget{ID: oldID, Name: "parent"}}, "launch")
	settleInspectorWork(r)
	if r.workspace().inspector.open {
		t.Fatalf("old parent launch selected unrelated %q", r.workspace().inspector.target.session.Name)
	}
}

type countedInspectorViews struct {
	sessions.SessionStore
	sessions.ViewStore
	histories atomic.Int32
	reads     atomic.Int32
	fail      atomic.Bool
}

func (s *countedInspectorViews) ReadView(ctx context.Context, target sessions.ViewTarget, revision string) (*sessions.SessionView, error) {
	s.reads.Add(1)
	if s.fail.Load() {
		return nil, errors.New("temporary storage failure")
	}
	info, err := s.ViewStore.ReadView(ctx, target, revision)
	if err == nil {
		s.histories.Add(int32(len(info.History)))
	}
	return info, err
}

func TestInspectorTransientReadBackoffAndRecovery(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "root")
	saved := testAcquireSession(t, store, "saved")
	defer saved.Close()
	testAddMessages(t, saved, []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "question"}, {Role: messages.MessageRoleAssistant, Content: "recovered output"}})
	counted := &countedInspectorViews{SessionStore: store, ViewStore: store.(sessions.ViewStore)}
	counted.fail.Store(true)
	r.state.sessionStore = counted
	r.inspect(viewTarget{session: sessions.ViewTarget{Name: "saved"}})
	r.refreshInspector(140)
	settleInspectorWork(r)
	v := r.workspace().inspector.current
	if v.failures != 1 || v.unavailable || !v.retryAt.After(time.Now()) {
		t.Fatal("transient failure lacks retry delay")
	}
	for n := 0; n < 4; n++ {
		r.refreshInspector(140)
	}
	if counted.reads.Load() != 1 || r.needsTick() {
		t.Fatal("retry delay spins or schedules another read")
	}
	v.retryAt = time.Now().Add(-time.Second)
	if !r.needsTick() {
		t.Fatal("expired retry delay did not wake idle inspection")
	}
	r.refreshInspector(140)
	settleInspectorWork(r)
	if v.failures != 2 || time.Until(v.retryAt) < time.Second {
		t.Fatal("repeated failure did not increase backoff")
	}
	counted.fail.Store(false)
	v.retryAt = time.Now().Add(-time.Second)
	v = waitInspector(t, r, 140)
	if v.failures != 0 || v.unavailable || !strings.Contains(inspectorText(v), "recovered output") {
		t.Fatal("transient read did not recover")
	}
}

func TestInspectorSavedItemReuseAndReplacement(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "root")
	saved := testAcquireSession(t, store, "saved")
	defer saved.Close()
	call := messages.ChatMessageToolCall{ID: "one", Name: "bash", Arguments: `{"command":"echo one"}`}
	history := []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "run"}, {Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{call}}, {Role: messages.MessageRoleTool, ToolCallID: call.ID, Content: "original output"}}
	testAddMessages(t, saved, history)
	r.inspect(viewTarget{session: sessions.ViewTarget{Name: "saved"}, kind: toolViewKind, item: "tool:1:one"})
	v := waitInspector(t, r, 140)
	first := v.model
	testAddMessages(t, saved, []messages.ChatMessage{{Role: messages.MessageRoleAssistant, Content: "unrelated answer"}})
	r.inspectorRefreshAt = time.Time{}
	if waitInspector(t, r, 140).model != first {
		t.Fatal("unrelated saved output rebuilt the tool")
	}
	if err := saved.Clear(context.Background()); err != nil {
		t.Fatal(err)
	}
	history[2].Content = "replacement output"
	testAddMessages(t, saved, history)
	r.inspectorRefreshAt = time.Time{}
	v = waitInspector(t, r, 140)
	if v.model == first || !strings.Contains(inspectorText(v), "replacement output") {
		t.Fatal("saved item replacement reused stale content")
	}
	// A live catalogue replacement must also invalidate identical per-item counters.
	r.model.hydrateInspections(history)
	r.inspectCommand("tools")
	v = waitInspector(t, r, 140)
	first = v.model
	history[2].Content = "live replacement"
	r.model.hydrateInspections(history)
	v = waitInspector(t, r, 140)
	if v.model == first || !strings.Contains(inspectorText(v), "live replacement") {
		t.Fatal("live catalogue replacement reused stale content")
	}
}

func TestInspectorRegressionAgentPickerReadsUnrelatedHistory(t *testing.T) {
	ctx := context.Background()
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "root")
	other := testAcquireSession(t, store, "other")
	defer other.Close()
	child, err := store.Acquire(ctx, "unrelated-child", sessions.AcquireOptions{Parent: "other"})
	if err != nil {
		t.Fatal(err)
	}
	testAddMessages(t, child, []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "unrelated"}, {Role: messages.MessageRoleAssistant, Content: strings.Repeat("large answer", 1000)}})
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	counted := &countedInspectorViews{SessionStore: store, ViewStore: store.(sessions.ViewStore)}
	r.state.sessionStore = counted
	r.openSessionsPicker()
	settleInspectorWork(r)
	if count := counted.histories.Load(); count > 0 {
		t.Fatalf("empty workspace agent picker deserialized %d unrelated history messages", count)
	}
}

func TestReopeningANameKeyedAgentStartsFromItsResolvedIdentity(t *testing.T) {
	ctx := context.Background()
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "parent")
	child, err := store.Acquire(ctx, "child", sessions.AcquireOptions{Parent: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	testAddMessages(t, child, []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "task"}, {Role: messages.MessageRoleAssistant, Content: "child output"}})
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	counted := &countedInspectorViews{SessionStore: store, ViewStore: store.(sessions.ViewStore)}
	r.state.sessionStore = counted
	link := viewTarget{session: sessions.ViewTarget{Name: "child", Parent: "parent"}}
	r.inspect(link)
	v := waitInspector(t, r, 140)
	w := r.workspace()
	id := w.inspector.target.session.ID
	if id == "" || !strings.Contains(inspectorText(v), "child output") {
		t.Fatalf("first open: id=%q text=%q", id, inspectorText(v))
	}
	r.inspect(link)
	if len(w.inspector.history) != 1 || w.inspector.current != v {
		t.Fatalf("a repeat click through the link reopened the view: history=%d", len(w.inspector.history))
	}
	histories := counted.histories.Load()
	r.closeInspector()
	r.inspect(link)
	if w.inspector.target.session.ID != id {
		t.Fatalf("reopen target = %+v, want the resolved identity %q", w.inspector.target.session, id)
	}
	v = waitInspector(t, r, 140)
	if counted.histories.Load() != histories {
		t.Fatal("reopening through the link read the session history again")
	}
	if !strings.Contains(inspectorText(v), "child output") {
		t.Fatalf("reopened view = %q", inspectorText(v))
	}
}
