package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	ui "github.com/metaspartan/gotui/v5"
)

func TestNewModalParagraphUsesValidGotuiStyleArguments(t *testing.T) {
	if got := newModalParagraph(); got == nil {
		t.Fatal("newModalParagraph() returned nil")
	}
}

func TestModalInputPlaceholderRendersAsStyledText(t *testing.T) {
	m := &replModal{inputMode: true, helper: "Enter save"}
	got := plainStyledText(m.text(10, 60))
	if strings.Contains(got, "fg:muted") || !strings.Contains(got, "type a value") {
		t.Fatalf("placeholder = %q", got)
	}
	lines := strings.Split(got, "\n")
	if helper := lines[len(lines)-1]; helper != strings.Repeat(" ", 24)+"Enter save" {
		t.Fatalf("centered input helper = %q", helper)
	}
}

func TestModelPickerAppliesExistingSettingPath(t *testing.T) {
	cfg := &Config{Launch: Settings{Model: "anthropic/claude-sonnet-4-6", MaxHistoryTokens: 256_000}}
	r := newManagedREPL(cfg, "ctx", 0, 0)
	r.state = &conversationState{settings: cfg.Launch}
	settings := &r.state.settings
	r.startupLogoVisible = true
	r.model.status.recordContextUsage(50_000, 156_000, false)

	if handled, quit := r.runCommand("/model"); !handled || quit || r.model.modal == nil {
		t.Fatalf("/model handled=%v quit=%v modal=%#v", handled, quit, r.model.modal)
	}
	if r.startupLogoVisible {
		t.Fatal("opening /model left the startup logo visible behind the modal")
	}
	r.openProviderModels("openai")
	r.applySelectedModel("openai/gpt-5.4")

	if settings.Model != "openai/gpt-5.4" || r.model.status.modelName != settings.Model {
		t.Fatalf("selected model settings=%q status=%q", settings.Model, r.model.status.modelName)
	}
	if cfg.Launch.Model != "anthropic/claude-sonnet-4-6" {
		t.Fatalf("picker rewrote the launch settings: %q", cfg.Launch.Model)
	}
	if r.model.status.contextUsed != 0 || r.model.status.contextLimit != settings.MaxHistoryTokens {
		t.Fatalf("model switch retained stale context usage: %d/%d", r.model.status.contextUsed, r.model.status.contextLimit)
	}
	if got := r.model.status.recentModels; len(got) == 0 || got[0] != "openai/gpt-5.4" {
		t.Fatalf("recent models after picker = %v, want the selection first", got)
	}
}

// Every model-change entry point shares one apply path, so /set model must
// feed the picker's recent list exactly as a picker selection does.
func TestSetModelRemembersRecentModel(t *testing.T) {
	cfg := &Config{Launch: Settings{Model: "anthropic/claude-sonnet-4-6", MaxHistoryTokens: 256_000}}
	r := newManagedREPL(cfg, "ctx", 0, 0)
	r.state = &conversationState{settings: cfg.Launch}
	settings := &r.state.settings

	if handled, quit := r.runCommand("/set model openai/gpt-5.4"); !handled || quit {
		t.Fatalf("/set model handled=%v quit=%v", handled, quit)
	}
	if settings.Model != "openai/gpt-5.4" || r.model.status.modelName != settings.Model {
		t.Fatalf("set model settings=%q status=%q", settings.Model, r.model.status.modelName)
	}
	if got := r.model.status.recentModels; len(got) != 2 || got[0] != "openai/gpt-5.4" || got[1] != "anthropic/claude-sonnet-4-6" {
		t.Fatalf("recent models after /set model = %v, want the new model ahead of the launch model", got)
	}
	r.runCommand("/set model openai/gpt-5.4")
	if got := r.model.status.recentModels; len(got) != 2 {
		t.Fatalf("re-setting the same model duplicated the recent list: %v", got)
	}
}

