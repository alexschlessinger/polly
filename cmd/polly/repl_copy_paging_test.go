package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

// Exposes the supplied native readers, but fails if copy tries a full view or
// acquires a lease. Hooks exercise replacement/deletion between bounded reads.
type copyPagingStore struct {
	sessions.SessionStore
	header func(context.Context, sessions.ViewTarget, string) (*sessions.SessionViewHeader, error)
	page   func(context.Context, sessions.ViewTarget, sessions.TranscriptQuery) (sessions.TranscriptPage, error)

	headers, views, acquires int
	queries                  []sessions.TranscriptQuery
	targets                  []sessions.ViewTarget
}

func (s *copyPagingStore) ReadViewHeader(ctx context.Context, target sessions.ViewTarget, revision string) (*sessions.SessionViewHeader, error) {
	s.headers++
	if s.header != nil {
		return s.header(ctx, target, revision)
	}
	return s.SessionStore.(sessions.ViewHeaderStore).ReadViewHeader(ctx, target, revision)
}

func (s *copyPagingStore) ReadTranscriptPage(ctx context.Context, target sessions.ViewTarget, q sessions.TranscriptQuery) (sessions.TranscriptPage, error) {
	s.queries = append(s.queries, q)
	s.targets = append(s.targets, target)
	if s.page != nil {
		return s.page(ctx, target, q)
	}
	return s.SessionStore.(sessions.TranscriptPageStore).ReadTranscriptPage(ctx, target, q)
}

func (s *copyPagingStore) ReadView(context.Context, sessions.ViewTarget, string) (*sessions.SessionView, error) {
	s.views++
	return nil, errors.New("unexpected full view")
}

func (s *copyPagingStore) Acquire(context.Context, string, sessions.AcquireOptions) (sessions.Session, error) {
	s.acquires++
	return nil, errors.New("unexpected lease")
}

func TestCopyNativeBoundedOlderMessages(t *testing.T) {
	for _, older := range []int{16, 1024} {
		t.Run(fmt.Sprint(older), func(t *testing.T) {
			store := testOpenMemoryStore(t, nil)
			session := testAcquireSession(t, store, "copy-bounded")
			history := make([]messages.ChatMessage, older)
			for i := range history {
				history[i] = messages.ChatMessage{Role: messages.MessageRoleUser, Content: strings.Repeat("old", 100)}
			}
			answer := "\n**latest saved answer** 日本語\n```go\nx := 1\n```\n"
			history = append(history, messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: answer})
			history = append(history, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "next prompt"})
			testAddMessages(t, session, history)
			id := session.(sessions.ViewIdentity).ViewID()
			reader := &copyPagingStore{SessionStore: store}
			recorder := runtimemetrics.New()
			ctx := runtimemetrics.WithRecorder(t.Context(), recorder)
			tracked := &copyLegacySession{Session: session}
			text, err := (copyResponseSource{store: reader, session: tracked, target: sessions.ViewTarget{ID: id}}).read(ctx)
			if err != nil || text != answer {
				t.Fatalf("copy = %q, %v", text, err)
			}
			counts := recorder.Snapshot()
			selected := min(len(history), copyTranscriptBlock)
			if got := counts.Count(runtimemetrics.TranscriptMessagesRead); got != uint64(selected) {
				t.Fatalf("decoded %d messages, want %d (older=%d)", got, selected, older)
			}
			if reader.headers != 1 || len(reader.queries) != 1 || reader.views != 0 || reader.acquires != 0 || tracked.reads != 0 {
				t.Fatalf("unexpected reads: %+v, histories=%d", reader, tracked.reads)
			}
			q := reader.queries[0]
			if q.After != int64(len(history)-selected) || q.Through != int64(len(history)) || q.Limit != copyTranscriptBlock || q.MaxBytes != sessions.DefaultTranscriptPageBytes || q.Epoch == "" || reader.targets[0] != (sessions.ViewTarget{ID: id}) {
				t.Fatalf("unbounded/unpinned query: %+v, target=%+v", q, reader.targets[0])
			}
			t.Logf("older=%d selected=%d statements=%d rows=%d payload=%d (returned work, not VM index visits)", older, selected, counts.Count(runtimemetrics.SQLStatements), counts.Count(runtimemetrics.RowsRead), counts.Count(runtimemetrics.PayloadBytesRead))
		})
	}
}

