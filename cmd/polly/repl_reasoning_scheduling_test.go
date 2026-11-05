package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

func TestReasoningCallbacksDeferMarkdownUntilPaint(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*gotuiTurnUI)
	}{
		{"pause", func(ui *gotuiTurnUI) { ui.ThinkingEnded() }},
		{"finish", func(ui *gotuiTurnUI) { ui.AppendAssistantText("answer") }},
		{"complete", func(ui *gotuiTurnUI) {
			ui.model.mu.Lock()
			defer ui.model.mu.Unlock()
			ui.model.completeThinkingTurn(true)
		}},
		{"retry-wait", func(ui *gotuiTurnUI) { ui.ModelRetry(llm.RetryEvent{Waiting: true}) }},
		{"attempt-start", func(ui *gotuiTurnUI) { ui.ModelAttemptStarted(1, 0) }},
		{"attempt-abort", func(ui *gotuiTurnUI) { ui.ModelAttemptAborted(0, 0, errors.New("disconnect")) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newReplModel()
			m.beginTurn("explain")
			ui := &gotuiTurnUI{model: m, config: &Config{}}
			ui.ShowThinking("**initial** thought.\n\nvisible **unfinished")
			record := m.currentReasoningRecord()
			version, revision := record.tailVersion, m.visual.revision
			tc.call(ui)
			if record.previewWidth != 0 || record.previewVersion != 0 || len(record.previewLines) != 0 || !record.dirty {
				t.Fatalf("callback rendered Markdown instead of scheduling paint: %+v", record)
			}
			if record.tailVersion != version || m.visual.revision == revision {
				t.Fatal("callback changed source text or failed to invalidate the display revision")
			}
			(conversationView{}).Rows(m, 80)
			if record.dirty || record.previewVersion != version || record.previewActive {
				t.Fatal("paint did not consume the paused/settled preview")
			}
			text := plainStyledText(m.transcript[record.transcriptIndex].text)
			if !strings.Contains(text, "initial thought.") || !strings.Contains(text, "visible **unfinished") || strings.Contains(text, "**initial**") {
				t.Fatalf("paint lost Markdown rendering or held settlement text: %q", text)
			}
			if record.unsaved && !strings.Contains(text, "not saved") {
				t.Fatalf("paint lost the unsaved label: %q", text)
			}
		})
	}
}

func TestReasoningPauseKeepsPaintedPreviewUntilNextPaint(t *testing.T) {
	m := newReplModel()
	m.beginTurn("explain")
	ui := &gotuiTurnUI{model: m, config: &Config{}}
	ui.ShowThinking("visible **unfinished")
	(conversationView{}).Rows(m, 40)
	record := m.currentReasoningRecord()
	before := strings.Join(record.previewLines, "\n")
	version := record.previewVersion
	// Held viewports used to enter layout from the callback as well. All
	// preview growth and its re-anchoring now wait for the next paint.
	m.followBottom = false
	ui.ThinkingEnded()
	if !record.dirty || !record.previewActive || record.previewVersion != version || strings.Join(record.previewLines, "\n") != before {
		t.Fatal("pause rewrote an already-painted Markdown preview")
	}
	(conversationView{}).Rows(m, 40)
	if record.dirty || record.previewActive || record.previewVersion != version || !strings.Contains(plainStyledText(strings.Join(record.previewLines, "\n")), "**unfinished") {
		t.Fatal("paint did not flush the held suffix without changing its source version")
	}
}

