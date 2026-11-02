package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestAgentOverviewRecentThinkingUsesNewestRecordedTail(t *testing.T) {
	m := newReplModel()
	m.hydrateHistory([]messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "Earlier task"},
		{Role: messages.MessageRoleAssistant, Reasoning: "Earlier task thought", Content: "Earlier answer"},
		{Role: messages.MessageRoleUser, Content: "Current task"},
		{Role: messages.MessageRoleAssistant, Reasoning: "Older segment in this task", Content: "Actual update"},
		{Role: messages.MessageRoleAssistant, Reasoning: "Omitted beginning\nRecent first\nRecent second\nRecent third"},
	}, "agent")
	var cache agentOverviewCache
	got := renderAgentOverviewCached(m, 80, agentOverviewState{}, &cache)
	text := overviewText(got)
	for _, want := range []string{"Latest update", "Actual update", "Recent thinking\nRecent first\nRecent second\nRecent third"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
	for _, omitted := range []string{"Earlier task thought", "Older segment", "Omitted beginning", "More in Watch"} {
		if strings.Contains(text, omitted) {
			t.Fatalf("unexpected excerpt text %q: %s", omitted, text)
		}
	}
	if len(cache.thinking.rows) != agentOverviewThinkingRows {
		t.Fatalf("thinking preview is not bounded: %+v", cache.thinking)
	}
	foundWatch := false
	for _, action := range got.actions {
		foundWatch = foundWatch || action == "watch"
	}
	if foundWatch || len(got.rows) != len(got.actions) {
		t.Fatal("thinking excerpt added a Watch footer or misaligned actions")
	}
	if strings.Index(text, "Actual update") > strings.Index(text, "Recent thinking") {
		t.Fatal("thinking displaced the actual assistant update")
	}
}

func TestAgentOverviewThinkingDoesNotInventOrReusePreviousTask(t *testing.T) {
	m := newReplModel()
	m.appendUserPrompt("Current task")
	var cache agentOverviewCache
	assertAbsent := func(state agentOverviewState) {
		t.Helper()
		text := overviewText(renderAgentOverviewCached(m, 80, state, &cache))
		if strings.Contains(text, "Recent thinking") || strings.Contains(text, "Actual thought") || cache.thinking.record != nil && len(cache.thinking.rows) > 0 {
			t.Fatalf("invented or reused thinking: %s", text)
		}
	}
	assertAbsent(agentOverviewState{})
	m.appendThinking(" \n\t")
	assertAbsent(agentOverviewState{})
	m.appendThinking("Actual thought")
	assertAbsent(agentOverviewState{assignment: "Different task"})
	text := overviewText(renderAgentOverviewCached(m, 80, agentOverviewState{assignment: "Augmented brief", currentTranscript: true}, &cache))
	if !strings.Contains(text, "Actual thought") {
		t.Fatalf("verified execution lost its thinking: %s", text)
	}
	m.appendUserPrompt("Follow-up with no thinking yet")
	assertAbsent(agentOverviewState{currentTranscript: true})
	if cache.thinking.record != nil {
		t.Fatal("new turn retained the previous thinking cache")
	}
}

func TestAgentOverviewThinkingCacheTracksStreamWidthThemeAndIdentity(t *testing.T) {
	restoreStyleEpochTestTheme(t)
	m := newReplModel()
	m.appendUserPrompt("Task")
	m.appendThinking("[literal](fg:red) 中 e\u0301\n" + strings.Repeat("older words ", 80) + "\nNewest thought")
	record := m.currentReasoningRecord()
	m.refreshReasoningRecords(90)
	m.transcriptRows(90)
	before := *record
	var cache agentOverviewCache
	first := renderAgentOverviewCached(m, 45, agentOverviewState{}, &cache)
	cachedRows := cache.thinking.rows
	again := renderAgentOverviewCached(m, 45, agentOverviewState{}, &cache)
	if !reflect.DeepEqual(first, again) || &cachedRows[0][0] != &cache.thinking.rows[0][0] {
		t.Fatal("unchanged thinking was rewrapped")
	}
	if !reflect.DeepEqual(before, *record) || m.visual.width != 90 {
		t.Fatal("overview changed Watch's reasoning disclosure, preview, or geometry")
	}
	m.appendThinking("\nNewest streamed thought")
	updated := overviewText(renderAgentOverviewCached(m, 45, agentOverviewState{}, &cache))
	if !strings.Contains(updated, "Newest streamed thought") || cache.thinking.version != record.tailVersion {
		t.Fatalf("streamed thinking did not refresh: %s", updated)
	}
	for _, width := range []int{12, 45, 90} {
		got := renderAgentOverviewCached(m, width, agentOverviewState{}, &cache)
		want := renderAgentOverview(m, width, agentOverviewState{})
		if !reflect.DeepEqual(got, want) || len(cache.thinking.rows) > agentOverviewThinkingRows {
			t.Fatalf("cached thinking differs at width %d", width)
		}
	}
	oldColor := cache.thinking.rows[0][0].Style.Fg
	style.Apply(style.Theme{Name: "thinking-preview", Colors: map[string]string{"muted": "#123abc"}})
	renderAgentOverviewCached(m, 90, agentOverviewState{}, &cache)
	if cache.thinking.epoch != style.Epoch() || cache.thinking.rows[0][0].Style.Fg == oldColor {
		t.Fatal("theme change left stale thinking colors")
	}
	// A refreshed projection can reuse numeric IDs and versions with different
	// content. Its independently owned record must not reuse the old preview.
	other := cloneReasoningRecord(record)
	other.tail = []rune("Different snapshot")
	rows := cache.thinking.render(other, 90)
	if strings.Join(transcriptRowsText(rows), "\n") != "Different snapshot" {
		t.Fatal("new snapshot reused stale thinking")
	}
}

func TestAgentOverviewThinkingRendersLiteralText(t *testing.T) {
	m := newReplModel()
	m.appendUserPrompt("Task")
	m.appendThinking("[literal](fg:red) 中 e\u0301 " + string(style.ImageMarker(0)))
	text := overviewText(renderAgentOverview(m, 80, agentOverviewState{}))
	if !strings.Contains(text, "[literal](fg:red) 中 e\u0301") || strings.ContainsRune(text, style.ImageMarker(0)) || strings.Contains(text, "More in Watch") {
		t.Fatalf("thinking text was interpreted as markup/media: %q", text)
	}
}