func TestCopyNativeFullTrailingScan(t *testing.T) {
	for _, size := range []int{0, 16, 1024} {
		for _, haveAnswer := range []bool{false, true} {
			t.Run(fmt.Sprintf("size=%d/answer=%v", size, haveAnswer), func(t *testing.T) {
				store := testOpenMemoryStore(t, nil)
				session := testAcquireSession(t, store, "copy-trailing")
				var history []messages.ChatMessage
				if haveAnswer {
					history = append(history, messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "old answer"})
				}
				for i := 0; i < size; i++ {
					history = append(history, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "not an answer"})
				}
				testAddMessages(t, session, history)
				reader := &copyPagingStore{SessionStore: store}
				recorder := runtimemetrics.New()
				text, err := (copyResponseSource{store: reader, target: sessions.ViewTarget{ID: session.(sessions.ViewIdentity).ViewID()}}).read(runtimemetrics.WithRecorder(t.Context(), recorder))
				if haveAnswer {
					if err != nil || text != "old answer" {
						t.Fatalf("trailing scan = %q, %v", text, err)
					}
				} else if err == nil || !strings.Contains(err.Error(), "no completed response") {
					t.Fatalf("no answer = %q, %v", text, err)
				}
				if got := recorder.Snapshot().Count(runtimemetrics.TranscriptMessagesRead); got != uint64(len(history)) {
					t.Fatalf("trailing scan decoded %d, want all %d", got, len(history))
				}
				wantPages := (len(history) + copyTranscriptBlock - 1) / copyTranscriptBlock
				if reader.headers != 1 || len(reader.queries) != wantPages || reader.views != 0 || reader.acquires != 0 {
					t.Fatalf("unexpected reads: %+v", reader)
				}
				through := int64(len(history))
				for _, q := range reader.queries {
					if q.Through != through || q.After != max(int64(0), through-copyTranscriptBlock) {
						t.Fatalf("not backwards contiguous: %+v, through=%d", q, through)
					}
					through = q.After
				}
			})
		}
	}
}

func TestCopyNativeByteBudgetKeepsLatestWholeAnswer(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "copy-bytes")
	answer := "\n" + strings.Repeat("answer 日本語\n", sessions.DefaultTranscriptPageBytes/8) + "\n"
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleAssistant, Content: "earlier answer in the same block"},
		{Role: messages.MessageRoleUser, Content: strings.Repeat("prompt", sessions.DefaultTranscriptPageBytes/8)},
		{Role: messages.MessageRoleAssistant, Parts: []messages.ContentPart{{Type: "text", Text: answer}, {Type: "image_url", ImageURL: "data:image/png;base64," + strings.Repeat("A", sessions.DefaultTranscriptPageBytes)}}, StopReason: messages.StopReasonMaxTokens},
		{Role: messages.MessageRoleAssistant, Content: "not final", ToolCalls: []messages.ChatMessageToolCall{{ID: "call", Name: "bash"}}},
		{Role: messages.MessageRoleAssistant, TextBlocks: []messages.AssistantText{{Phase: messages.PhaseCommentary, Text: "commentary"}}},
		{Role: messages.MessageRoleAssistant, Reasoning: "private"},
		{Role: messages.MessageRoleAssistant, Content: "failed", Metadata: map[string]any{messages.MetadataKeyIsError: true}},
	}
	testAddMessages(t, session, history)
	reader := &copyPagingStore{SessionStore: store}
	oversized := 0
	reader.page = func(ctx context.Context, target sessions.ViewTarget, q sessions.TranscriptQuery) (sessions.TranscriptPage, error) {
		page, err := store.(sessions.TranscriptPageStore).ReadTranscriptPage(ctx, target, q)
		if page.Oversized {
			oversized++
			if len(page.Messages) != 1 || page.Messages[0].Parts[0].Text != answer || page.Messages[0].Parts[1].ImageURL != history[2].Parts[1].ImageURL {
				t.Fatal("page truncated answer/media")
			}
		}
		return page, err
	}
	got, err := (copyResponseSource{store: reader, target: sessions.ViewTarget{ID: session.(sessions.ViewIdentity).ViewID()}}).read(t.Context())
	if err != nil || got != answer || len(reader.queries) < 3 || oversized != 1 {
		t.Fatalf("copy bytes: answer length=%d want=%d err=%v pages=%d oversized=%d", len(got), len(answer), err, len(reader.queries), oversized)
	}
	for i, q := range reader.queries {
		if q.Through != int64(len(history)) || (i > 0 && q.After <= reader.queries[i-1].After) {
			t.Fatalf("byte pages did not advance inside captured block: %+v", reader.queries)
		}
	}
}

