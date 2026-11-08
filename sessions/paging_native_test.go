package sessions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestNativePagingEpochsHeadersAndOwnership(t *testing.T) {
	store, path := openTestStore(t, ModeDisk, nil, 0)
	s := nativeHandle(t, store, "root")
	ref, err := s.ArtifactStore().Put(t.Context(), artifacts.Blob{Kind: artifacts.KindText, Data: []byte("retained artifact")})
	nativeMust(t, err)
	nativePut(t, s, "format", "swarm", `{"version":2}`)
	large := strings.Repeat("image", 10000)
	_, err = s.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
		return tx.Effects(CoordinationEffects{Append: []messages.ChatMessage{{Role: "user", Content: "first", Metadata: map[string]any{"nested": map[string]any{"value": "owned"}}}, {Role: "user", Parts: []messages.ContentPart{{Type: "image_base64", ImageData: large}}}, {Role: "assistant", Content: "last"}}, Transcript: &TranscriptIdentity{Execution: "e", Generation: 1}})
	})
	nativeMust(t, err)
	before := lastUsed(t, s)
	target := ViewTarget{ID: s.ViewID()}
	access, err := OpenTranscriptAccess(store)
	nativeMust(t, err)
	if access.Capabilities() != (TranscriptCapabilities{NativeIndexed: true, StableEpoch: true, NativeDisplay: true}) {
		t.Fatal(access.Capabilities())
	}
	first, err := access.ReadTranscriptPage(t.Context(), target, TranscriptQuery{Limit: 3, MaxBytes: 256})
	nativeMust(t, err)
	if first.Next != 1 || first.Through != 3 || first.Oversized || first.Transcript == nil || first.Transcript.Sequence != 3 {
		t.Fatalf("first %+v", first)
	}
	header, err := store.ReadViewHeader(t.Context(), target, "")
	nativeMust(t, err)
	full, err := store.ReadView(t.Context(), target, "")
	nativeMust(t, err)
	if header.Epoch != first.Epoch || header.Revision != full.Revision || header.Sequence != full.Sequence || !reflect.DeepEqual(header.Metadata, full.Metadata) || !header.InUse {
		t.Fatalf("header/full %+v %+v", header, full)
	}
	same, err := store.ReadViewHeader(t.Context(), target, header.Revision)
	nativeMust(t, err)
	if !same.Unchanged || same.Sequence != 3 || same.Epoch != first.Epoch {
		t.Fatal(same)
	}
	header.Metadata.Description = "mutated"
	first.Messages[0].Metadata["nested"].(map[string]any)["value"] = "mutated"
	query := TranscriptQuery{Epoch: first.Epoch, After: first.Next, Through: first.Through, Limit: 3, MaxBytes: 256}
	second, err := access.ReadTranscriptPage(t.Context(), target, query)
	nativeMust(t, err)
	if !second.Oversized || second.Next != 2 || len(second.Messages) != 1 || second.Messages[0].Parts[0].ImageData != large {
		t.Fatal("oversize truncated")
	}
	query.After = second.Next
	last, err := access.ReadTranscriptPage(t.Context(), target, query)
	nativeMust(t, err)
	if last.Next != 3 || last.Messages[0].Content != "last" {
		t.Fatal(last)
	}
	if !lastUsed(t, s).Equal(before) {
		t.Fatal("read touched LastUsed")
	}
	nativeMust(t, s.AddMessages(t.Context(), messages.User("append")))
	continued, err := access.ReadTranscriptPage(t.Context(), target, query)
	nativeMust(t, err)
	if continued.Epoch != first.Epoch || continued.Through != 3 || continued.Transcript.Sequence != 3 {
		t.Fatal("append invalidated captured range")
	}
	meta, err := s.GetMetadata(t.Context())
	nativeMust(t, err)
	meta.Description = "changed"
	nativeMust(t, s.SetMetadata(t.Context(), meta))
	changed, err := store.ReadViewHeader(t.Context(), target, header.Revision)
	nativeMust(t, err)
	if changed.Unchanged || changed.Epoch != header.Epoch || changed.Metadata.Description != "changed" {
		t.Fatal(changed)
	}
	again, err := access.ReadTranscriptPage(t.Context(), target, TranscriptQuery{Limit: 1})
	nativeMust(t, err)
	if again.Messages[0].Metadata["nested"].(map[string]any)["value"] != "owned" {
		t.Fatal("message alias")
	}
	if _, err := header.Artifacts.Put(t.Context(), artifacts.Blob{Data: []byte("forbidden")}); !errors.Is(err, ErrReadOnlyView) {
		t.Fatal(err)
	}
	nativeMust(t, s.Close())
	if got := string(readArtifact(t, header.Artifacts, ref.ID)); got != "retained artifact" {
		t.Fatal(got)
	}
	nativeMust(t, store.Close())
	reopened, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
	nativeMust(t, err)
	defer reopened.Close()
	persisted, err := reopened.ReadTranscriptPage(t.Context(), target, query)
	nativeMust(t, err)
	if persisted.Epoch != first.Epoch {
		t.Fatal("reopen changed epoch")
	}
	s = nativeHandle(t, reopened, "root")
	for _, reset := range []bool{false, true} {
		old, err := reopened.ReadTranscriptPage(t.Context(), target, TranscriptQuery{Limit: 1})
		nativeMust(t, err)
		if reset {
			nativeMust(t, s.Reset(t.Context(), &Metadata{SystemPrompt: "new system"}))
		} else {
			nativeMust(t, s.Clear(t.Context()))
		}
		nativeMust(t, s.AddMessages(t.Context(), messages.User("replacement")))
		if _, err = reopened.ReadTranscriptPage(t.Context(), target, TranscriptQuery{Epoch: old.Epoch, Limit: 1}); !errors.Is(err, ErrStaleTranscriptEpoch) {
			t.Fatal(err)
		}
		newHeader, err := reopened.ReadViewHeader(t.Context(), target, "")
		nativeMust(t, err)
		page, err := reopened.ReadTranscriptPage(t.Context(), target, TranscriptQuery{Limit: 1})
		nativeMust(t, err)
		if page.Epoch == old.Epoch || page.Epoch != newHeader.Epoch || page.Transcript != nil {
			t.Fatal("replacement epoch/identity")
		}
	}
}

