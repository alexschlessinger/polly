package sessions_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

// These adapters intentionally retain their result buffers. Paging must not
// mutate backend ordering or share returned nested values, discover a different
// backend, acquire a lease, or fall back after a backend error.
type legacyPagingStore struct {
	sessions.SessionStore
	rows      []sessions.SessionSummary
	err       error
	calls     int
	afterRead func()
}

func (s *legacyPagingStore) ListSummaries(context.Context) ([]sessions.SessionSummary, error) {
	s.calls++
	if s.afterRead != nil {
		s.afterRead()
	}
	return s.rows, s.err
}

type legacyViewPagingStore struct {
	legacyPagingStore
	view      *sessions.SessionView
	viewErr   error
	viewCalls int
	known     string
	target    sessions.ViewTarget
}

func (s *legacyViewPagingStore) ReadView(_ context.Context, target sessions.ViewTarget, known string) (*sessions.SessionView, error) {
	s.viewCalls++
	s.target, s.known = target, known
	if s.afterRead != nil {
		s.afterRead()
	}
	return s.view, s.viewErr
}

func TestSummaryPagingLegacyOrderingAndCursor(t *testing.T) {
	t.Parallel()
	stamp := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	row := func(id, name, parent string, used time.Time) sessions.SessionSummary {
		return sessions.SessionSummary{ID: id, ParentID: parent, Metadata: &sessions.Metadata{Name: name, LastUsed: used, ActiveSkills: []string{"owned"}}}
	}
	store := &legacyPagingStore{rows: []sessions.SessionSummary{
		row("d", "older", "parent", stamp.Add(-time.Second)),
		row("c", "same", "", stamp),
		row("b", "same", "parent", stamp),
		row("a", "newest", "parent", stamp.Add(time.Second)),
	}}
	access, err := sessions.OpenSummaryAccess(store)
	pagingMust(t, err)
	if access.Capabilities() != (sessions.SummaryCapabilities{}) {
		t.Fatal("slow fallback claimed indexed")
	}
	var ids []string
	q := sessions.SummaryQuery{Limit: 1}
	for {
		page, err := access.ListSummaryPage(t.Context(), q)
		pagingMust(t, err)
		for _, row := range page.Summaries {
			ids = append(ids, row.ID)
		}
		if page.Next == "" {
			break
		}
		q.After = page.Next
		// Limit changes are not filter changes.
		q.Limit = 2
	}
	if !reflect.DeepEqual(ids, []string{"a", "b", "c", "d"}) || store.calls != 3 || store.rows[0].ID != "d" {
		t.Fatalf("order/whole-result behavior: ids=%v calls=%d rows=%+v", ids, store.calls, store.rows)
	}
	parent := "parent"
	q = sessions.SummaryQuery{Limit: 1, ParentID: &parent}
	page, err := access.ListSummaryPage(t.Context(), q)
	pagingMust(t, err)
	if page.Next == "" {
		t.Fatal("missing continuation")
	}
	page.Summaries[0].Metadata.ActiveSkills[0] = "mutated"
	got, err := access.GetSummary(t.Context(), "a")
	pagingMust(t, err)
	if got.Metadata.ActiveSkills[0] != "owned" {
		t.Fatal("returned summary aliases backend metadata")
	}
	got.Metadata.ActiveSkills[0] = "mutated again"
	if store.rows[3].Metadata.ActiveSkills[0] != "owned" {
		t.Fatal("stable-ID lookup aliases backend metadata")
	}
	for _, filter := range []*string{nil, new(string), new("another-parent")} {
		if _, err := access.ListSummaryPage(t.Context(), sessions.SummaryQuery{Limit: 1, ParentID: filter, After: page.Next}); !errors.Is(err, sessions.ErrInvalidSummaryCursor) {
			t.Fatalf("mismatched filter accepted: %v", err)
		}
	}
	data, err := base64.RawURLEncoding.DecodeString(page.Next)
	pagingMust(t, err)
	for field, value := range map[string]any{"v": 99, "order": "name-desc", "id": ""} {
		var cursor map[string]any
		pagingMust(t, json.Unmarshal(data, &cursor))
		cursor[field] = value
		bad, err := json.Marshal(cursor)
		pagingMust(t, err)
		q.After = base64.RawURLEncoding.EncodeToString(bad)
		if _, err := access.ListSummaryPage(t.Context(), q); !errors.Is(err, sessions.ErrInvalidSummaryCursor) {
			t.Fatalf("invalid %s accepted: %v", field, err)
		}
	}
	for _, token := range []string{"not base64!", base64.RawURLEncoding.EncodeToString([]byte("null")), base64.RawURLEncoding.EncodeToString([]byte("{}"))} {
		q.After = token
		if _, err := access.ListSummaryPage(t.Context(), q); !errors.Is(err, sessions.ErrInvalidSummaryCursor) {
			t.Fatalf("malformed cursor accepted: %v", err)
		}
	}
	// A live keyset survives deletion of the boundary row without an offset.
	q.After = page.Next
	store.rows = store.rows[:3]
	next, err := access.ListSummaryPage(t.Context(), q)
	pagingMust(t, err)
	if len(next.Summaries) != 1 || next.Summaries[0].ID != "b" {
		t.Fatalf("deleted boundary: %+v", next)
	}
}

