package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestContextReaderReopenCacheIsBoundedAndChecksTranscript(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "reopen-context")
	testAddMessages(t, session, []messages.ChatMessage{
		{Role: messages.MessageRoleSystem, Content: "persona", Metadata: map[string]any{"nested": map[string]any{"value": "original"}}},
		{Role: messages.MessageRoleUser, Content: strings.Repeat("retired text ", 20_000)},
		messages.Compaction{Summary: "summary"}.Message(),
		cacheMessage(100, 50, true),
	})
	state, config := &conversationState{session: session}, &Config{SchemaPath: "schema.json"}
	reader := &contextSnapshotReader{}
	first, err := reader.read(t.Context(), config, state)
	if err != nil {
		t.Fatal(err)
	}
	cache := reader.reopenCache()
	if cache == nil {
		t.Fatal("small request basis was not cached")
	}
	reader.history[0].Metadata["nested"].(map[string]any)["value"] = "changed"
	if got := cache.request.first[0].Metadata["nested"].(map[string]any)["value"]; got != "original" {
		t.Fatalf("reopen cache retained source transcript buffers: %v", got)
	}
	reopened := newContextSnapshotReader(cache)
	metric := runtimemetrics.New()
	ctx := runtimemetrics.WithRecorder(t.Context(), metric)
	got, err := reopened.read(ctx, config, state)
	if err != nil || !reflect.DeepEqual(got, first) || len(reopened.history) != 0 {
		t.Fatalf("reopen reused raw history or lost counts: got=%+v err=%v retained=%d", got, err, len(reopened.history))
	}
	count := metric.Snapshot()
	if count.Count(runtimemetrics.TranscriptMessagesRead) != 0 || count.Count(runtimemetrics.PayloadBytesRead) != 0 {
		t.Fatalf("unchanged reopen reread retired transcript: %+v", count)
	}
	testAddMessages(t, session, []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "next turn"}})
	got, err = reopened.read(t.Context(), config, state)
	fresh, freshErr := collectContextSnapshot(t.Context(), config, state, testSessionHistory(t, session))
	if err != nil || freshErr != nil || !reflect.DeepEqual(got, fresh) || reopened.restored {
		t.Fatalf("changed reopen was not reconstructed: got=%+v fresh=%+v errors=%v/%v", got, fresh, err, freshErr)
	}
	// Huge first-system messages and unbounded tool stubs must never be pinned
	// merely to make a later open faster.
	reopened.request.first[0].Content = strings.Repeat("x", 65<<10)
	if reopened.reopenCache() != nil {
		t.Fatal("oversized first system message was retained")
	}
	reopened.request.first[0].Content = "small"
	reopened.usage.agents = make([]contextAgentUsage, 129)
	cache = reopened.reopenCache()
	if cache == nil || len(cache.usage.agents) != 0 || len(cache.revisions) != 0 {
		t.Fatal("reopen cache retained an unbounded agent roster")
	}
}

func TestContextPopoverReopenUsesNumericCache(t *testing.T) {
	r := contextDashboardREPL(t, false)
	if r.state.contextStats == nil {
		t.Fatal("completed popover did not publish a reopen cache")
	}
	r.closeModal()
	r.openContextPopover()
	awaitContextSnapshot(t, r)
	if !r.model.modal.context.reader.restored || len(r.model.modal.context.reader.history) != 0 {
		t.Fatal("unchanged popover reopened with a full retained transcript")
	}
}