func TestNativeSummaryLivePagesVisibilityAndCursors(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	root := nativeHandle(t, store, "root")
	var children []*sqliteSession
	for _, name := range []string{"z", "a", "b"} {
		child, err := store.Acquire(t.Context(), name, AcquireOptions{Parent: "root"})
		nativeMust(t, err)
		defer child.Close()
		children = append(children, child.(*sqliteSession))
	}
	other := nativeHandle(t, store, "other")
	_, err := store.db.Exec(`UPDATE sessions SET updated_ns=1000`)
	nativeMust(t, err)
	parent := root.ViewID()
	access, err := OpenSummaryAccess(store)
	nativeMust(t, err)
	if !access.Capabilities().NativeIndexed {
		t.Fatal("native capability absent")
	}
	first, err := access.ListSummaryPage(t.Context(), SummaryQuery{ParentID: &parent, Limit: 1})
	nativeMust(t, err)
	if len(first.Summaries) != 1 || first.Summaries[0].Metadata.Name != "a" || first.Next == "" {
		t.Fatal(first)
	}
	second, err := access.ListSummaryPage(t.Context(), SummaryQuery{ParentID: &parent, After: first.Next, Limit: 2})
	nativeMust(t, err)
	if len(second.Summaries) != 2 || second.Summaries[0].Metadata.Name != "b" || second.Summaries[1].Metadata.Name != "z" || second.Next != "" {
		t.Fatal(second)
	}
	roots := ""
	if _, err = access.ListSummaryPage(t.Context(), SummaryQuery{ParentID: &roots, After: first.Next, Limit: 1}); !errors.Is(err, ErrInvalidSummaryCursor) {
		t.Fatal(err)
	}
	rootsPage, err := access.ListSummaryPage(t.Context(), SummaryQuery{ParentID: &roots, Limit: 10})
	nativeMust(t, err)
	if len(rootsPage.Summaries) != 2 {
		t.Fatal(rootsPage)
	}
	nativeMust(t, root.Rename(t.Context(), "renamed"))
	nativeMust(t, children[0].Rename(t.Context(), "renamed-child"))
	summary, err := access.GetSummary(t.Context(), children[0].ViewID())
	nativeMust(t, err)
	if summary.Metadata.Name != "renamed-child" || summary.Metadata.Parent != "renamed" || summary.ParentID != parent {
		t.Fatal(summary)
	}
	summary.Metadata.Description = "mutated"
	summary, err = access.GetSummary(t.Context(), children[0].ViewID())
	nativeMust(t, err)
	if summary.Metadata.Description == "mutated" {
		t.Fatal("metadata aliases storage")
	}
	// Logical expiry hides only unleased, unpinned rows, without deleting them.
	nativeMust(t, other.Close())
	nativeMust(t, children[0].Close())
	_, err = root.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
		return tx.Effects(CoordinationEffects{Members: []string{children[0].ViewID()}})
	})
	nativeMust(t, err)
	_, err = store.db.Exec(`UPDATE sessions SET ttl_ns=1,updated_ns=1`)
	nativeMust(t, err)
	if _, err = access.GetSummary(t.Context(), other.ViewID()); !errors.Is(err, ErrSessionNotFound) {
		t.Fatal(err)
	}
	if _, err = access.GetSummary(t.Context(), children[0].ViewID()); err != nil {
		t.Fatal("pinned child hidden", err)
	}
	if _, err = access.GetSummary(t.Context(), root.ViewID()); err != nil {
		t.Fatal("leased root hidden", err)
	}
	if _, err = store.ReadTranscriptPage(t.Context(), ViewTarget{ID: other.ViewID()}, TranscriptQuery{Limit: 1}); !errors.Is(err, ErrSessionNotFound) {
		t.Fatal(err)
	}
	if _, err = store.ReadViewHeader(t.Context(), ViewTarget{ID: other.ViewID()}, ""); !errors.Is(err, ErrSessionNotFound) {
		t.Fatal(err)
	}
	var count int
	nativeMust(t, store.db.QueryRow(`SELECT count(*) FROM sessions WHERE id=?`, other.id).Scan(&count))
	if count != 1 {
		t.Fatal("read physically expired row")
	}
	nativeMust(t, store.Delete(t.Context(), "other"))
	replacement := nativeHandle(t, store, "other")
	if replacement.ViewID() == other.ViewID() {
		t.Fatal("ID reuse")
	}
	if _, err = access.GetSummary(t.Context(), other.ViewID()); !errors.Is(err, ErrSessionNotFound) {
		t.Fatal(err)
	}
}

