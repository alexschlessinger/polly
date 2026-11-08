package swarm

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

func displayUsageRuntime(t *testing.T) (*Runtime, sessions.CoordinationRecords) {
	t.Helper()
	store, err := sessions.OpenStore(sessions.StoreConfig{Mode: sessions.ModeMemory})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	session, err := store.Acquire(t.Context(), "usage", sessions.AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	records := session.(sessions.CoordinationRecords)
	_, err = records.UpdateRecords(t.Context(), func(tx sessions.CoordinationTx) error {
		return tx.Put(sessions.RecordKey{Kind: "format", ID: "swarm"}, json.RawMessage(`{"version":2}`))
	})
	if err != nil {
		t.Fatal(err)
	}
	return &Runtime{parent: session.(sessions.CoordinationSession)}, records
}

func putDisplayUsageRecord(tx sessions.CoordinationTx, kind, id string, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return tx.Put(sessions.RecordKey{Kind: kind, ID: id}, payload)
}

func TestDisplayUsageBeforeFirstDelegation(t *testing.T) {
	t.Parallel()
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint(legacy), func(t *testing.T) {
			store, err := sessions.OpenStore(sessions.StoreConfig{Mode: sessions.ModeMemory})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			session, err := store.Acquire(t.Context(), "fresh", sessions.AcquireOptions{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = session.Close() })
			parent := session.(sessions.CoordinationSession)
			r := &Runtime{ID: session.(sessions.ViewIdentity).ViewID(), parent: parent}
			if legacy {
				r.parent = repositoryLegacyOnly{parent}
			}
			for _, journal := range []bool{false, true} {
				if journal {
					_, err = session.(sessions.CoordinationRecords).UpdateRecords(t.Context(), func(tx sessions.CoordinationTx) error {
						return putDisplayUsageRecord(tx, "parent_turn", r.ID, &ParentTurn{})
					})
					if err != nil {
						t.Fatal(err)
					}
				}
				metric := runtimemetrics.New()
				usage, err := r.ReadDisplayUsage(runtimemetrics.WithRecorder(t.Context(), metric), nil)
				if err != nil || len(usage.Members) != 0 || metric.Snapshot().Count(runtimemetrics.WriteTransactions) != 0 {
					t.Fatalf("empty root journal=%v usage=%+v error=%v", journal, usage, err)
				}
			}
			// Formatless domain records are corruption, not an empty swarm.
			if err := parent.UpdateCoordination(t.Context(), func(raw *sessions.CoordinationState) error {
				raw.Records["execution"] = map[string]json.RawMessage{"bad": json.RawMessage(`{"id":"bad"}`)}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := r.ReadDisplayUsage(t.Context(), nil); err == nil {
				t.Fatal("formatless domain record accepted")
			}
		})
	}
}

func TestDisplayUsageNativeLegacyParityAndMissingCounts(t *testing.T) {
	t.Parallel()
	r, records := displayUsageRuntime(t)
	input, output, zero := 1200, 30, 0
	_, err := records.UpdateRecords(t.Context(), func(tx sessions.CoordinationTx) error {
		for id, value := range map[string]*Execution{
			"a-first":    {ID: "a-first", Member: "a", Status: "completed", Usage: Usage{Samples: 2, InputTokens: &input, OutputTokens: &output}, Result: &AgentResult{Usage: Usage{Samples: 99, InputTokens: &input}}},
			"a-latest":   {ID: "a-latest", Member: "a", Status: "running", Generation: 3, Request: AgentRequest{Task: "latest task"}, Usage: Usage{Samples: 1, OutputTokens: &output}},
			"b-workflow": {ID: "b-workflow", Member: "b", Workflow: "workflow", Status: "failed", Usage: Usage{InputTokens: &input, OutputTokens: &output}},
			"c-empty":    {ID: "c-empty", Member: "c"},
			"d-zero":     {ID: "d-zero", Member: "orphan", Usage: Usage{Samples: 1, InputTokens: &zero, OutputTokens: &zero}},
			"e-missing":  {ID: "e-missing", Member: "unknown", Usage: Usage{Samples: 2}},
		} {
			if err := putDisplayUsageRecord(tx, "execution", id, value); err != nil {
				return err
			}
		}
		for id, value := range map[string]*Member{
			"a": {ID: "a", Label: "Explicit", Execution: "a-latest"},
			"b": {ID: "b", Name: "fallback", Task: "task"},
			"c": {ID: "c", Name: "queued"},
		} {
			if err := putDisplayUsageRecord(tx, "member", id, value); err != nil {
				return err
			}
		}
		return putDisplayUsageRecord(tx, "task", "task", &Task{ID: "task", Description: "task brief"})
	})
	if err != nil {
		t.Fatal(err)
	}
	native, err := r.ReadDisplayUsage(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	legacyRuntime := &Runtime{parent: repositoryLegacyOnly{r.parent}}
	legacy, err := legacyRuntime.ReadDisplayUsage(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []sessions.DisplayMemberUsage{
		{Member: "a", Label: "Explicit", Brief: "latest task", InputTokens: 1200, OutputTokens: 60, InputMissing: true},
		{Member: "b", Brief: "task brief", InputTokens: 1200, OutputTokens: 30},
		{Member: "c", Brief: "queued"},
		{Member: "orphan"},
		{Member: "unknown", InputMissing: true, OutputMissing: true},
	}
	if !reflect.DeepEqual(native.Members, want) || !reflect.DeepEqual(legacy.Members, want) {
		t.Fatalf("native=%+v legacy=%+v want=%+v", native.Members, legacy.Members, want)
	}
	if len(native.Revisions) != 3 || len(legacy.Revisions) != 0 {
		t.Fatalf("revision capabilities native=%+v legacy=%+v", native, legacy)
	}
}

func TestDisplayUsageReadWorkIgnoresUnrelatedPayloads(t *testing.T) {
	t.Parallel()
	r, records := displayUsageRuntime(t)
	input, output := 100, 10
	_, err := records.UpdateRecords(t.Context(), func(tx sessions.CoordinationTx) error {
		if err := putDisplayUsageRecord(tx, "execution", "selected", &Execution{ID: "selected", Member: "worker", Usage: Usage{InputTokens: &input, OutputTokens: &output}}); err != nil {
			return err
		}
		return putDisplayUsageRecord(tx, "member", "worker", &Member{ID: "worker", Name: "reader"})
	})
	if err != nil {
		t.Fatal(err)
	}
	read := func(known []sessions.Revision) (DisplayUsage, runtimemetrics.Snapshot) {
		t.Helper()
		metric := runtimemetrics.New()
		ctx := runtimemetrics.WithRecorder(context.Background(), metric)
		got, err := r.ReadDisplayUsage(ctx, known)
		if err != nil {
			t.Fatal(err)
		}
		return got, metric.Snapshot()
	}
	first, before := read(nil)
	_, err = records.UpdateRecords(t.Context(), func(tx sessions.CoordinationTx) error {
		for i := 0; i < 200; i++ {
			id := fmt.Sprintf("irrelevant-%03d", i)
			if err := putDisplayUsageRecord(tx, "task", id, &Task{ID: id, Result: strings.Repeat("large result ", 2048)}); err != nil {
				return err
			}
		}
		return putDisplayUsageRecord(tx, "execution", "selected", &Execution{ID: "selected", Member: "worker", Usage: Usage{InputTokens: &input, OutputTokens: &output}, Intent: []messages.ChatMessage{{Role: "user", Content: strings.Repeat("unrelated intent ", 20_000)}}, Result: &AgentResult{Value: strings.Repeat("large result ", 20_000)}})
	})
	if err != nil {
		t.Fatal(err)
	}
	after, count := read(nil)
	if !reflect.DeepEqual(first.Members, after.Members) {
		t.Fatal("irrelevant payloads changed usage")
	}
	for _, counter := range []runtimemetrics.Counter{runtimemetrics.RowsRead, runtimemetrics.PayloadBytesRead, runtimemetrics.RecordsDecoded, runtimemetrics.SQLStatements} {
		if count.Count(counter) != before.Count(counter) {
			t.Fatalf("read work grew for counter %v: before=%+v after=%+v", counter, before, count)
		}
	}
	unchanged, count := read(after.Revisions)
	// The repository's fail-closed format guard still reads {"version":2};
	// there must be no execution/member/task payload or projection query.
	if !unchanged.Unchanged || unchanged.Members != nil || count.Count(runtimemetrics.PayloadBytesRead) > uint64(len(`{"version":2}`)) || count.Count(runtimemetrics.RecordsDecoded) > 2 || count.Count(runtimemetrics.RowsRead) > 6 || count.Count(runtimemetrics.SQLStatements) > 4 || count.Count(runtimemetrics.WriteTransactions) != 0 {
		t.Fatalf("unchanged display exceeded format/revision work: %+v %+v", unchanged, count)
	}
	// Mail/publications do not invalidate these three scoped revision tokens.
	_, err = records.UpdateRecords(t.Context(), func(tx sessions.CoordinationTx) error {
		return putDisplayUsageRecord(tx, "mail", "mail", &Mail{ID: "mail", To: "worker", Text: "new message"})
	})
	if err != nil {
		t.Fatal(err)
	}
	unchanged, _ = read(after.Revisions)
	if !unchanged.Unchanged {
		t.Fatal("unrelated family kind invalidated usage")
	}
	// A correction/deletion must invalidate the cached all-execution sum.
	_, err = records.UpdateRecords(t.Context(), func(tx sessions.CoordinationTx) error {
		return tx.Delete(sessions.RecordKey{Kind: "execution", ID: "selected"})
	})
	if err != nil {
		t.Fatal(err)
	}
	deleted, _ := read(after.Revisions)
	if deleted.Unchanged || len(deleted.Members) != 0 {
		t.Fatalf("deleted execution retained: %+v", deleted)
	}
}