func TestKeyModalMasksAndInstallsSessionCredential(t *testing.T) {
	agent := llm.NewAgent(llm.NewMultiPass(map[string]string{"openai": "environment-key"}), nil, llm.AgentConfig{})
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.state = &conversationState{agent: agent}
	r.openProviderKeyInput("openai")

	for _, ch := range "super-secret" {
		r.handleModalEvent(ui.Event{Type: ui.KeyboardEvent, ID: string(ch)})
	}
	if rendered := plainStyledText(r.model.modal.text(10, 60)); strings.Contains(rendered, "super-secret") {
		t.Fatalf("masked modal exposed key: %q", rendered)
	}
	r.handleModalEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
	if source := agent.ProviderAPIKeySource("openai"); source != "session" {
		t.Fatalf("key source = %q, want session", source)
	}
}

func TestResumePickerListsRecentSessionsAndOpensThemInTabs(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	target := testAcquireSession(t, store, "older-work")
	targetMetadata, err := target.GetMetadata(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	targetMetadata.Model = "openai/gpt-5.4"
	if err := target.SetMetadata(context.Background(), targetMetadata); err != nil {
		t.Fatal(err)
	}
	testAddMessages(t, target, []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "question"},
		{Role: messages.MessageRoleAssistant, Content: "answer"},
	})
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}
	r := newTabTestREPL(t, store, "current-work")
	current := r.tabs[0].state.session
	status := r.model.statusRow(80)
	if !strings.Contains(status, "[current-work](fg:accent)") {
		t.Fatalf("clickable session was not accented: %q", status)
	}
	if plain := plainStyledText(status); strings.Contains(plain, "tab 1/1") {
		t.Fatalf("a lone tab was placed in the status row: %q", plain)
	}
	placement := r.model.status.sessionField
	if !placement.hit(placement.X, 23, 24) || placement.hit(placement.X-1, 23, 24) {
		t.Fatalf("status session placement = %#v", placement)
	}
	r.handleEvent(ui.Event{Type: ui.MouseEvent, ID: "<MouseLeft>", Payload: ui.Mouse{X: placement.X, Y: 23}})
	if r.model.modal == nil {
		t.Fatal("clicking the status session did not open the sessions picker")
	}
	if r.model.modal.title != "Sessions" {
		t.Fatalf("modal title = %q", r.model.modal.title)
	}
	if r.model.modal.width != 64 {
		t.Fatalf("modal width = %d, want 64", r.model.modal.width)
	}
	if r.model.modal.maxRows != 14 || !r.model.modal.showCount {
		t.Fatalf("modal window = %d rows, count=%v", r.model.modal.maxRows, r.model.modal.showCount)
	}
	if got := modalWidthForTerminal(140, r.model.modal.width); got != 64 {
		t.Fatalf("rendered modal width = %d, want 64", got)
	}
	if footer := plainStyledText(r.model.modal.text(40, 64)); !strings.Contains(footer, "F2 rename") {
		t.Fatalf("resume modal lacks rename affordance: %q", footer)
	}
	for _, item := range r.model.modal.items {
		if strings.Contains(item.label, "openai/") {
			t.Fatalf("resume row exposed model/provider: %q", item.label)
		}
		if strings.Contains(item.label, "ago") {
			t.Fatalf("resume row used verbose age: %q", item.label)
		}
		if item.value == "older-work" && !strings.Contains(item.label, "2 msgs") {
			t.Fatalf("resume row lacks durable session length: %q", item.label)
		}
	}
	selectedItem := r.model.modal.items[r.model.modal.selected]
	if !strings.Contains(selectedItem.selectedDisplay, "fg:accent") || !strings.Contains(selectedItem.selectedDisplay, "fg:muted") {
		t.Fatalf("selected row does not split accent and muted fields: %q", selectedItem.selectedDisplay)
	}
	if got := r.model.modal.items[r.model.modal.selected].value; got != "current-work" {
		t.Fatalf("selected session = %q, want current-work", got)
	}

	for i, item := range r.model.modal.items {
		if item.value == "older-work" {
			r.model.modal.selected = i
			break
		}
	}
	r.handleModalEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
	if r.opening != "older-work" {
		t.Fatalf("selection did not start opening the session: opening=%q", r.opening)
	}
	select {
	case <-r.quit:
		t.Fatal("session selection tore the managed REPL down instead of opening a tab")
	default:
	}
	r.finishOpen(<-r.openDone)
	if r.opening != "" {
		t.Fatalf("open did not settle: opening=%q", r.opening)
	}
	if len(r.tabs) != 2 || r.visibleTabIndex() != 1 {
		t.Fatalf("tabs after open = %d, visible %d; want 2 with the new tab visible", len(r.tabs), r.visibleTabIndex())
	}
	if name, err := r.state.session.GetName(context.Background()); err != nil || name != "older-work" {
		t.Fatalf("live session = %q, %v; want older-work", name, err)
	}
	if r.model.status.contextName != "older-work" {
		t.Fatalf("status context = %q, want older-work", r.model.status.contextName)
	}
	transcript := r.model.fullTranscript()
	if !strings.Contains(transcript, "question") || !strings.Contains(transcript, "answer") {
		t.Fatalf("opened transcript was not hydrated: %q", transcript)
	}
	if strings.Contains(transcript, "opened older-work in tab 2") {
		t.Fatalf("routine opened notice remains: %q", transcript)
	}
	// The session left behind stays open in its tab, still leased here.
	if current.Context().Err() != nil {
		t.Fatal("previous session was closed by opening another tab")
	}
	if !sessionInUse(t, store, "current-work") {
		t.Fatal("previous session dropped its lease")
	}

	// Picking it again shows its tab instead of opening a second copy.
	r.openSessionsPicker()
	for i, item := range r.model.modal.items {
		if item.value == "current-work" {
			if !strings.HasSuffix(item.label, "workspace 1") {
				t.Fatalf("open session not marked with its tab: %q", item.label)
			}
			r.model.modal.selected = i
		}
	}
	r.handleModalEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
	if r.opening != "" || r.showTabRequest != 0 {
		t.Fatalf("picking an open session did not request its tab: opening=%q request=%d", r.opening, r.showTabRequest)
	}
	r.applyTabRequests()
	if r.state.session != current || r.visibleTabIndex() != 0 {
		t.Fatal("picking an open session did not show its tab")
	}
}