func TestNativeTranscriptSelectedRangeValidation(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	s := nativeHandle(t, store, "root")
	nativeMust(t, s.AddMessages(t.Context(), []messages.ChatMessage{{Role: "user", Content: "a"}, {Role: "user", Content: "b"}, {Role: "user", Content: "c"}}))
	target := ViewTarget{ID: s.ViewID()}
	for _, q := range []TranscriptQuery{{Limit: 0}, {Limit: 1001}, {Limit: 1, After: -1}, {Limit: 1, After: 2, Through: 1}, {Limit: 1, Through: 4}, {Limit: 1, MaxBytes: -1}, {Limit: 1, MaxBytes: MaxTranscriptPageBytes + 1}} {
		if _, err := store.ReadTranscriptPage(t.Context(), target, q); err == nil {
			t.Fatal("accepted", q)
		}
	}
	_, err := store.db.Exec(`UPDATE messages SET payload_json=x'21' WHERE session_id=? AND sequence=2`, s.id)
	nativeMust(t, err)
	if _, err = store.ReadTranscriptPage(t.Context(), target, TranscriptQuery{Limit: 1}); err != nil {
		t.Fatal("decoded unselected corrupt row", err)
	}
	if _, err = store.ReadTranscriptPage(t.Context(), target, TranscriptQuery{After: 2, Limit: 1}); err == nil {
		t.Fatal("selected corrupt JSON accepted")
	}
	_, err = store.db.Exec(`DELETE FROM messages WHERE session_id=? AND sequence=1`, s.id)
	nativeMust(t, err)
	if _, err = store.ReadTranscriptPage(t.Context(), target, TranscriptQuery{Through: 2, Limit: 3}); err == nil {
		t.Fatal("gap accepted")
	}
	if _, err = store.ReadTranscriptPage(t.Context(), target, TranscriptQuery{After: 3, Limit: 1}); err != nil {
		t.Fatal("empty end", err)
	}
}