func TestReasoningRetryRestorationDefersMarkdownUntilPaint(t *testing.T) {
	m := newReplModel()
	m.beginTurn("explain")
	ui := &gotuiTurnUI{model: m, config: &Config{}}
	ui.ShowThinking("**accepted** reasoning")
	ui.ThinkingEnded()
	accepted := m.currentReasoningRecord()
	ui.ModelAttemptStarted(1, 0)
	ui.ShowThinking("**discarded** reasoning")
	ui.ModelAttemptAborted(1, 0, errors.New("disconnect"))
	for _, id := range m.reasoningOrder {
		record := m.reasoningRecords.get(id)
		if len(record.previewLines) != 0 || record.previewWidth != 0 || !record.dirty {
			t.Fatal("retry parsed Markdown while restoring or splitting reasoning")
		}
	}
	(conversationView{}).Rows(m, 80)
	if accepted.unsaved || !strings.Contains(plainStyledText(m.transcript[accepted.transcriptIndex].text), "accepted reasoning") {
		t.Fatal("accepted prefix was not restored and painted")
	}
	failed := m.reasoningRecords.get(m.reasoningOrder[len(m.reasoningOrder)-1])
	text := plainStyledText(m.transcript[failed.transcriptIndex].text)
	if failed == accepted || !failed.unsaved || !strings.Contains(text, "discarded reasoning") || !strings.Contains(text, "not saved") {
		t.Fatalf("discarded suffix lost its separate preview or label: %q", text)
	}
}

func TestReasoningHistoryAndChildProjectionsDeferMarkdownUntilPaint(t *testing.T) {
	info := &sessions.SessionView{ID: "child", Metadata: &sessions.Metadata{Name: "child"}, History: []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "explain"},
		{Role: messages.MessageRoleAssistant, Reasoning: "# Plan\n\n**restored** thought"},
	}}
	m := prepareChildDisplay(info, &Config{}, 80)
	cached := buildCachedChildView(info, m, 0)
	projected, err := (conversationView{}).Project(context.Background(), viewSource{model: cached.model}, viewState{})
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range []*replModel{m, cached.model, projected} {
		record := model.reasoningRecords.get(model.reasoningOrder[0])
		if len(record.previewLines) != 0 || record.previewWidth != 0 || !record.dirty {
			t.Fatal("history/child projection parsed reasoning before paint")
		}
	}
	rows := (conversationView{}).Rows(projected, 80)
	text := strings.Join(transcriptRowsText(rows), "\n")
	if !strings.Contains(text, "Plan") || !strings.Contains(text, "restored thought") || strings.Contains(text, "**restored**") {
		t.Fatalf("child paint did not materialize the pending Markdown preview: %q", text)
	}
}

// Separate model locks match a visible tab applying a theme while a background
// tab receives provider callbacks. Run with -race: before deferral, pause/finish
// read ui.StyleParserColorMap through RenderMutedRows while applyTheme wrote it.
func TestReasoningCallbacksConcurrentThemeApply(t *testing.T) {
	active := newManagedREPL(&Config{}, "active", 0, 0)
	background := newReplModel()
	background.beginTurn("explain")
	ui := &gotuiTurnUI{model: background, config: &Config{}}
	theme := style.DefaultTheme()
	var wg sync.WaitGroup
	wg.Add(1)
	start := make(chan struct{})
	go func() {
		defer wg.Done()
		<-start
		for range 200 {
			ui.ShowThinking("**next** thought.\n")
			ui.ThinkingEnded()
			ui.ShowThinking("**finished** thought.\n")
			ui.AppendAssistantText("answer")
			ui.ModelAttemptStarted(1, 0)
			ui.ShowThinking("**discarded** thought.\n")
			ui.ModelAttemptAborted(1, 0, errors.New("disconnect"))
			background.mu.Lock()
			background.completeThinkingTurn(true)
			background.mu.Unlock()
		}
	}()
	close(start)
	for range 200 {
		active.model.mu.Lock()
		active.applyTheme(theme)
		active.model.mu.Unlock()
	}
	wg.Wait()
	for _, id := range background.reasoningOrder {
		record := background.reasoningRecords.get(id)
		if len(record.previewLines) != 0 || record.previewWidth != 0 || !record.dirty {
			t.Fatal("background callbacks parsed Markdown without an event-loop paint")
		}
	}
}
