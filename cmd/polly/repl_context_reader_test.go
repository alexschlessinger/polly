package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

type gatedContextSession struct {
	sessions.Session
	entered chan struct{}
}

func (s *gatedContextSession) GetHistory(ctx context.Context) ([]messages.ChatMessage, error) {
	close(s.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestContextPopoverOpenDoesNotWaitForStorage(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	session := &gatedContextSession{Session: testAcquireSession(t, store, "gated-context"), entered: make(chan struct{})}
	r := newManagedREPL(&Config{SchemaPath: "schema.json"}, "gated-context", 0, 0)
	r.state = &conversationState{session: session}
	opened := make(chan struct{})
	go func() {
		r.model.mu.Lock()
		r.openContextPopover()
		r.model.mu.Unlock()
		close(opened)
	}()
	select {
	case <-opened:
	case <-time.After(5 * time.Second):
		_ = session.Close()
		t.Fatal("opening context blocked on storage under the UI lock")
	}
	d := r.model.modal.context
	if !d.loading || !strings.Contains(strings.Join(r.model.modal.details, "\n"), "Loading") {
		t.Fatal("opening did not expose an honest loading state")
	}
	if text := strings.Join(r.model.modal.details, "\n"); strings.Contains(text, "transcript: 0") || strings.Contains(text, "No subagents") {
		t.Fatalf("loading advertised empty counts: %s", text)
	}
	select {
	case <-session.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("background context read did not start")
	}
	r.closeModal()
	select {
	case result := <-d.pending:
		if !errors.Is(result.err, context.Canceled) {
			t.Fatalf("closed reader error = %v", result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("closing did not cancel storage read")
	}
}

func TestContextReaderNativeUnchangedAndAppendWork(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "incremental-context")
	history := []messages.ChatMessage{{Role: messages.MessageRoleSystem, Content: "persona"}}
	for i := 0; i < 600; i++ {
		history = append(history, cacheMessage(100, 50, true))
	}
	testAddMessages(t, session, history)
	state := &conversationState{session: session}
	config := &Config{SchemaPath: "schema.json"}
	reader := &contextSnapshotReader{}
	first, err := reader.read(t.Context(), config, state)
	if err != nil {
		t.Fatal(err)
	}
	metric := runtimemetrics.New()
	ctx := runtimemetrics.WithRecorder(t.Context(), metric)
	unchanged, err := reader.read(ctx, config, state)
	if err != nil || !reflect.DeepEqual(first, unchanged) {
		t.Fatalf("unchanged = %+v, err=%v", unchanged, err)
	}
	count := metric.Snapshot()
	if count.Count(runtimemetrics.TranscriptMessagesRead) != 0 || count.Count(runtimemetrics.PayloadBytesRead) != 0 || count.Count(runtimemetrics.RowsRead) > 2 || count.Count(runtimemetrics.TranscriptReads) != 1 {
		t.Fatalf("unchanged read scaled with transcript: %+v", count)
	}
	testAddMessages(t, session, []messages.ChatMessage{cacheMessage(200, 100, true)})
	metric = runtimemetrics.New()
	ctx = runtimemetrics.WithRecorder(t.Context(), metric)
	appended, err := reader.read(ctx, config, state)
	if err != nil {
		t.Fatal(err)
	}
	count = metric.Snapshot()
	if count.Count(runtimemetrics.TranscriptMessagesRead) != 1 || count.Count(runtimemetrics.TranscriptReads) != 1 {
		t.Fatalf("append reread retained transcript: %+v", count)
	}
	if appended.storedMessages != 602 || appended.usage.parent.input.tokens != 60_200 || appended.roles[1].count != 601 || appended.trend[len(appended.trend)-1].tokens != 200 {
		t.Fatalf("incremental counts = %+v", appended)
	}
	if first.storedMessages != 601 || first.trend[len(first.trend)-1].tokens != 100 {
		t.Fatal("append mutated published snapshot")
	}
}

func TestContextRequestSummaryImagesAppendIncrementally(t *testing.T) {
	for _, keepsTurn := range []bool{false, true} {
		t.Run(map[bool]string{false: "summary", true: "keeps turn"}[keepsTurn], func(t *testing.T) {
			reader := &contextSnapshotReader{}
			state, config := &conversationState{}, &Config{SchemaPath: "schema.json"}
			reader.appendHistory([]messages.ChatMessage{
				{Role: messages.MessageRoleSystem, Content: "persona"},
				{Role: messages.MessageRoleUser, Content: "inspect", Parts: []messages.ContentPart{{Type: "image_url", ImageURL: "https://example.test/image.png"}}},
				messages.Compaction{Summary: "image work", KeepsTurn: keepsTurn}.Message(),
			})
			if _, err := reader.snapshot(t.Context(), config, state); err != nil {
				t.Fatal(err)
			}
			for i, message := range []messages.ChatMessage{
				{Role: messages.MessageRoleAssistant, Content: "working"},
				{Role: messages.MessageRoleUser, Content: "admission", Metadata: map[string]any{messages.MetadataKeyAgentSynthetic: true}},
				{Role: messages.MessageRoleUser, Content: "next turn"},
				{Role: messages.MessageRoleAssistant, Content: "answer"},
				{Role: messages.MessageRoleUser, Content: "another turn"},
			} {
				first := &reader.request.first[0]
				reader.appendHistory([]messages.ChatMessage{message})
				got, err := reader.snapshot(t.Context(), config, state)
				want, fullErr := collectContextSnapshot(t.Context(), config, state, reader.history)
				if err != nil || fullErr != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("step %d incremental=%+v full=%+v errors=%v/%v", i, got, want, err, fullErr)
				}
				if rebuilt := &reader.request.first[0] != first; rebuilt != (i == 2) {
					t.Fatalf("step %d rebuilt=%t; only the first real user after summary needs retained projection", i, rebuilt)
				}
			}
		})
	}
}

func TestContextReaderProjectionMatchesCanonicalAcrossChanges(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "projection-context")
	state := &conversationState{session: session, settings: Settings{SystemPrompt: "persona"}}
	reader := &contextSnapshotReader{}
	config := &Config{}
	batches := [][]messages.ChatMessage{
		{{Role: messages.MessageRoleSystem, Content: "persona"}, {Role: messages.MessageRoleUser, Content: "first"}},
		{cacheMessage(200, 100, true), {Role: messages.MessageRoleTool, ToolCallID: "one", Content: strings.Repeat("old output ", 100)}},
		{messages.Compaction{ClearThrough: "one"}.Message()},
		{{Role: messages.MessageRoleUser, Content: "second"}, cacheMessage(300, 100, true)},
		{messages.Compaction{Summary: "earlier context", KeepsTurn: true}.Message()},
		{{Role: messages.MessageRoleUser, Content: "third"}, cacheMessage(400, 100, true)},
	}
	for _, batch := range batches {
		testAddMessages(t, session, batch)
		got, err := reader.read(t.Context(), config, state)
		if err != nil {
			t.Fatal(err)
		}
		history := testSessionHistory(t, session)
		view := llm.RequestView(history, state.viewTools())
		view, _, err = composeSessionContracts(t.Context(), state, &state.settings, view)
		if err != nil {
			t.Fatal(err)
		}
		req := llm.CompletionRequest{Messages: view, Skills: state.skillCatalog}
		view, err = llm.WithSandboxContext(req.ResolvedMessages(), state.toolRegistry)
		if err != nil {
			t.Fatal(err)
		}
		want := contextRoles()
		for _, msg := range view {
			addContextRole(want[:], msg)
		}
		if !reflect.DeepEqual(got.roles, want[:]) {
			t.Fatalf("incremental roles=%+v canonical=%+v", got.roles, want)
		}
		fresh, err := collectContextSnapshot(t.Context(), config, state, history)
		if err != nil || !reflect.DeepEqual(got, fresh) {
			t.Fatalf("incremental=%+v fresh=%+v error=%v", got, fresh, err)
		}
	}
	// Metadata/contracts may change without a transcript append.
	state.displayContract = richTerminalDisplayContract
	changed, err := reader.read(t.Context(), config, state)
	fresh, freshErr := collectContextSnapshot(t.Context(), config, state, testSessionHistory(t, session))
	if err != nil || freshErr != nil || !reflect.DeepEqual(changed, fresh) {
		t.Fatalf("contract change stale: changed=%+v fresh=%+v errors=%v/%v", changed, fresh, err, freshErr)
	}
	if err := session.Clear(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, err := reader.read(t.Context(), config, state)
	if err != nil || after.storedMessages != 0 || after.usage.parent.input.tokens != 0 || len(after.trend) != 0 {
		t.Fatalf("reset kept old aggregates: %+v err=%v", after, err)
	}
}