func TestRuntimeStorageScaleNativePages(t *testing.T) {
	for _, size := range []int{16, 1024} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			store, _ := openTestStore(t, ModeMemory, nil, 0)
			root := nativeHandle(t, store, "root")
			// Persist a growing archive without one live handle/goroutine per saved row.
			nativeMust(t, store.withWrite(t.Context(), func(conn *sql.Conn) error {
				for i := 0; i < size; i++ {
					if _, err := conn.ExecContext(t.Context(), `INSERT INTO sessions(id,name,retention,created_ns,updated_ns,settings_json,parent_id) VALUES(randomblob(16),?,'named',1,1,?,?)`, fmt.Sprintf("archive-%04d", i), []byte(`{}`), root.id); err != nil {
						return err
					}
				}
				return nil
			}))
			msgs := make([]messages.ChatMessage, size)
			for i := range msgs {
				msgs[i] = messages.ChatMessage{Role: "user", Content: strings.Repeat("x", 256)}
			}
			nativeMust(t, root.AddMessages(t.Context(), msgs))
			parent := root.ViewID()
			recorder := runtimemetrics.New()
			ctx := runtimemetrics.WithRecorder(t.Context(), recorder)
			page, err := store.ListSummaryPage(ctx, SummaryQuery{ParentID: &parent, Limit: 2})
			nativeMust(t, err)
			if len(page.Summaries) != 2 {
				t.Fatal(page)
			}
			counts := recorder.Snapshot()
			if counts.Count(runtimemetrics.SummariesRead) != 2 || counts.Count(runtimemetrics.RowsRead) != 5 || counts.Count(runtimemetrics.PayloadBytesRead) != 4 {
				t.Fatalf("summary selected work %+v", counts)
			}
			recorder = runtimemetrics.New()
			ctx = runtimemetrics.WithRecorder(t.Context(), recorder)
			transcript, err := store.ReadTranscriptPage(ctx, ViewTarget{ID: root.ViewID()}, TranscriptQuery{After: int64(size - 2), Limit: 2})
			nativeMust(t, err)
			tc := recorder.Snapshot()
			if len(transcript.Messages) != 2 || tc.Count(runtimemetrics.TranscriptMessagesRead) != 2 || tc.Count(runtimemetrics.RowsRead) != 5 {
				t.Fatal("transcript work grew", tc)
			}
			recorder = runtimemetrics.New()
			ctx = runtimemetrics.WithRecorder(t.Context(), recorder)
			header, err := store.ReadViewHeader(ctx, ViewTarget{ID: root.ViewID()}, "")
			nativeMust(t, err)
			hc := recorder.Snapshot()
			if header.Sequence != int64(size) || hc.Count(runtimemetrics.TranscriptMessagesRead) != 0 || hc.Count(runtimemetrics.RowsRead) != 1 {
				t.Fatal("header fetched history", hc)
			}
			t.Logf("coverage=native-page-statements/returned-rows/payload-bytes (not VM index visits); archive/transcript=%d summary statements=%d rows=%d payload=%d decoded_summaries=2; transcript statements=%d rows=%d payload=%d messages=2; header rows=%d transcript_messages=0", size, counts.Count(runtimemetrics.SQLStatements), counts.Count(runtimemetrics.RowsRead), counts.Count(runtimemetrics.PayloadBytesRead), tc.Count(runtimemetrics.SQLStatements), tc.Count(runtimemetrics.RowsRead), tc.Count(runtimemetrics.PayloadBytesRead), hc.Count(runtimemetrics.RowsRead))
		})
	}
}

func TestNativeReadSnapshotAcrossConnection(t *testing.T) {
	store, path := openTestStore(t, ModeDisk, nil, 0)
	s := nativeHandle(t, store, "root")
	nativePut(t, s, "custom", "x", `1`)
	observer, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
	nativeMust(t, err)
	defer observer.Close()
	nativeMust(t, observer.ReadRecordsView(t.Context(), s.ViewID(), func(r CoordinationReader) error {
		before, _, err := r.Get(RecordKey{"custom", "x"})
		nativeMust(t, err)
		nativePut(t, s, "custom", "x", `2`)
		after, _, err := r.Get(RecordKey{"custom", "x"})
		nativeMust(t, err)
		if string(after.Payload) != "1" || after.Revision != before.Revision {
			t.Fatal("snapshot changed", before, after)
		}
		return nil
	}))
	nativeMust(t, observer.ReadRecordsView(t.Context(), s.ViewID(), func(r CoordinationReader) error {
		v, _, err := r.Get(RecordKey{"custom", "x"})
		if string(v.Payload) != "2" {
			t.Fatal(v)
		}
		return err
	}))
	// A canceled read must neither invoke its callback nor acquire a lease.
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if err := observer.ReadRecordsView(ctx, s.ViewID(), func(CoordinationReader) error { t.Fatal("canceled callback"); return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}