func TestResumePickerRenamesSavedAndCurrentSessions(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	saved := testAcquireSession(t, store, "saved-work")
	if err := saved.Close(); err != nil {
		t.Fatal(err)
	}
	current := testAcquireSession(t, store, "current-work")
	r := newManagedREPL(&Config{}, "current-work", 0, 0)
	r.state = &conversationState{sessionStore: store, session: current}

	r.openSessionsPicker()
	for i, item := range r.model.modal.items {
		if item.value == "saved-work" {
			r.model.modal.selected = i
		}
	}
	r.handleModalEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<F2>"})
	if r.model.modal == nil || r.model.modal.title != "Rename session" || r.model.modal.input.text() != "saved-work" {
		t.Fatalf("saved rename modal = %#v", r.model.modal)
	}
	r.model.modal.input.setText("renamed-work")
	r.handleModalEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
	if exists, err := store.Exists(context.Background(), "renamed-work"); err != nil || !exists {
		t.Fatalf("renamed saved session exists=%v, err=%v", exists, err)
	}
	if exists, err := store.Exists(context.Background(), "saved-work"); err != nil || exists {
		t.Fatalf("old saved session exists=%v, err=%v", exists, err)
	}
	if r.model.modal == nil || r.model.modal.items[r.model.modal.selected].value != "renamed-work" {
		t.Fatalf("picker did not retain renamed selection: %#v", r.model.modal)
	}

	for i, item := range r.model.modal.items {
		if item.value == "current-work" {
			r.model.modal.selected = i
		}
	}
	r.handleModalEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<F2>"})
	r.model.modal.input.setText("renamed-current")
	r.handleModalEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Enter>"})
	if got := r.model.status.contextName; got != "renamed-current" {
		t.Fatalf("status session after rename = %q", got)
	}
	if got, err := current.GetName(context.Background()); err != nil || got != "renamed-current" {
		t.Fatalf("current session after rename = %q, %v", got, err)
	}
	select {
	case <-r.quit:
		t.Fatal("renaming a session requested a resume restart")
	default:
	}
}

