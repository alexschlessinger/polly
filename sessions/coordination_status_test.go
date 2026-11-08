package sessions

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
)

func TestSemanticStatusPagesStagingAndRollback(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	s := nativeHandle(t, store, "root")
	nativePut(t, s, "format", "swarm", `{"version":2}`)
	actor := hex.EncodeToString(s.id)
	_, err := s.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
		for i := 0; i < 5; i++ {
			id := fmt.Sprintf("t%d", i)
			if err := tx.Put(RecordKey{"task", id}, json.RawMessage(fmt.Sprintf(`{"id":%q,"status":"pending"}`, id))); err != nil {
				return err
			}
		}
		reader := tx.(CoordinationStatusReader)
		header, err := reader.SemanticStatus(actor, true)
		if err != nil {
			return err
		}
		if header.Counts.NeedsDecision != 5 {
			t.Fatalf("staged header=%+v", header)
		}
		first, err := reader.SemanticStatusPage(SemanticStatusQuery{Actor: actor, Section: "decisions", Limit: 2})
		if err != nil {
			return err
		}
		if len(first.Decisions) != 2 || first.Total != 5 || first.Next == "" {
			t.Fatalf("first=%+v", first)
		}
		next, err := reader.SemanticStatusPage(SemanticStatusQuery{Actor: actor, Section: "decisions", Limit: 2, After: first.Next})
		if err != nil {
			return err
		}
		if next.Decisions[0].ID != "t2" || next.Total != 5 {
			t.Fatalf("next=%+v", next)
		}
		if _, err := reader.SemanticStatusPage(SemanticStatusQuery{Actor: actor, Section: "working", Limit: 2, After: first.Next}); !errors.Is(err, ErrCoordinationCursor) {
			t.Fatalf("foreign selector=%v", err)
		}
		oversized, err := reader.SemanticStatusPage(SemanticStatusQuery{Actor: actor, Section: "decisions", Limit: 5, MaxBytes: 1})
		if err != nil {
			return err
		}
		if !oversized.Oversized || len(oversized.Decisions) != 1 || oversized.Next == "" {
			t.Fatalf("oversized=%+v", oversized)
		}
		return nil
	})
	nativeMust(t, err)
	mark := semanticMarkFor(t, s)
	veto := errors.New("veto")
	_, err = s.UpdateRecords(t.Context(), func(tx CoordinationTx) error {
		if err := tx.Delete(RecordKey{"task", "t0"}); err != nil {
			return err
		}
		h, err := tx.(CoordinationStatusReader).SemanticStatus(actor, true)
		if err != nil {
			return err
		}
		if h.Counts.NeedsDecision != 4 {
			t.Fatal(h)
		}
		return veto
	})
	if !errors.Is(err, veto) {
		t.Fatal(err)
	}
	if got := semanticMarkFor(t, s); got != mark {
		t.Fatal("veto changed history")
	}
	nativeMust(t, s.ReadRecords(t.Context(), func(r CoordinationReader) error {
		h, err := r.(CoordinationStatusReader).SemanticStatus(actor, true)
		if err == nil && h.Counts.NeedsDecision != 5 {
			t.Fatal(h)
		}
		return err
	}))
}
func TestSemanticPromotionReopenAndCleanup(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	s := nativeHandle(t, store, "root")
	nativePut(t, s, "format", "swarm", `{"version":2}`)
	nativePut(t, s, "task", "t", `{"id":"t","status":"pending"}`)
	baseline := semanticMarkFor(t, s)
	path := filepath.Join(t.TempDir(), "semantic.db")
	nativeMust(t, store.Promote(t.Context(), path))
	if got := semanticMarkFor(t, s); got != baseline {
		t.Fatal("promotion changed mark")
	}
	nativeMust(t, s.Close())
	nativeMust(t, store.Close())
	reopened, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
	nativeMust(t, err)
	defer reopened.Close()
	s = nativeHandle(t, reopened, "root")
	if got := semanticCompareFor(t, s, SemanticComparison{Baseline: baseline, Mode: SemanticParentNews}); got.Different {
		t.Fatal("restart changed canonical baseline")
	}
	actor := hex.EncodeToString(s.id)
	nativeMust(t, reopened.ReadRecordsView(t.Context(), actor, func(r CoordinationReader) error {
		h, err := r.(CoordinationStatusReader).SemanticStatus(actor, true)
		if err == nil && h.Counts.NeedsDecision != 1 {
			t.Fatal(h)
		}
		return err
	}))
	nativePut(t, s, "task", "t", `{"id":"t","status":"blocked"}`)
	if !semanticCompareFor(t, s, SemanticComparison{Baseline: baseline, Mode: SemanticParentNews}).Different {
		t.Fatal("restart lost history")
	}
	nativeMust(t, s.Close())
	nativeMust(t, reopened.withWrite(t.Context(), func(conn *sql.Conn) error {
		_, err := conn.ExecContext(t.Context(), `DELETE FROM sessions WHERE id=?`, s.id)
		return err
	}))
	nativeMust(t, reopened.withRead(t.Context(), func(conn *sql.Conn) error {
		for _, table := range []string{"swarm_semantic_heads", "swarm_semantic_history", "swarm_status_facts", "swarm_status_items", "swarm_status_totals"} {
			var n int
			if err := conn.QueryRowContext(t.Context(), `SELECT count(*) FROM `+table).Scan(&n); err != nil {
				return err
			}
			if n != 0 {
				t.Fatalf("%s retains %d rows", table, n)
			}
		}
		return nil
	}))
}
func TestSemanticMarksRejectForeignEpochVersionAndFuture(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	s := nativeHandle(t, store, "root")
	mark := semanticMarkFor(t, s)
	var token semanticToken
	nativeMust(t, semanticDecode(string(mark), &token))
	for _, mutate := range []func(*semanticToken){func(v *semanticToken) { v.Root = "foreign" }, func(v *semanticToken) { v.Version++ }, func(v *semanticToken) { v.Epoch += "1" }, func(v *semanticToken) { v.Stamp++ }} {
		bad := token
		mutate(&bad)
		err := s.ReadRecords(t.Context(), func(r CoordinationReader) error {
			_, err := SemanticReader(r).SemanticChanges(SemanticComparison{Baseline: SemanticMark(semanticEncode(bad)), Mode: SemanticParentNews})
			return err
		})
		if !errors.Is(err, ErrCoordinationCursor) {
			t.Fatalf("accepted %+v: %v", bad, err)
		}
	}
}
func TestSemanticHistoryCostAndNoop(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	s := nativeHandle(t, store, "root")
	nativePut(t, s, "format", "swarm", `{"version":2}`)
	for _, status := range []string{"pending", "blocked", "pending"} {
		nativePut(t, s, "task", "t", fmt.Sprintf(`{"id":"t","status":%q}`, status))
	}
	nativePut(t, s, "task", "t", `{"id":"t","status":"pending","description":"label only"}`)
	nativeMust(t, store.withRead(t.Context(), func(conn *sql.Conn) error {
		var rows, bytes int
		if err := conn.QueryRowContext(t.Context(), `SELECT count(*),sum(length(value)) FROM swarm_semantic_history WHERE parent_id=? AND kind='task' AND id='t'`, s.id).Scan(&rows, &bytes); err != nil {
			return err
		}
		if rows != 3 {
			t.Fatalf("history rows=%d", rows)
		}
		t.Logf("canonical payload storage: %d transitions, %d bytes, %.1f bytes/transition (excludes SQLite index/page overhead)", rows, bytes, float64(bytes)/float64(rows))
		return nil
	}))
}
func TestSemanticStatusDoesNotHydrateOversizedLookahead(t *testing.T) {
	store, _ := openTestStore(t, ModeMemory, nil, 0)
	s := nativeHandle(t, store, "root")
	nativePut(t, s, "format", "swarm", `{"version":2}`)
	actor := hex.EncodeToString(s.id)
	for i := 0; i < 3; i++ {
		text := "short"
		if i > 0 {
			text = strings.Repeat("large", 10000)
		}
		payload, _ := json.Marshal(map[string]any{"id": fmt.Sprintf("e%d", i), "status": "paused", "retryPause": map[string]any{"reason": text, "attempts": 3}})
		nativePut(t, s, "execution", fmt.Sprintf("e%d", i), string(payload))
		nativePut(t, s, "task", fmt.Sprintf("t%d", i), fmt.Sprintf(`{"id":"t%d","status":"blocked","execution":"e%d"}`, i, i))
	}
	recorder := runtimemetrics.New()
	ctx := runtimemetrics.WithRecorder(t.Context(), recorder)
	nativeMust(t, s.ReadRecords(ctx, func(r CoordinationReader) error {
		p, err := r.(CoordinationStatusReader).SemanticStatusPage(SemanticStatusQuery{Actor: actor, Section: "decisions", Limit: 3, MaxBytes: 500})
		if err == nil && (len(p.Decisions) != 1 || p.Next == "") {
			t.Fatal(p)
		}
		return err
	}))
	if bytes := recorder.Snapshot().Count(runtimemetrics.PayloadBytesRead); bytes > 1000 {
		t.Fatalf("hydrated oversized lookahead: %d bytes", bytes)
	}
}
