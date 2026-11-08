package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
	"github.com/alexschlessinger/pollytool/sessions"
)

func TestSessionsPickerNativeBoundedOpenPaintRefreshAndCompleteSearch(t *testing.T) {
	for _, size := range []int{128, 512} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			store := testOpenMemoryStore(t, nil)
			ctx := t.Context()
			for i := range size {
				saved := testAcquireSession(t, store, fmt.Sprintf("saved-%04d", i))
				if i == 0 {
					md, err := saved.GetMetadata(ctx)
					if err != nil {
						t.Fatal(err)
					}
					md.Description = "distant NeedLe"
					if err := saved.SetMetadata(ctx, md); err != nil {
						t.Fatal(err)
					}
				}
				if err := saved.Close(); err != nil {
					t.Fatal(err)
				}
				child, err := store.Acquire(ctx, fmt.Sprintf("child-%04d", i), sessions.AcquireOptions{Parent: "saved-0000"})
				if err != nil {
					t.Fatal(err)
				}
				md, err := child.GetMetadata(ctx)
				if err != nil {
					t.Fatal(err)
				}
				md.Description = strings.Repeat("unrelated child payload ", 1024) + "NeedLe"
				if err := child.SetMetadata(ctx, md); err != nil {
					t.Fatal(err)
				}
				if err := child.Close(); err != nil {
					t.Fatal(err)
				}
			}
			r := newTabTestREPL(t, store, "current")
			counted := &countedHostSummaries{SessionStore: store, pages: store.(sessions.SummaryPageStore)}
			r.state.sessionStore = counted
			recorder := runtimemetrics.New()
			r.work.ctx = runtimemetrics.WithRecorder(r.work.ctx, recorder)
			r.openSessionsPicker()
			if len(r.model.modal.items) != 1 || r.model.modal.items[0].identity != r.visibleTab().viewID() {
				t.Fatal("open did not immediately show current workspace")
			}
			waitSessionsPicker(t, r)
			p, m := r.sessionsPicker, r.model.modal
			if len(counted.queries) != 1 || counted.listed != 0 || len(m.items) != sessionsPickerPageSize || p.done {
				t.Fatalf("open drained catalogue: pages=%d listed=%d rows=%d done=%t", len(counted.queries), counted.listed, len(m.items), p.done)
			}
			counts := recorder.Snapshot()
			if counts.Count(runtimemetrics.SummariesRead) != sessionsPickerPageSize || counts.Count(runtimemetrics.RowsRead) != 2*sessionsPickerPageSize+1 || counts.Count(runtimemetrics.WriteTransactions) != 0 {
				t.Fatalf("native open work grew with unrelated roots/children: %+v", counts)
			}
			// Paint/filter/navigation reuse descriptors and filtered slices; no
			// storage calls or catalogue styling is hidden behind a paint.
			m.visible = 10
			first := &m.items[0]
			for range 50 {
				m.refresh()
				m.text(10, 96)
				if &m.items[0] != first {
					t.Fatal("paint rebuilt catalogue")
				}
			}
			if len(counted.queries) != 1 || p.listing {
				t.Fatal("paint read storage")
			}
			before := recorder.Snapshot()
			p.listedAt = time.Time{}
			m.refresh()
			waitSessionsPicker(t, r)
			after := recorder.Snapshot()
			if after.Count(runtimemetrics.SummariesRead)-before.Count(runtimemetrics.SummariesRead) > 2*sessionsPickerPageSize || len(counted.queries) != 2 {
				t.Fatal("refresh drained loaded catalogue")
			}
			m.input.setText("NEEDLE")
			m.refresh()
			if m.titleNotice != "searching…" {
				t.Fatal("partial search was not identified")
			}
			waitSessionsPicker(t, r)
			matches := m.filteredItems()
			if len(matches) != 1 || matches[0].value != "saved-0000" || !p.searchDone {
				t.Fatalf("off-page root description not found: %+v done=%t", matches, p.searchDone)
			}
			for _, query := range counted.queries {
				if query.ParentID == nil || *query.ParentID != "" || query.Limit != sessionsPickerPageSize {
					t.Fatalf("unbounded/nonroot selector: %+v", query)
				}
			}
			if recorder.Snapshot().Count(runtimemetrics.TranscriptMessagesRead) != 0 || counted.listed != 0 {
				t.Fatal("picker loaded transcripts or global summaries")
			}
			// No total cap: scrolling to the current frontier eventually reaches
			// every saved root, without exposing or acquiring children.
			m.input.clear()
			for !p.done {
				m.selected, m.visible = len(m.items)-1, 10
				m.refresh()
				waitSessionsPicker(t, r)
			}
			if len(m.items) != size+1 {
				t.Fatalf("picker capped total: %d", len(m.items))
			}
		})
	}
}

func TestSessionsPickerLegacyListsOnceAndKeepsRootParity(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	for i := range sessionsPickerPageSize + 3 {
		if err := testAcquireSession(t, store, fmt.Sprintf("saved-%03d", i)).Close(); err != nil {
			t.Fatal(err)
		}
	}
	child, err := store.Acquire(t.Context(), "child", sessions.AcquireOptions{Parent: "saved-000"})
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	calls := 0
	legacy := pickerListingStore{SessionStore: store, list: func(ctx context.Context) ([]sessions.SessionSummary, error) {
		calls++
		return store.ListSummaries(ctx)
	}}
	r := newTabTestREPL(t, store, "current")
	r.state.sessionStore = legacy
	r.openSessionsPicker()
	waitSessionsPicker(t, r)
	if calls != 1 || len(r.model.modal.items) != sessionsPickerPageSize+4 || !r.sessionsPicker.done {
		t.Fatalf("legacy paging repeated listing/capped rows: calls=%d rows=%d", calls, len(r.model.modal.items))
	}
	r.model.modal.input.setText("saved-000")
	r.model.modal.refresh()
	waitSessionsPicker(t, r)
	if calls != 2 || len(r.model.modal.filteredItems()) != 1 {
		t.Fatal("legacy search lost rows")
	}
}