func TestResumeModalBoundsRowsAndReportsVisibleRange(t *testing.T) {
	items := make([]replModalItem, 20)
	for i := range items {
		items[i] = replModalItem{label: fmt.Sprintf("session-%02d", i+1)}
	}
	m := &replModal{items: items, maxRows: 14, showCount: true}
	first := plainStyledText(m.text(40, 60))
	if !strings.Contains(first, "session-14") || strings.Contains(first, "session-15") || !strings.Contains(first, "1–14 of 20") {
		t.Fatalf("first modal window = %q", first)
	}
	lines := strings.Split(first, "\n")
	if footer := lines[len(lines)-1]; footer != "     1–14 of 20 · type filter · ↑↓ · Enter open · Esc" {
		t.Fatalf("centered footer = %q", footer)
	}
	m.selected = 15
	scrolled := plainStyledText(m.text(40, 60))
	if !strings.Contains(scrolled, "session-16") || !strings.Contains(scrolled, "3–16 of 20") {
		t.Fatalf("scrolled modal window = %q", scrolled)
	}
	m.input.setText("session-1")
	filtered := plainStyledText(m.text(40, 60))
	if !strings.Contains(filtered, "10 matches") || !strings.Contains(filtered, "/session-1") {
		t.Fatalf("filtered modal window = %q", filtered)
	}
}

func TestFormatCompactDuration(t *testing.T) {
	for _, tc := range []struct {
		duration time.Duration
		want     string
	}{
		{30 * time.Second, "now"},
		{10 * time.Minute, "10m"},
		{8 * time.Hour, "8h"},
		{49 * time.Hour, "2d"},
	} {
		if got := formatCompactDuration(tc.duration); got != tc.want {
			t.Errorf("formatCompactDuration(%s) = %q, want %q", tc.duration, got, tc.want)
		}
	}
}

func TestContextUsageStatusIsProviderVisibleAndCompact(t *testing.T) {
	m := newReplModel()
	m.status.modelName = "openai/gpt-5.4"
	m.status.contextName = "work"
	m.status.recordContextUsage(41_200, 156_000, false)
	wide := plainStyledText(m.statusRow(120))
	if !strings.Contains(wide, "41.2k/156k") {
		t.Fatalf("status = %q", wide)
	}
	m.status.recordContextUsage(12_300, 156_000, true)
	if got := plainStyledText(m.statusRow(120)); !strings.Contains(got, "~12.3k/156k") {
		t.Fatalf("estimated status = %q", got)
	}
}