func TestCopyPagesRejectReadAndIdentityErrors(t *testing.T) {
	readErr := errors.New("read failed")
	for _, tc := range []struct {
		name   string
		header func(*sessions.SessionViewHeader) (*sessions.SessionViewHeader, error)
		page   func(*sessions.TranscriptPage) error
		want   error
	}{
		{name: "header error", header: func(*sessions.SessionViewHeader) (*sessions.SessionViewHeader, error) { return nil, readErr }, want: readErr},
		{name: "missing header", header: func(*sessions.SessionViewHeader) (*sessions.SessionViewHeader, error) { return nil, nil }},
		{name: "header identity", header: func(h *sessions.SessionViewHeader) (*sessions.SessionViewHeader, error) {
			h.ID = "other"
			return h, nil
		}},
		{name: "header epoch", header: func(h *sessions.SessionViewHeader) (*sessions.SessionViewHeader, error) { h.Epoch = ""; return h, nil }},
		{name: "header sequence", header: func(h *sessions.SessionViewHeader) (*sessions.SessionViewHeader, error) {
			h.Sequence = -1
			return h, nil
		}},
		{name: "page error", page: func(*sessions.TranscriptPage) error { return readErr }, want: readErr},
		{name: "replacement", page: func(*sessions.TranscriptPage) error { return sessions.ErrStaleTranscriptEpoch }, want: sessions.ErrStaleTranscriptEpoch},
		{name: "deletion", page: func(*sessions.TranscriptPage) error { return sessions.ErrSessionNotFound }, want: sessions.ErrSessionNotFound},
		{name: "page identity", page: func(p *sessions.TranscriptPage) error { p.ID = "other"; return nil }, want: sessions.ErrStaleTranscriptEpoch},
		{name: "page epoch", page: func(p *sessions.TranscriptPage) error { p.Epoch = "other"; return nil }, want: sessions.ErrStaleTranscriptEpoch},
		{name: "page first", page: func(p *sessions.TranscriptPage) error { p.First++; return nil }},
		{name: "page end", page: func(p *sessions.TranscriptPage) error { p.Through++; return nil }},
		{name: "page no progress", page: func(p *sessions.TranscriptPage) error { p.Next = 0; return nil }},
		{name: "page overrun", page: func(p *sessions.TranscriptPage) error { p.Next = 2; return nil }},
		{name: "page gap", page: func(p *sessions.TranscriptPage) error { p.Messages = nil; return nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &copyPagingStore{}
			reader.header = func(context.Context, sessions.ViewTarget, string) (*sessions.SessionViewHeader, error) {
				h := &sessions.SessionViewHeader{ID: "id", Epoch: "epoch", Sequence: 1}
				if tc.header != nil {
					return tc.header(h)
				}
				return h, nil
			}
			reader.page = func(context.Context, sessions.ViewTarget, sessions.TranscriptQuery) (sessions.TranscriptPage, error) {
				p := sessions.TranscriptPage{ID: "id", Epoch: "epoch", First: 0, Next: 1, Through: 1, Messages: []messages.ChatMessage{{Role: messages.MessageRoleAssistant, Content: "answer"}}}
				if tc.page != nil {
					err := tc.page(&p)
					return p, err
				}
				return p, nil
			}
			text, err := (copyResponseSource{store: reader, target: sessions.ViewTarget{ID: "id"}}).read(t.Context())
			if text != "" || err == nil || (tc.want != nil && !errors.Is(err, tc.want)) || reader.views != 0 || reader.acquires != 0 {
				t.Fatalf("read = %q, %v, views=%d leases=%d", text, err, reader.views, reader.acquires)
			}
		})
	}
}

