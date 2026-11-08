package sessions

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
)

func TestTranscriptDisplayIndexedWindowsAndPrefixBudgets(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	s := nativeHandle(t, store, "display")
	msgs := []messages.ChatMessage{{Role: "system", Content: strings.Repeat("system", 1000)}, {Role: "user", Content: "one"}, {Role: "assistant", Reasoning: strings.Repeat("reasoning", 1000), ToolCalls: []messages.ChatMessageToolCall{{ID: "call", Name: "bash", Arguments: `{"command":"true"}`}}}, {Role: "tool", ToolCallID: "call", ToolName: "bash", Content: strings.Repeat("output", 10000)}, {Role: "user", Content: "mail", Metadata: map[string]any{messages.MetadataKeySwarmMessages: []string{"receipt"}}}, {Role: "user", Content: "two"}, {Role: "assistant", Content: "done"}}
	nativeMust(t, s.AddMessages(t.Context(), msgs))
	target := ViewTarget{ID: s.ViewID()}
	header, err := store.ReadViewHeader(t.Context(), target, "")
	nativeMust(t, err)
	window, err := store.ReadTranscriptWindow(t.Context(), target, TranscriptWindowQuery{Epoch: header.Epoch, Through: header.Sequence, Turns: 1})
	nativeMust(t, err)
	if window.Start != 5 || window.PreviousTurn != 1 {
		t.Fatalf("window %+v", window)
	}
	first, err := store.ReadTranscriptDisplayPage(t.Context(), target, TranscriptQuery{Epoch: header.Epoch, Through: window.Start, Limit: 10, MaxBytes: 1})
	nativeMust(t, err)
	if len(first.Facts) != 1 || !first.Oversized || first.Next != 3 || first.Facts[0].Message.Reasoning != "x" {
		t.Fatalf("first %+v", first)
	}
	second, err := store.ReadTranscriptDisplayPage(t.Context(), target, TranscriptQuery{Epoch: header.Epoch, After: first.Next, Through: window.Start, Limit: 10, MaxBytes: 1024})
	nativeMust(t, err)
	if len(second.Facts) != 1 || second.Next != window.Start || second.Facts[0].Message.Content != "x" || second.Facts[0].ContentHash == "" {
		t.Fatalf("second %+v", second)
	}
	first.Facts[0].Message.ToolCalls[0].ID = "mutated"
	again, err := store.ReadTranscriptDisplayPage(t.Context(), target, TranscriptQuery{Epoch: header.Epoch, Through: window.Start, Limit: 1})
	nativeMust(t, err)
	if again.Facts[0].Message.ToolCalls[0].ID != "call" {
		t.Fatal("prefix ownership leaked")
	}
	previous, err := store.ReadTranscriptPreviousUser(t.Context(), target, header.Epoch, window.Start)
	nativeMust(t, err)
	if previous.Sequence != 1 || previous.Message.Content != "one" {
		t.Fatalf("previous %+v", previous)
	}
	// Through=0 is an exact empty snapshot, unlike the transcript page API's
	// capture-current convention. An append racing an empty header stays empty.
	empty, err := store.ReadTranscriptWindow(t.Context(), target, TranscriptWindowQuery{Epoch: header.Epoch, Through: 0, Turns: 1})
	nativeMust(t, err)
	if empty.Start != 0 || empty.Through != 0 {
		t.Fatalf("empty snapshot advanced: %+v", empty)
	}
	nativeMust(t, s.Clear(t.Context()))
	_, err = store.ReadTranscriptWindow(t.Context(), target, TranscriptWindowQuery{Epoch: header.Epoch, Through: 0, Turns: 1})
	if !errors.Is(err, ErrStaleTranscriptEpoch) {
		t.Fatalf("stale empty epoch accepted: %v", err)
	}
}

func TestTranscriptDisplayWriteRollbackAndCascade(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	s := nativeHandle(t, store, "display")
	nativeMust(t, s.AddMessages(t.Context(), messages.User("first")))
	stop := errors.New("rollback")
	err := store.withWrite(t.Context(), func(conn *sql.Conn) error {
		if _, err := appendMessages(t.Context(), conn, s.id, 1, messages.User("rolled back")); err != nil {
			return err
		}
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatal(err)
	}
	var count int
	nativeMust(t, store.db.QueryRow(`SELECT count(*) FROM transcript_display WHERE session_id=?`, s.id).Scan(&count))
	if count != 1 {
		t.Fatalf("rollback left %d display rows", count)
	}
	nativeMust(t, s.Clear(t.Context()))
	nativeMust(t, store.db.QueryRow(`SELECT count(*) FROM transcript_display WHERE session_id=?`, s.id).Scan(&count))
	if count != 0 {
		t.Fatalf("reset left %d display rows", count)
	}
	nativeMust(t, s.AddMessages(t.Context(), messages.User("replacement")))
	header, err := store.ReadViewHeader(t.Context(), ViewTarget{ID: s.ViewID()}, "")
	nativeMust(t, err)
	// A missing selected boundary is corruption, never an empty window or an
	// invitation for the read path to rebuild history.
	_, err = store.db.Exec(`DELETE FROM transcript_display WHERE session_id=?`, s.id)
	nativeMust(t, err)
	_, err = store.ReadTranscriptWindow(t.Context(), ViewTarget{ID: s.ViewID()}, TranscriptWindowQuery{Epoch: header.Epoch, Through: header.Sequence, Turns: 1})
	if err == nil {
		t.Fatal("missing display boundary accepted")
	}
	nativeMust(t, store.db.QueryRow(`SELECT count(*) FROM transcript_display WHERE session_id=?`, s.id).Scan(&count))
	if count != 0 {
		t.Fatal("read repaired display index")
	}
}

func TestIndexValidationRejectsMissingIndex(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	_, err := store.db.Exec(`DROP INDEX transcript_display_facts_idx`)
	nativeMust(t, err)
	err = store.withRead(t.Context(), func(conn *sql.Conn) error { return validateIndexDefinitions(t.Context(), conn) })
	if err == nil {
		t.Fatal("missing index accepted")
	}
}