func TestResumePickerNestsAgentsUnderTheirParent(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	ctx := context.Background()
	spawn := func(name, parent, description string) {
		session, err := store.Acquire(ctx, name, sessions.AcquireOptions{Parent: parent})
		if err != nil {
			t.Fatal(err)
		}
		metadata, err := session.GetMetadata(ctx)
		if err != nil {
			t.Fatal(err)
		}
		metadata.Description = description
		if err := session.SetMetadata(ctx, metadata); err != nil {
			t.Fatal(err)
		}
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := testAcquireSession(t, store, "gamma").Close(); err != nil {
		t.Fatal(err)
	}
	spawn("epsilon", "gamma", "")
	spawn("delta", "gamma", "count files")
	r := newTabTestREPL(t, store, "current-work")

	values := func() []string {
		var out []string
		for _, item := range r.model.modal.filteredItems() {
			out = append(out, item.value)
		}
		return out
	}
	selectedValue := func() string {
		items := r.model.modal.filteredItems()
		return items[r.model.modal.selected].value
	}
	key := func(id string) { r.handleModalEvent(ui.Event{Type: ui.KeyboardEvent, ID: id}) }

	r.openSessionsPicker()
	m := r.model.modal
	if m == nil || m.width != sessionsPickerNestedWidth {
		t.Fatalf("picker with agents = %#v, want the wider modal", m)
	}
	if got := strings.Join(values(), " "); got != "current-work gamma" {
		t.Fatalf("collapsed picker lists %q, want the agents hidden under gamma", got)
	}
	text := plainStyledText(m.text(40, m.width))
	if !strings.Contains(text, "▸ 2 agents") || !strings.Contains(text, "→ agents") {
		t.Fatalf("collapsed parent lacks its agent count or hint: %q", text)
	}

	key("<Down>")
	if selectedValue() != "gamma" {
		t.Fatalf("selected %q, want gamma", selectedValue())
	}
	key("<Right>")
	if got := strings.Join(values(), " "); got != "current-work gamma delta epsilon" {
		t.Fatalf("expanded picker lists %q", got)
	}
	if selectedValue() != "gamma" {
		t.Fatalf("expanding moved the selection to %q", selectedValue())
	}
	items := m.filteredItems()
	if !strings.HasPrefix(items[2].label, "↳ delta") || items[2].parent != "gamma" || items[2].searchText != "count files" || !strings.Contains(items[2].display, styled("count files", "muted", "")) {
		t.Fatalf("agent row = %#v, want it named by its session under gamma with its brief muted after", items[2])
	}
	if !strings.HasPrefix(items[3].label, "↳ epsilon") {
		t.Fatalf("unlabeled agent row = %q, want its session name", items[3].label)
	}
	if text := plainStyledText(m.text(40, m.width)); !strings.Contains(text, "▾ 2 agents") {
		t.Fatalf("expanded parent marker missing: %q", text)
	}

	key("<Down>")
	key("<Left>")
	if got := strings.Join(values(), " "); got != "current-work gamma" {
		t.Fatalf("collapsing from an agent row lists %q", got)
	}
	if selectedValue() != "gamma" {
		t.Fatalf("collapsing from an agent selected %q, want its parent", selectedValue())
	}

	for _, ch := range "count" {
		key(string(ch))
	}
	if got := strings.Join(values(), " "); got != "delta" {
		t.Fatalf("filter reached %q, want the collapsed agent by its label", got)
	}
	key("<Enter>")
	if r.opening != "delta" {
		t.Fatalf("picking a filtered agent opened %q", r.opening)
	}
	r.finishOpen(<-r.openDone)
	if len(r.tabs) != 2 || r.visibleTab().name != "gamma" || !r.workspace().inspector.open || r.workspace().inspector.target.session.Name != "delta" {
		t.Fatalf("agent did not open in its parent's inspector: %d tabs, root %s", len(r.tabs), r.visibleTab().name)
	}

	// Reopening on an agent shows it, expanding its parent.
	r.openSessionsPickerSelected("epsilon")
	if selectedValue() != "epsilon" {
		t.Fatalf("picker opened on %q, want epsilon", selectedValue())
	}
	if !r.pickerExpanded["gamma"] {
		t.Fatal("opening on an agent did not expand its parent")
	}
}

// The picker lists the open workspaces first, in tab order, with what each
// is doing; a workspace with a live agent starts expanded and the agent that
// needs an approval leads. Saved sessions follow.
func TestSessionsPickerListsOpenWorkspacesFirstWithAgents(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	ctx := context.Background()
	for _, name := range []string{"older-saved", "newer-saved", "root"} {
		if err := testAcquireSession(t, store, name).Close(); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"idle-agent", "waiting-agent"} {
		child, err := store.Acquire(ctx, name, sessions.AcquireOptions{Parent: "root"})
		if err != nil {
			t.Fatal(err)
		}
		if err := child.Close(); err != nil {
			t.Fatal(err)
		}
	}
	r := newTabTestREPL(t, store, "root", "second")
	root := r.tabs[0]
	// Live agent tabs report to root without holding the screen.
	for _, name := range []string{"idle-agent", "waiting-agent"} {
		r.tabs = append(r.tabs, &replTab{name: name, parent: root, parentName: root.name, model: newReplModel()})
	}
	r.showTab(0)
	r.tabs[1].model.busy = true
	r.tabs[1].model.state = turnStateStreaming
	r.tabs[3].model.approval = &approvalState{reply: make(chan []bool, 1)}
	r.openSessionsPicker()
	m := r.model.modal
	if m == nil || m.title != "Sessions" {
		t.Fatalf("picker = %#v", m)
	}
	var order []string
	for _, item := range m.filteredItems() {
		order = append(order, item.value)
	}
	if got := strings.Join(order, " "); got != "root waiting-agent idle-agent second newer-saved older-saved" {
		t.Fatalf("picker order = %q", got)
	}
	labels := make(map[string]string)
	markers := make(map[string]string)
	for _, item := range m.items {
		labels[item.value] = item.label
		markers[item.value] = m.nestMarker(item)
	}
	for value, want := range map[string]string{
		"root":          "current",
		"second":        "workspace 2 · streaming",
		"waiting-agent": "approval needed",
		"idle-agent":    "active agent",
	} {
		if !strings.HasSuffix(labels[value], want) {
			t.Fatalf("%s row = %q, want suffix %q", value, labels[value], want)
		}
	}
	// The parent's marker carries the count and what the agents are doing.
	if markers["root"] != "▾ 2 agents · 1 needs approval" {
		t.Fatalf("root marker = %q", markers["root"])
	}
	if !r.pickerExpanded["root"] {
		t.Fatal("workspace with live agents did not start expanded")
	}
}

// The open picker follows the tabs: an agent's row changes as it works and
// settles, the parent's marker with it, a session spawned meanwhile joins
// under its parent, and the selection stays on its row throughout.
func TestSessionsPickerRefreshesWhileOpen(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	ctx := context.Background()
	for _, name := range []string{"root", "saved"} {
		if err := testAcquireSession(t, store, name).Close(); err != nil {
			t.Fatal(err)
		}
	}
	child, err := store.Acquire(ctx, "busy-agent", sessions.AcquireOptions{Parent: "root"})
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	r := newTabTestREPL(t, store, "root")
	root := r.tabs[0]
	agent := &replTab{name: "busy-agent", parent: root, parentName: root.name, model: newReplModel()}
	r.tabs = append(r.tabs, agent)
	agent.model.busy = true
	agent.model.state = turnStateTool
	agent.model.toolName = "bash"
	agent.model.turnStarted = time.Now().Add(-12 * time.Second)
	r.showTab(0)
	r.openSessionsPicker()
	m := r.model.modal
	if m == nil || m.refresh == nil || m.picker == nil {
		t.Fatalf("picker = %#v, want a live listing", m)
	}
	row := func(value string) string {
		for _, item := range m.items {
			if item.value == value {
				return item.label + "  " + m.nestMarker(item)
			}
		}
		return ""
	}
	values := func() []string {
		var out []string
		for _, item := range m.filteredItems() {
			out = append(out, item.value)
		}
		return out
	}
	if got := row("busy-agent"); !strings.HasSuffix(got, "running bash · 12s  ") {
		t.Fatalf("running agent row = %q", got)
	}
	if got := row("root"); !strings.HasSuffix(got, "current  ▾ 1 agent · 1 running") {
		t.Fatalf("parent row = %q", got)
	}

	// Select the saved session below the agent, then let the agent settle.
	r.handleModalEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Down>"})
	r.handleModalEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<Down>"})
	if got := m.filteredItems()[m.selected].value; got != "saved" {
		t.Fatalf("selected %q, want saved", got)
	}
	agent.model.busy = false
	agent.model.unseenOutcome = turnOutcomeDone
	agent.model.lastElapsed = 41800 * time.Millisecond
	m.refresh()
	if got := row("busy-agent"); !strings.HasSuffix(got, "done · 41.8s  ") {
		t.Fatalf("settled agent row = %q", got)
	}
	if got := row("root"); !strings.HasSuffix(got, "current  ▾ 1 agent") {
		t.Fatalf("parent row after settling = %q", got)
	}

	// A session spawned since the picker opened joins under its parent on
	// the next store read; the selection stays on the saved session.
	late, err := store.Acquire(ctx, "late-agent", sessions.AcquireOptions{Parent: "root"})
	if err != nil {
		t.Fatal(err)
	}
	if err := late.Close(); err != nil {
		t.Fatal(err)
	}
	m.picker.listedAt = time.Time{}
	m.refresh()
	deadline := time.After(5 * time.Second)
	for m.picker.listing {
		select {
		case fn := <-r.uiTasks:
			fn()
		case <-deadline:
			t.Fatal("store listing did not finish")
		}
	}
	m.refresh()
	if got := strings.Join(values(), " "); got != "root busy-agent late-agent saved" {
		t.Fatalf("picker after the listing = %q", got)
	}
	if got := m.filteredItems()[m.selected].value; got != "saved" {
		t.Fatalf("selection moved to %q", got)
	}

	// The agent's tab closing keeps how it ended on its row.
	r.tabs = r.tabs[:1]
	m.refresh()
	if got := row("busy-agent"); !strings.HasSuffix(got, "done · 41.8s  ") || strings.Contains(got, "active agent") {
		t.Fatalf("closed agent row = %q", got)
	}
}

// The status row says what the visible workspace's agents are doing, in a
// field that opens the sessions picker on them; approvals outrank running.
func TestStatusRowShowsTheWorkspaceAgents(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	r := newTabTestREPL(t, store, "root")
	root := r.tabs[0]
	r.model.status.modelName = "gpt-mini"
	if text, color := r.agentsStatus(); text != "" || color != "" {
		t.Fatalf("idle workspace reports %q", text)
	}
	one := &replTab{name: "one", parent: root, parentName: root.name, model: newReplModel()}
	two := &replTab{name: "two", parent: root, parentName: root.name, model: newReplModel()}
	r.tabs = append(r.tabs, one, two)
	one.model.busy, one.model.state = true, turnStateStreaming
	if text, color := r.agentsStatus(); text != "1 agent running" || color != "run" {
		t.Fatalf("one running agent = %q %q", text, color)
	}
	two.model.busy, two.model.state = true, turnStateTool
	if text, _ := r.agentsStatus(); text != "2 agents running" {
		t.Fatalf("two running agents = %q", text)
	}
	two.model.approval = &approvalState{reply: make(chan []bool, 1)}
	text, color := r.agentsStatus()
	if text != "1 needs approval" || color != "active" {
		t.Fatalf("waiting agent = %q %q", text, color)
	}
	m := r.model
	m.status.agents, m.status.agentsColor = text, color
	row := m.statusRow(120)
	plain := plainStyledText(row)
	if !strings.HasSuffix(plain, "gpt-mini · root · 1 needs approval") || !strings.Contains(row, styled(text, "active", "")) {
		t.Fatalf("status row = %q", plain)
	}
	f := m.status.agentsField
	if cells := []rune(plain); f.Cols != len(text) || string(cells[f.X:f.X+f.Cols]) != text || !f.hit(f.X, 23, 24) || f.hit(f.X-1, 23, 24) {
		t.Fatalf("agents field %+v does not cover %q in %q", f, text, plain)
	}
	// The field gives way before the session name does.
	if narrow := plainStyledText(m.statusRow(20)); strings.Contains(narrow, "approval") || !strings.Contains(narrow, "root") {
		t.Fatalf("narrow status row = %q", narrow)
	}
	if m.status.agentsField.Cols != 0 {
		t.Fatal("dropped field kept a hitbox")
	}
}

func TestCtrlGPreselectsAgentNeedingApproval(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	if err := testAcquireSession(t, store, "root").Close(); err != nil {
		t.Fatal(err)
	}
	child, err := store.Acquire(context.Background(), "agent", sessions.AcquireOptions{Parent: "root"})
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	r := newTabTestREPL(t, store, "root")
	root := r.tabs[0]
	tab := &replTab{name: "agent", parent: root, parentName: root.name, model: newReplModel()}
	r.tabs = append(r.tabs, tab)
	r.model.ed.setText("draft")
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-g>"})
	m := r.model.modal
	if m == nil || m.filteredItems()[m.selected].value != "root" {
		t.Fatalf("Ctrl-G without attention did not open on the current session: %#v", m)
	}
	r.closeModal()
	tab.model.approval = &approvalState{reply: make(chan []bool, 1)}
	r.handleEvent(ui.Event{Type: ui.KeyboardEvent, ID: "<C-g>"})
	m = r.model.modal
	if m == nil || m.filteredItems()[m.selected].value != "agent" {
		t.Fatalf("Ctrl-G did not open on the agent needing approval: %#v", m)
	}
	if r.model.ed.text() != "draft" {
		t.Fatal("opening the picker changed the composer")
	}
}
