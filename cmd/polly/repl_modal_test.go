package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
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
}

func TestModelPickerAppliesExistingSettingPath(t *testing.T) {
	cfg := &Config{Settings: Settings{Model: "anthropic/claude-sonnet-4-6", MaxHistoryTokens: 256_000}}
	r := newManagedREPL(cfg, "ctx", 0, 0)
	r.startupLogoVisible = true
	r.model.recordContextUsage(50_000, 156_000, false)

	if handled, quit := r.runCommand("/model"); !handled || quit || r.model.modal == nil {
		t.Fatalf("/model handled=%v quit=%v modal=%#v", handled, quit, r.model.modal)
	}
	if r.startupLogoVisible {
		t.Fatal("opening /model left the startup logo visible behind the modal")
	}
	r.openProviderModels("openai")
	r.applySelectedModel("openai/gpt-5.4")

	if cfg.Model != "openai/gpt-5.4" || r.model.modelName != cfg.Model {
		t.Fatalf("selected model config=%q status=%q", cfg.Model, r.model.modelName)
	}
	if r.model.contextUsed != 0 || r.model.contextLimit != cfg.MaxHistoryTokens {
		t.Fatalf("model switch retained stale context usage: %d/%d", r.model.contextUsed, r.model.contextLimit)
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

func TestResumePickerListsRecentSessionsAndRequestsRestart(t *testing.T) {
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
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}

	current := testAcquireSession(t, store, "current-work")
	r := newManagedREPL(&Config{}, "current-work", 0, 0)
	r.state = &conversationState{sessionStore: store, session: current}
	if handled, quit := r.runCommand("/resume"); !handled || quit || r.model.modal == nil {
		t.Fatalf("/resume handled=%v quit=%v modal=%#v", handled, quit, r.model.modal)
	}
	if r.model.modal.title != "Resume session" {
		t.Fatalf("modal title = %q", r.model.modal.title)
	}
	if r.model.modal.width != 60 {
		t.Fatalf("modal width = %d, want 60", r.model.modal.width)
	}
	if r.model.modal.maxRows != 14 || !r.model.modal.showCount {
		t.Fatalf("modal window = %d rows, count=%v", r.model.modal.maxRows, r.model.modal.showCount)
	}
	if got := modalWidthForTerminal(140, r.model.modal.width); got != 60 {
		t.Fatalf("rendered modal width = %d, want 60", got)
	}
	for _, item := range r.model.modal.items {
		if strings.Contains(item.label, "openai/") {
			t.Fatalf("resume row exposed model/provider: %q", item.label)
		}
		if strings.Contains(item.label, "ago") {
			t.Fatalf("resume row used verbose age: %q", item.label)
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
	if r.resumeContext != "older-work" {
		t.Fatalf("resume context = %q, want older-work", r.resumeContext)
	}
	select {
	case <-r.quit:
	default:
		t.Fatal("session selection did not request a managed REPL restart")
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
	m.modelName = "openai/gpt-5.4"
	m.contextName = "work"
	m.recordContextUsage(41_200, 156_000, false)
	wide := plainStyledText(m.statusRow(120))
	if !strings.Contains(wide, "ctx 41.2k/156k") {
		t.Fatalf("status = %q", wide)
	}
	m.recordContextUsage(12_300, 156_000, true)
	if got := plainStyledText(m.statusRow(120)); !strings.Contains(got, "ctx ~12.3k/156k") {
		t.Fatalf("estimated status = %q", got)
	}
}