func TestCopyNativePinsSnapshotAcrossChanges(t *testing.T) {
	for _, change := range []string{"append", "rename", "clear", "delete-reuse"} {
		t.Run(change, func(t *testing.T) {
			store := testOpenMemoryStore(t, nil)
			session := testAcquireSession(t, store, "original")
			testAddMessage(t, session, messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "original answer"})
			reader := &copyPagingStore{SessionStore: store}
			reader.header = func(ctx context.Context, target sessions.ViewTarget, revision string) (*sessions.SessionViewHeader, error) {
				header, err := store.(sessions.ViewHeaderStore).ReadViewHeader(ctx, target, revision)
				if err != nil {
					return nil, err
				}
				switch change {
				case "append":
					testAddMessage(t, session, messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "later answer"})
				case "rename":
					err = session.Rename(ctx, "renamed")
				case "clear":
					err = session.Clear(ctx)
				case "delete-reuse":
					if err = session.Close(); err != nil {
						t.Fatal(err)
					}
					if err = store.Delete(ctx, "original"); err != nil {
						t.Fatal(err)
					}
					replacement := testAcquireSession(t, store, "original")
					testAddMessage(t, replacement, messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "wrong session"})
				}
				if err != nil {
					t.Fatal(err)
				}
				return header, nil
			}
			text, err := (copyResponseSource{store: reader, target: sessions.ViewTarget{Name: "original"}}).read(t.Context())
			switch change {
			case "append", "rename":
				if err != nil || text != "original answer" {
					t.Fatalf("snapshot copy = %q, %v", text, err)
				}
			case "clear":
				if !errors.Is(err, sessions.ErrStaleTranscriptEpoch) || text != "" {
					t.Fatalf("replacement = %q, %v", text, err)
				}
			case "delete-reuse":
				if !errors.Is(err, sessions.ErrSessionNotFound) || text != "" {
					t.Fatalf("reused name = %q, %v", text, err)
				}
			}
			if len(reader.targets) != 1 || reader.targets[0] != (sessions.ViewTarget{ID: session.(sessions.ViewIdentity).ViewID()}) {
				t.Fatalf("page target was not pinned: %+v", reader.targets)
			}
		})
	}
}

type copyLegacyStore struct {
	sessions.SessionStore
	sessions.ViewStore
	reads int
}

func (s *copyLegacyStore) ReadView(ctx context.Context, target sessions.ViewTarget, revision string) (*sessions.SessionView, error) {
	s.reads++
	return s.ViewStore.ReadView(ctx, target, revision)
}

type copyLegacySession struct {
	sessions.Session
	reads int
}

func (s *copyLegacySession) GetHistory(ctx context.Context) ([]messages.ChatMessage, error) {
	s.reads++
	return s.Session.GetHistory(ctx)
}

func TestCopyLegacyFallbackOnlyForAbsentCapabilities(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "legacy")
	testAddMessage(t, session, messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "legacy answer"})
	id := session.(sessions.ViewIdentity).ViewID()
	for _, capability := range []string{"neither", "header only", "page only"} {
		t.Run(capability, func(t *testing.T) {
			legacy := &copyLegacyStore{SessionStore: store, ViewStore: store.(sessions.ViewStore)}
			var reader sessions.SessionStore = legacy
			switch capability {
			case "header only":
				reader = struct {
					*copyLegacyStore
					sessions.ViewHeaderStore
				}{legacy, store.(sessions.ViewHeaderStore)}
			case "page only":
				reader = struct {
					*copyLegacyStore
					sessions.TranscriptPageStore
				}{legacy, store.(sessions.TranscriptPageStore)}
			}
			for _, active := range []bool{false, true} {
				source := copyResponseSource{store: reader, target: sessions.ViewTarget{ID: id}}
				tracked := &copyLegacySession{Session: session}
				if active {
					source.session = tracked
				}
				text, err := source.read(t.Context())
				if err != nil || text != "legacy answer" || legacy.reads != 1 || (active && tracked.reads != 1) {
					t.Fatalf("legacy active=%v: text=%q err=%v views=%d histories=%d", active, text, err, legacy.reads, tracked.reads)
				}
			}
		})
	}
}
