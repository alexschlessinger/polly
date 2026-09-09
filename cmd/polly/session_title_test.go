package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/subagent"
	"github.com/alexschlessinger/pollytool/tools"
	ui "github.com/metaspartan/gotui/v5"
)

func TestSessionTitleToolChildIsolation(t *testing.T) {
	ctx := context.Background()
	model := &scriptedStreamLLM{responses: []messages.ChatMessage{
		spawnTestToolCall(sessionTitleToolName, `{"title":"Revised child objective"}`), spawnTestReply("done"),
	}}
	hits := 0
	parent, store := newSpawnTestParent(t, model, &hits)
	if _, err := parent.session.(sessions.TitleSession).SetTitle(ctx, "Parent title", sessions.TitleSourceUser); err != nil {
		t.Fatal(err)
	}
	res, err := spawnRunner(&Config{}, model, parent)(ctx, subagent.Request{Task: "Investigate", Label: "Assigned brief", Tools: []string{"read_transcript"}})
	if err != nil {
		t.Fatal(err)
	}
	md, history := readChildSession(t, store, res.Session)
	if md.Title != "Revised child objective" || md.TitleSource != sessions.TitleSourceAgent || md.Description != "Assigned brief" || md.Name != res.Session {
		t.Fatalf("child metadata: %+v", md)
	}
	for _, msg := range history {
		if strings.Contains(msg.Content, sessionTitleContract) {
			t.Fatal("title contract persisted")
		}
	}
	md, err = parent.session.GetMetadata(ctx)
	if err != nil || md.Title != "Parent title" || md.TitleSource != sessions.TitleSourceUser {
		t.Fatalf("child changed parent: %+v, %v", md, err)
	}
	tool, _ := parent.toolRegistry.Get(sessionTitleToolName)
	for _, tt := range []struct {
		args map[string]any
		code string
	}{
		{map[string]any{"title": "  "}, "INVALID_TITLE"},
		{map[string]any{"title": "Overwrite"}, "TITLE_PROTECTED"},
	} {
		_, err := tool.Execute(ctx, tt.args)
		var te *tools.ToolError
		if !errors.As(err, &te) || te.Code != tt.code {
			t.Fatalf("tool error = %v", err)
		}
	}
	guidance, err := sessionTitleGuidance(ctx, parent)
	if err != nil || !strings.Contains(guidance, `"title":"Parent title"`) || !strings.Contains(guidance, `"source":"user"`) {
		t.Fatalf("guidance: %s, %v", guidance, err)
	}
}