func TestSessionsPickerCancelsCloseReplacementAndQuery(t *testing.T) {
	for _, action := range []string{"close", "replace", "query"} {
		t.Run(action, func(t *testing.T) {
			store := testOpenMemoryStore(t, nil)
			r := newTabTestREPL(t, store, "current")
			entered, returned := make(chan struct{}), make(chan struct{})
			r.state.sessionStore = pickerListingStore{SessionStore: store, list: func(ctx context.Context) ([]sessions.SessionSummary, error) {
				close(entered)
				<-ctx.Done()
				defer close(returned)
				// Deliberately ignores cancellation: neither the result nor its
				// apparent success may revive the closed/superseded picker.
				return []sessions.SessionSummary{{ID: "late", Metadata: &sessions.Metadata{Name: "late"}}}, nil
			}}
			r.openSessionsPicker()
			<-entered
			old, p := r.model.modal, r.sessionsPicker
			if len(old.items) != 1 || old.items[0].value != "current" {
				t.Fatal("blocked listing delayed initial UI")
			}
			switch action {
			case "close":
				r.closeModal()
			case "replace":
				r.openModal(&replModal{title: "replacement"})
			case "query":
				r.state.sessionStore = store
				old.input.setText("current")
				old.refresh()
				waitSessionsPicker(t, r)
			}
			<-returned
			r.work.wg.Wait()
			for {
				select {
				case task := <-r.uiTasks:
					task()
				default:
					if _, ok := p.infos["late"]; ok {
						t.Fatal("late result crossed cancellation fence")
					}
					if action == "query" && (p.query != "current" || len(old.filteredItems()) != 1) {
						t.Fatal("new query lost its result")
					}
					return
				}
			}
		})
	}
}

func TestReadPickerBatchCancellationFence(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	legacy := pickerListingStore{SessionStore: store, list: func(context.Context) ([]sessions.SessionSummary, error) {
		cancel()
		return []sessions.SessionSummary{{ID: "late", Metadata: &sessions.Metadata{Name: "late"}}}, nil
	}}
	if _, err := readPickerBatch(ctx, legacy, "", nil, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("late legacy success: %v", err)
	}
}

func TestSessionsPickerNativeRenameDeleteAndOffPagePreference(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	saved := testAcquireSession(t, store, "saved")
	id := saved.(sessions.ViewIdentity).ViewID()
	if err := saved.Close(); err != nil {
		t.Fatal(err)
	}
	for i := range sessionsPickerPageSize + 2 {
		if err := testAcquireSession(t, store, fmt.Sprintf("newer-%03d", i)).Close(); err != nil {
			t.Fatal(err)
		}
	}
	r := newTabTestREPL(t, store, "current")
	// The supplied paging capability deliberately has no header/name reader.
	counted := &countedHostSummaries{SessionStore: store, pages: store.(sessions.SummaryPageStore)}
	r.state.sessionStore = counted
	r.openSessionsPickerSelected(id)
	waitSessionsPicker(t, r)
	m, p := r.model.modal, r.sessionsPicker
	if pickerSelection(m) != id {
		t.Fatal("off-page identity preference lost")
	}
	renamed := testAcquireSession(t, store, "saved")
	if err := renamed.Rename(t.Context(), "renamed"); err != nil {
		t.Fatal(err)
	}
	if err := renamed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := testAcquireSession(t, store, "saved").Close(); err != nil {
		t.Fatal(err)
	}
	p.listedAt = time.Time{}
	m.refresh()
	waitSessionsPicker(t, r)
	if pickerSelection(m) != id || m.selectedValue() != "renamed" {
		t.Fatal("rename/reused handle moved selection")
	}
	if err := store.Delete(t.Context(), "renamed"); err != nil {
		t.Fatal(err)
	}
	p.listedAt = time.Time{}
	m.refresh()
	waitSessionsPicker(t, r)
	if _, ok := p.infos[id]; ok {
		t.Fatal("deleted visible identity survived refresh")
	}
	if _, ok := p.itemIndex[id]; ok {
		t.Fatal("deleted identity survived item index")
	}
}

func TestSessionsPickerSelectedIdentitySurvivesReusedNameAndWipe(t *testing.T) {
	p := &sessionsPicker{infos: map[string]sessions.SessionSummary{
		"old": {ID: "old", Metadata: &sessions.Metadata{Name: "same"}},
		"new": {ID: "new", Metadata: &sessions.Metadata{Name: "same"}},
	}, byName: map[string]string{"same": "new"}}
	m := &replModal{items: []replModalItem{{value: "same", identity: "old"}, {value: "same", identity: "new"}}}
	p.modal = m
	m.onWipe = func() { p.closedSelection = pickerSelection(m) }
	m.wipe()
	if got, _ := p.named("same"); got.ID != "old" {
		t.Fatal("reused name redirected selection")
	}
}
