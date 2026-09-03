package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
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
	cfg := &Config{Settings: Settings{Model: "anthropic/claude-sonnet-4-6", MaxHistoryTokens: 256_000}}
	r := newManagedREPL(cfg, "ctx", 0, 0)
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

	if cfg.Model != "openai/gpt-5.4" || r.model.status.modelName != cfg.Model {
		t.Fatalf("selected model config=%q status=%q", cfg.Model, r.model.status.modelName)
	}
	if r.model.status.contextUsed != 0 || r.model.status.contextLimit != cfg.MaxHistoryTokens {
		t.Fatalf("model switch retained stale context usage: %d/%d", r.model.status.contextUsed, r.model.status.contextLimit)
	}
	if got := r.model.status.recentModels; len(got) == 0 || got[0] != "openai/gpt-5.4" {
		t.Fatalf("recent models after picker = %v, want the selection first", got)
	}
}

// Every model-change entry point shares one apply path, so /set model must
// feed the picker's recent list exactly as a picker selection does.
func TestSetModelRemembersRecentModel(t *testing.T) {
	cfg := &Config{Settings: Settings{Model: "anthropic/claude-sonnet-4-6", MaxHistoryTokens: 256_000}}
	r := newManagedREPL(cfg, "ctx", 0, 0)

	if handled, quit := r.runCommand("/set model openai/gpt-5.4"); !handled || quit {
		t.Fatalf("/set model handled=%v quit=%v", handled, quit)
	}
	if cfg.Model != "openai/gpt-5.4" || r.model.status.modelName != cfg.Model {
		t.Fatalf("set model config=%q status=%q", cfg.Model, r.model.status.modelName)
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

func TestResumePickerListsRecentSessionsAndSwitchesInPlace(t *testing.T) {
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

	current := testAcquireSession(t, store, "current-work")
	r := newManagedREPL(&Config{}, "current-work", 0, 0)
	r.state = &conversationState{sessionStore: store, session: current}
	r.opener = testSessionOpener(store)
	status := r.model.statusRow(80)
	if !strings.Contains(status, "[current-work](fg:accent)") {
		t.Fatalf("clickable session was not accented: %q", status)
	}
	placement := r.model.status.sessionField
	if !placement.hit(placement.X, 23, 24) || placement.hit(placement.X-1, 23, 24) {
		t.Fatalf("status session placement = %#v", placement)
	}
	r.handleEvent(ui.Event{Type: ui.MouseEvent, ID: "<MouseLeft>", Payload: ui.Mouse{X: placement.X, Y: 23}})
	if r.model.modal == nil {
		t.Fatal("clicking the status session did not open /resume")
	}
	if r.model.modal.title != "Resume session" {
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
	if r.switchTarget != "older-work" || !r.switchInFlight {
		t.Fatalf("selection did not start a switch: target=%q inFlight=%v", r.switchTarget, r.switchInFlight)
	}
	select {
	case <-r.quit:
		t.Fatal("session selection tore the managed REPL down instead of switching in place")
	default:
	}
	r.finishSwitch(<-r.switchDone)
	if r.switchTarget != "" || r.switchInFlight {
		t.Fatalf("switch did not settle: target=%q inFlight=%v", r.switchTarget, r.switchInFlight)
	}
	if name, err := r.state.session.GetName(context.Background()); err != nil || name != "older-work" {
		t.Fatalf("live session = %q, %v; want older-work", name, err)
	}
	if r.model.status.contextName != "older-work" {
		t.Fatalf("status context = %q, want older-work", r.model.status.contextName)
	}
	if transcript := r.model.fullTranscript(); !strings.Contains(transcript, "question") || !strings.Contains(transcript, "answer") {
		t.Fatalf("switched transcript was not hydrated: %q", transcript)
	}
	if current.Context().Err() == nil {
		t.Fatal("previous session was left open after the switch")
	}
	summaries, err := store.ListSummaries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, summary := range summaries {
		if summary.Metadata.Name == "current-work" && summary.InUse {
			t.Fatal("previous session still holds its lease after the switch")
		}
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

	r.openResumePicker()
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
	if footer := lines[len(lines)-1]; footer != "    1–14 of 20 · type filter · ↑↓ · Enter resume · Esc" {
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