func TestSessionTitleChildSeed(t *testing.T) {
	ctx := context.Background()
	model := &scriptedStreamLLM{}
	hits := 0
	parent, _ := newSpawnTestParent(t, model, &hits)
	for _, label := range []string{"Original child brief", "invalid\x00label"} {
		child, err := openChildState(ctx, model, parent, subagent.Request{Task: "Task", Label: label, Tools: []string{sessionTitleToolName}})
		if err != nil {
			t.Fatal(err)
		}
		md, _ := child.session.GetMetadata(ctx)
		if label == "Original child brief" && (md.Title != label || md.TitleSource != sessions.TitleSourceAgent) {
			t.Fatalf("seed: %+v", md)
		}
		if strings.ContainsRune(label, 0) && md.Title != "" {
			t.Fatalf("invalid seed: %+v", md)
		}
		if err := child.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSessionTitleUIReconcilesDelayedEvents(t *testing.T) {
	ctx := context.Background()
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "first-handle", "second-handle")
	tab := r.tabs[0]
	setter := tab.state.session.(sessions.TitleSession)
	if _, err := setter.SetTitle(ctx, "Agent title", sessions.TitleSourceAgent); err != nil {
		t.Fatal(err)
	}
	tui := &gotuiTurnUI{repl: r, state: tab.state, model: tab.model}
	r.inspect(tabViewTarget(tab))
	waitInspector(t, r, 80)
	r.openSessionsPicker()
	r.model.modal.input.setText("first-handle")
	r.model.modal.selected = 0
	tui.SessionTitleChanged(tab.state.session)
	// The manual edit commits before the queued agent notification arrives.
	if _, err := setter.SetTitle(ctx, "Manual title", sessions.TitleSourceUser); err != nil {
		t.Fatal(err)
	}
	tab.model.ed.setText("unfinished draft")
	visible := r.model
	select {
	case fn := <-r.uiTasks:
		fn()
	default:
		t.Fatal("missing title event")
	}
	if tab.model.status.displayLabel() != "Manual title" || tab.name != "first-handle" || tab.model.ed.text() != "unfinished draft" || r.model != visible {
		t.Fatal("title event changed identity/focus/draft or painted stale text")
	}
	if !strings.Contains(tab.model.frameTitle(), "Manual title") || !strings.Contains(tab.model.mastheadTitle(100), "Manual title") {
		t.Fatal("title missing from chrome")
	}
	if r.model.status.displayLabel() != "second-handle" {
		t.Fatal("hidden title update reached visible session")
	}
	if r.model.modal.input.text() != "first-handle" || len(r.model.modal.filteredItems()) != 1 || !strings.Contains(r.model.modal.filteredItems()[0].label, "Manual title") {
		t.Fatal("open picker lost its filter/selection or title stayed stale")
	}
	r.refreshInspector(80)
	header := r.inspectorHeader(80, 20, 0, 0)
	if !strings.Contains(plainStyledText(header.text), "Manual title") || headerButton(header.buttons, "parent").Empty() {
		t.Fatalf("inspector title/navigation: %s", header.text)
	}
}

func TestSessionTitlePickerRejectsReusedIdentityAndExternalLease(t *testing.T) {
	ctx := context.Background()
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "root")
	saved := testAcquireSession(t, store, "saved")
	if err := saved.Close(); err != nil {
		t.Fatal(err)
	}
	summaries, _ := store.ListSummaries(ctx)
	var target sessions.SessionSummary
	for _, summary := range summaries {
		if summary.Metadata.Name == "saved" {
			target = summary
		}
	}
	if err := store.Delete(ctx, "saved"); err != nil {
		t.Fatal(err)
	}
	reused := testAcquireSession(t, store, "saved")
	r.editSessionTitle(target, "Wrong identity")
	md, _ := reused.GetMetadata(ctx)
	if md.Title != "" {
		t.Fatal("reused handle received old title edit")
	}
	summaries, _ = store.ListSummaries(ctx)
	for _, summary := range summaries {
		if summary.Metadata.Name == "saved" {
			target = summary
		}
	}
	r.editSessionTitle(target, "External lease")
	md, _ = reused.GetMetadata(ctx)
	if md.Title != "" {
		t.Fatal("external lease was mutated")
	}
}

func TestSessionTitlePickerSearchAndManualClaim(t *testing.T) {
	ctx := context.Background()
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "first-handle", "second-handle")
	longTitle := strings.Repeat("word ", 10) + "search-tail"
	for _, tab := range r.tabs {
		if _, err := tab.state.session.(sessions.TitleSession).SetTitle(ctx, longTitle, sessions.TitleSourceAgent); err != nil {
			t.Fatal(err)
		}
	}
	r.openSessionsPicker()
	r.model.modal.input.setText("search-tail")
	items := r.model.modal.filteredItems()
	if len(items) != 2 || items[0].value == items[1].value {
		t.Fatalf("duplicate title results: %+v", items)
	}
	r.model.modal.input.setText("first-handle")
	if len(r.model.modal.filteredItems()) != 1 {
		t.Fatal("handle did not match")
	}
	r.model.modal.selected = 0
	r.handleModalEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<F2>"})
	if r.model.modal.input.text() != longTitle {
		t.Fatalf("prefill: %q", r.model.modal.input.text())
	}
	r.handleModalEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
	md, _ := r.tabs[0].state.session.GetMetadata(ctx)
	if md.Title != longTitle || md.TitleSource != sessions.TitleSourceUser || md.Name != "first-handle" {
		t.Fatalf("unchanged manual title not claimed: %+v", md)
	}
}

func TestTitleCommandPreservesHandleAndExpiry(t *testing.T) {
	ctx := context.Background()
	store := testOpenMemoryStore(t, nil)
	session, err := store.Acquire(ctx, "quiet-otter", sessions.AcquireOptions{Auto: true})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	before, _ := session.GetMetadata(ctx)
	state := &conversationState{session: session, sessionStore: store}
	c := &replCommandContext{ctx: ctx, state: state, reply: func(line string) error { t.Errorf("unexpected notice: %s", line); return nil }}
	result := replTitleCommand(c, []string{"/title", "Manual", "session", "title"})
	if result.err != nil {
		t.Fatal(result.err)
	}
	md, _ := session.GetMetadata(ctx)
	if md.Title != "Manual session title" || md.TitleSource != sessions.TitleSourceUser || md.Name != before.Name || md.TTL != before.TTL || !md.LastUsed.Equal(before.LastUsed) {
		t.Fatalf("title command: %+v", md)
	}
}
