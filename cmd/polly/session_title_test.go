package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/swarm"
	"github.com/alexschlessinger/pollytool/tools"
	ui "github.com/metaspartan/gotui/v5"
)

func newTitleSwarm(t *testing.T, model llm.LLM, configure func(*swarm.Config)) *managedREPL {
	t.Helper()
	r := newSwarmTestREPL(t, model, configure)
	registerSessionTitleTool(r.state)
	return r
}

func TestSessionTitleToolChildIsolation(t *testing.T) {
	ctx := context.Background()
	model := integrationModel(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		for _, tool := range req.Tools {
			if tool.GetName() == sessionTitleToolName {
				t.Error("child received title tool")
			}
		}
		for _, msg := range req.Messages {
			if strings.Contains(msg.Content, sessionTitleContract) {
				t.Error("child received title guidance")
			}
		}
		return spawnTestReply("done")
	})
	r := newTitleSwarm(t, model, nil)
	parent, store := r.state, r.state.sessionStore
	if _, err := parent.session.(sessions.TitleSession).SetTitle(ctx, "Parent title", sessions.TitleSourceUser); err != nil {
		t.Fatal(err)
	}
	res, err := parent.swarm.Agent(ctx, "", swarm.AgentRequest{Task: "Investigate", Label: "Assigned brief", ReadOnly: true, Tools: []string{"read_transcript"}})
	if err != nil {
		t.Fatal(err)
	}
	view, err := store.(sessions.ViewStore).ReadView(ctx, sessions.ViewTarget{ID: res.Session}, "")
	if err != nil {
		t.Fatal(err)
	}
	md := view.Metadata
	if md.Title != "Assigned brief" || md.TitleSource != sessions.TitleSourceAgent || md.Description != "Assigned brief" || md.Name == res.Session {
		t.Fatalf("child metadata: %+v", md)
	}
	child, err := store.Acquire(ctx, md.Name, sessions.AcquireOptions{ExistingOnly: true, ExpectedID: res.Session})
	if err != nil {
		t.Fatal(err)
	}
	// Opening a child as a conversation must not register the root-only tool.
	childRegistry := tools.NewToolRegistry(nil)
	defer childRegistry.Close()
	childState := &conversationState{session: child, toolRegistry: childRegistry}
	registerSessionTitleTool(childState)
	if _, exists := childRegistry.Get(sessionTitleToolName); exists {
		t.Fatal("opened child received root title tool")
	}
	if guidance, err := sessionTitleGuidance(ctx, childState); err != nil || guidance != "" {
		t.Fatalf("opened child title guidance = %q, %v", guidance, err)
	}
	for _, msg := range testSessionHistory(t, child) {
		if strings.Contains(msg.Content, sessionTitleContract) {
			t.Fatal("title contract persisted")
		}
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
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
	for _, label := range []string{"Original child brief", "invalid\x00label"} {
		t.Run(label, func(t *testing.T) {
			ctx := context.Background()
			r := newTitleSwarm(t, integrationModel(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return spawnTestReply("done") }), nil)
			res, err := r.state.swarm.Agent(ctx, "", swarm.AgentRequest{Task: "Task", Label: label, ReadOnly: true, Tools: []string{}})
			if strings.ContainsRune(label, 0) {
				if err == nil {
					t.Fatal("invalid label created an agent")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			view, err := r.state.sessionStore.(sessions.ViewStore).ReadView(ctx, sessions.ViewTarget{ID: res.Session}, "")
			if err != nil {
				t.Fatal(err)
			}
			md := view.Metadata
			if label == "Original child brief" && (md.Title != label || md.TitleSource != sessions.TitleSourceAgent) {
				t.Fatalf("seed: %+v", md)
			}
		})
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

func TestSessionTitleMemberRequestBoundaries(t *testing.T) {
	for _, tt := range []struct {
		name                 string
		tools                []string
		disabled, structured bool
	}{
		{name: "default"},
		{name: "explicit title", tools: []string{sessionTitleToolName}},
		{name: "other allowed tool", tools: []string{"read_transcript"}},
		{name: "no tools", tools: []string{}},
		{name: "disabled", disabled: true},
		{name: "structured", structured: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			wantTools := !tt.disabled && (tt.tools == nil || len(tt.tools) > 0)
			model := integrationModel(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
				hasTitle := false
				for _, tool := range req.Tools {
					hasTitle = hasTitle || tool.GetName() == sessionTitleToolName
					if tt.tools != nil && tool.GetName() == "bash" {
						t.Error("host title binding widened filesystem tool allowlist")
					}
				}
				if hasTitle || !wantTools && len(req.Tools) > 0 {
					t.Errorf("unexpected child tools: title available=%v, tools=%d", hasTitle, len(req.Tools))
				}
				hasGuidance := false
				for _, msg := range req.Messages {
					hasGuidance = hasGuidance || strings.Contains(msg.Content, sessionTitleContract)
				}
				if hasGuidance {
					t.Errorf("guidance=%v", hasGuidance)
				}
				if tt.structured {
					return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse,
						ToolCalls: []messages.ChatMessageToolCall{{ID: "complete", Name: "swarm_complete", Arguments: `{"value":{"ok":true}}`}}}
				}
				return spawnTestReply("done")
			})
			r := newTitleSwarm(t, model, nil)
			if tt.disabled {
				r.state.swarm.UpdateDefaults(llm.CompletionRequest{Model: "test/model"}, llm.AgentConfig{MaxIterations: 10, DisableTools: true}, nil)
			}
			req := swarm.AgentRequest{Task: "Task", Label: "Assigned brief", ReadOnly: true, Tools: tt.tools}
			if tt.structured {
				req.Schema = map[string]any{"type": "object", "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}, "required": []string{"ok"}, "additionalProperties": false}
			}
			res, err := r.state.swarm.Agent(ctx, "", req)
			if tt.name == "explicit title" {
				if err == nil {
					t.Fatal("child accepted title-only tool request")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			view, err := r.state.sessionStore.(sessions.ViewStore).ReadView(ctx, sessions.ViewTarget{ID: res.Session}, "")
			if err != nil {
				t.Fatal(err)
			}
			if view.Metadata.Title != "Assigned brief" {
				t.Fatalf("unseeded title: %+v", view.Metadata)
			}
			for _, msg := range view.History {
				if strings.Contains(msg.Content, sessionTitleContract) {
					t.Fatal("request guidance entered history")
				}
			}
		})
	}
}

func TestSessionTitleMemberRenameAndManualContinuation(t *testing.T) {
	ctx := context.Background()
	r := newTitleSwarm(t, integrationModel(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		for _, tool := range req.Tools {
			if tool.GetName() == sessionTitleToolName {
				t.Error("continued child received title tool")
			}
		}
		for _, msg := range req.Messages {
			if strings.Contains(msg.Content, sessionTitleContract) {
				t.Error("continued child received title guidance")
			}
		}
		return spawnTestReply("done")
	}), nil)
	res, err := r.state.swarm.Agent(ctx, "", swarm.AgentRequest{Task: "First task", Label: "Assigned brief", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	view, err := r.state.sessionStore.(sessions.ViewStore).ReadView(ctx, sessions.ViewTarget{ID: res.Session}, "")
	if err != nil {
		t.Fatal(err)
	}
	oldName := view.Metadata.Name
	child, err := r.state.sessionStore.Acquire(ctx, oldName, sessions.AcquireOptions{ExistingOnly: true, ExpectedID: res.Session})
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Rename(ctx, "renamed-child"); err != nil {
		t.Fatal(err)
	}
	if _, err := child.(sessions.TitleSession).SetTitle(ctx, "Manual child title", sessions.TitleSourceUser); err != nil {
		t.Fatal(err)
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	replacement := testAcquireSession(t, r.state.sessionStore, oldName)
	defer replacement.Close()
	continued, err := r.state.swarm.Agent(ctx, "", swarm.AgentRequest{Session: res.Session, Task: "Continue"})
	if err != nil {
		t.Fatal(err)
	}
	if continued.Session != res.Session {
		t.Fatal("continuation changed stable identity")
	}
	view, err = r.state.sessionStore.(sessions.ViewStore).ReadView(ctx, sessions.ViewTarget{ID: res.Session}, "")
	if err != nil {
		t.Fatal(err)
	}
	if view.Metadata.Name != "renamed-child" || view.Metadata.Title != "Manual child title" || view.Metadata.TitleSource != sessions.TitleSourceUser {
		t.Fatalf("manual title or handle changed: %+v", view.Metadata)
	}
	md, err := replacement.GetMetadata(ctx)
	if err != nil || md.Title != "" {
		t.Fatalf("replacement was touched: %+v %v", md, err)
	}
	for _, msg := range view.History {
		if strings.Contains(msg.Content, sessionTitleContract) {
			t.Fatal("continuation guidance entered history")
		}
	}
}

func TestF2TitleEditorSupportsRoutineEditingKeys(t *testing.T) {
	r := newTabTestREPL(t, testOpenMemoryStore(t, nil), "root")
	r.model.ed.setText("keep composer draft")
	_, err := r.state.session.(sessions.TitleSession).SetTitle(context.Background(), "Old title", sessions.TitleSourceAgent)
	if err != nil {
		t.Fatal(err)
	}
	r.openSessionsPickerSelected("root")
	key := func(id string) { r.handleModalEvent(ui.Event{Type: ui.KeyboardEvent, ID: id}) }
	key("<F2>")
	m := r.model.modal
	if m == nil || m.title != "Edit title" || m.input.text() != "Old title" {
		t.Fatal("missing prefilled title editor")
	}
	for _, step := range []struct {
		key    string
		cursor int
	}{{"<Home>", 0}, {"<Right>", 1}, {"<Left>", 0}, {"<End>", 9}, {"<C-a>", 0}, {"<C-e>", 9}} {
		key(step.key)
		if m.input.cursor != step.cursor {
			t.Fatalf("%s cursor = %d, want %d", step.key, m.input.cursor, step.cursor)
		}
	}
	key("<C-u>")
	if !m.input.empty() {
		t.Fatal("Ctrl-U retained the prefilled title")
	}
	for _, ch := range "New title!" {
		key(string(ch))
	}
	key("<Left>")
	key("<Delete>")
	if m.input.text() != "New title" {
		t.Fatalf("title correction: %q", m.input.text())
	}
	key("<Enter>")
	md, err := r.state.session.GetMetadata(context.Background())
	if err != nil || md.Title != "New title" || md.TitleSource != sessions.TitleSourceUser || md.Name != "root" {
		t.Fatalf("edited title: %+v %v", md, err)
	}
	if r.model.ed.text() != "keep composer draft" {
		t.Fatal("modal changed the composer")
	}
}