func TestPagingLegacyValidationAndErrors(t *testing.T) {
	t.Parallel()
	if _, err := sessions.OpenSummaryAccess(nil); !errors.Is(err, sessions.ErrPagingUnavailable) {
		t.Fatalf("nil summary store: %v", err)
	}
	if _, err := sessions.OpenTranscriptAccess(nil); !errors.Is(err, sessions.ErrPagingUnavailable) {
		t.Fatalf("nil transcript store: %v", err)
	}
	bare := &legacyPagingStore{}
	if _, err := sessions.OpenTranscriptAccess(bare); !errors.Is(err, sessions.ErrPagingUnavailable) {
		t.Fatalf("bare store silently acquired a session: %v", err)
	}
	store := &legacyViewPagingStore{view: &sessions.SessionView{ID: "id", Revision: "revision", Sequence: 1, History: messages.User("saved")}}
	summary, err := sessions.OpenSummaryAccess(store)
	pagingMust(t, err)
	transcript, err := sessions.OpenTranscriptAccess(store)
	pagingMust(t, err)
	target := sessions.ViewTarget{ID: "id"}
	for _, limit := range []int{-1, 0, sessions.MaxPageLimit + 1} {
		if _, err := summary.ListSummaryPage(t.Context(), sessions.SummaryQuery{Limit: limit}); !errors.Is(err, sessions.ErrInvalidPageRequest) {
			t.Fatalf("summary limit %d: %v", limit, err)
		}
		if _, err := transcript.ReadTranscriptPage(t.Context(), target, sessions.TranscriptQuery{Limit: limit}); !errors.Is(err, sessions.ErrInvalidPageRequest) {
			t.Fatalf("transcript limit %d: %v", limit, err)
		}
	}
	for _, bytes := range []int{-1, sessions.MaxTranscriptPageBytes + 1} {
		if _, err := transcript.ReadTranscriptPage(t.Context(), target, sessions.TranscriptQuery{Limit: 1, MaxBytes: bytes}); !errors.Is(err, sessions.ErrInvalidPageRequest) {
			t.Fatalf("byte limit %d: %v", bytes, err)
		}
	}
	if store.calls != 0 || store.viewCalls != 0 {
		t.Fatal("invalid limits touched backend")
	}
	for _, q := range []sessions.TranscriptQuery{{After: -1}, {Through: -1}, {After: 2, Through: 1}, {Through: 2}, {After: 2}} {
		q.Limit = 1
		if _, err := transcript.ReadTranscriptPage(t.Context(), target, q); !errors.Is(err, sessions.ErrInvalidTranscriptRange) {
			t.Fatalf("invalid range %+v: %v", q, err)
		}
	}
	page, err := transcript.ReadTranscriptPage(t.Context(), target, sessions.TranscriptQuery{After: 1, Limit: 1})
	pagingMust(t, err)
	if page.First != 1 || page.Next != 1 || page.Through != 1 || len(page.Messages) != 0 || store.target != target || store.known != "" {
		t.Fatalf("empty range/full-view request: %+v", page)
	}
	sentinel := errors.New("supplied backend failure")
	store.err, store.viewErr = sentinel, sentinel
	if _, err := summary.ListSummaryPage(t.Context(), sessions.SummaryQuery{Limit: 1}); !errors.Is(err, sentinel) {
		t.Fatalf("summary error lost: %v", err)
	}
	if _, err := summary.GetSummary(t.Context(), "id"); !errors.Is(err, sentinel) {
		t.Fatalf("lookup error lost: %v", err)
	}
	if _, err := transcript.ReadTranscriptPage(t.Context(), target, sessions.TranscriptQuery{Limit: 1}); !errors.Is(err, sentinel) {
		t.Fatalf("transcript error lost: %v", err)
	}
	store.err, store.viewErr = nil, nil
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	beforeCalls, beforeViews := store.calls, store.viewCalls
	if _, err := summary.ListSummaryPage(ctx, sessions.SummaryQuery{Limit: 1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("summary cancellation lost: %v", err)
	}
	if _, err := summary.GetSummary(ctx, "id"); !errors.Is(err, context.Canceled) {
		t.Fatalf("lookup cancellation lost: %v", err)
	}
	if _, err := transcript.ReadTranscriptPage(ctx, target, sessions.TranscriptQuery{Limit: 1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("transcript cancellation lost: %v", err)
	}
	if store.calls != beforeCalls || store.viewCalls != beforeViews {
		t.Fatal("canceled request touched backend")
	}
	for _, operation := range []func(context.Context) error{
		func(ctx context.Context) error {
			_, err := summary.ListSummaryPage(ctx, sessions.SummaryQuery{Limit: 1})
			return err
		},
		func(ctx context.Context) error { _, err := summary.GetSummary(ctx, "id"); return err },
		func(ctx context.Context) error {
			_, err := transcript.ReadTranscriptPage(ctx, target, sessions.TranscriptQuery{Limit: 1})
			return err
		},
	} {
		ctx, cancel := context.WithCancel(t.Context())
		store.afterRead = cancel
		err := operation(ctx)
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation during read lost: %v", err)
		}
	}
	store.afterRead = nil
	for _, rows := range [][]sessions.SessionSummary{{{ID: "id"}}, {{Metadata: &sessions.Metadata{Name: "name"}}}} {
		store.rows = rows
		if _, err := summary.ListSummaryPage(t.Context(), sessions.SummaryQuery{Limit: 1}); !errors.Is(err, sessions.ErrPagingUnavailable) {
			t.Fatalf("unidentified summary accepted: %v", err)
		}
	}
	for _, view := range []*sessions.SessionView{nil, {}, {ID: "id"}, {ID: "id", Revision: "revision", Unchanged: true}, {ID: "id", Revision: "revision", Sequence: 2, History: messages.User("gap")}} {
		store.view = view
		if _, err := transcript.ReadTranscriptPage(t.Context(), target, sessions.TranscriptQuery{Limit: 1}); err == nil {
			t.Fatalf("incomplete/corrupt view accepted: %+v", view)
		}
	}
}

func TestTranscriptPagingLegacyEpochAndOwnership(t *testing.T) {
	t.Parallel()
	store := &legacyViewPagingStore{view: &sessions.SessionView{
		ID: "id", Revision: "revision", Sequence: 2,
		Metadata: &sessions.Metadata{Transcript: &sessions.TranscriptIdentity{Execution: "exec", Generation: 1, Sequence: 2}},
		History:  []messages.ChatMessage{{Content: "first", Metadata: map[string]any{"nested": []any{"owned"}}}, {Content: "second"}},
	}}
	access, err := sessions.OpenTranscriptAccess(store)
	pagingMust(t, err)
	page, err := access.ReadTranscriptPage(t.Context(), sessions.ViewTarget{Name: "initial-name"}, sessions.TranscriptQuery{Limit: 1})
	pagingMust(t, err)
	if page.First != 0 || page.Next != 1 || page.Through != 2 {
		t.Fatalf("page boundaries: %+v", page)
	}
	page.Messages[0].Metadata["nested"].([]any)[0] = "mutated"
	page.Transcript.Execution = "mutated"
	if store.view.History[0].Metadata["nested"].([]any)[0] != "owned" || store.view.Metadata.Transcript.Execution != "exec" {
		t.Fatal("page aliases retained backend view")
	}
	q := sessions.TranscriptQuery{Epoch: page.Epoch, After: page.Next, Through: page.Through, Limit: 1}
	second, err := access.ReadTranscriptPage(t.Context(), sessions.ViewTarget{ID: page.ID}, q)
	pagingMust(t, err)
	if second.Next != 2 || second.Messages[0].Content != "second" || store.viewCalls != 2 {
		t.Fatalf("continuation failed: %+v; reads=%d", second, store.viewCalls)
	}
	store.view.Revision = "metadata-change"
	if _, err := access.ReadTranscriptPage(t.Context(), sessions.ViewTarget{ID: page.ID}, q); !errors.Is(err, sessions.ErrStaleTranscriptEpoch) {
		t.Fatalf("view revision change accepted: %v", err)
	}
	store.view.Revision, store.view.ID = "revision", "another-stable-id"
	if _, err := access.ReadTranscriptPage(t.Context(), sessions.ViewTarget{ID: store.view.ID}, q); !errors.Is(err, sessions.ErrStaleTranscriptEpoch) {
		t.Fatalf("cross-session epoch accepted: %v", err)
	}
	store.view.ID = "id"
	q.Epoch = "unknown-v99:revision"
	if _, err := access.ReadTranscriptPage(t.Context(), sessions.ViewTarget{ID: "id"}, q); !errors.Is(err, sessions.ErrStaleTranscriptEpoch) {
		t.Fatalf("unsupported epoch accepted: %v", err)
	}
}

type suppliedPagingStore struct {
	legacyViewPagingStore
	pageCalls, lookupCalls, transcriptCalls int
	nativeErr                               error
}

func (s *suppliedPagingStore) ListSummaryPage(context.Context, sessions.SummaryQuery) (sessions.SummaryPage, error) {
	s.pageCalls++
	return sessions.SummaryPage{}, s.nativeErr
}
func (s *suppliedPagingStore) GetSummary(context.Context, string) (*sessions.SessionSummary, error) {
	s.lookupCalls++
	return nil, s.nativeErr
}
func (s *suppliedPagingStore) ReadTranscriptPage(context.Context, sessions.ViewTarget, sessions.TranscriptQuery) (sessions.TranscriptPage, error) {
	s.transcriptCalls++
	return sessions.TranscriptPage{}, s.nativeErr
}

type reportedPagingStore struct{ suppliedPagingStore }

func (*reportedPagingStore) SummaryCapabilities() sessions.SummaryCapabilities {
	return sessions.SummaryCapabilities{NativeIndexed: true}
}
func (*reportedPagingStore) TranscriptCapabilities() sessions.TranscriptCapabilities {
	return sessions.TranscriptCapabilities{NativeIndexed: true, StableEpoch: true}
}

func TestPagingSelectsOnlySuppliedBackend(t *testing.T) {
	t.Parallel()
	for _, reported := range []bool{false, true} {
		sentinel := errors.New("supplied paging failure")
		store := &reportedPagingStore{}
		store.nativeErr = sentinel
		var supplied sessions.SessionStore = &store.suppliedPagingStore
		if reported {
			supplied = store
		}
		summary, err := sessions.OpenSummaryAccess(supplied)
		pagingMust(t, err)
		transcript, err := sessions.OpenTranscriptAccess(supplied)
		pagingMust(t, err)
		if summary.Capabilities().NativeIndexed != reported || transcript.Capabilities().NativeIndexed != reported || transcript.Capabilities().StableEpoch != reported {
			t.Fatal("invented/ignored supplied capability report")
		}
		if _, err := summary.ListSummaryPage(t.Context(), sessions.SummaryQuery{Limit: 1}); !errors.Is(err, sentinel) {
			t.Fatalf("supplied summary method bypassed: %v", err)
		}
		if _, err := summary.GetSummary(t.Context(), "id"); !errors.Is(err, sentinel) {
			t.Fatalf("supplied lookup bypassed: %v", err)
		}
		if _, err := transcript.ReadTranscriptPage(t.Context(), sessions.ViewTarget{ID: "id"}, sessions.TranscriptQuery{Limit: 1}); !errors.Is(err, sentinel) {
			t.Fatalf("supplied transcript method bypassed: %v", err)
		}
		if store.pageCalls != 1 || store.lookupCalls != 1 || store.transcriptCalls != 1 || store.calls != 0 || store.viewCalls != 0 {
			t.Fatalf("fallback/native selection called wrong methods: %+v", store)
		}
	}
}
