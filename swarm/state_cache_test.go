package swarm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/sessions"
)

// A display polling between checkpoints gets the previous decode back; a
// mutation invalidates it.
func TestStateCacheReusesUnchangedDecode(t *testing.T) {
	t.Parallel()
	r := runtimeTest(t, idleModel(), 1, 1)
	ctx := context.Background()
	first, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	again, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if again != first {
		t.Fatal("unchanged records decoded twice")
	}
	task, err := r.CreateTask(ctx, "describe", "criteria", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	after, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after == first || after.Tasks[task.ID] == nil {
		t.Fatal("mutation served the stale decode")
	}
	if first.Tasks[task.ID] != nil {
		t.Fatal("stale decode was mutated")
	}
}

func TestSameRecords(t *testing.T) {
	t.Parallel()
	base := func() map[string]map[string]json.RawMessage {
		return map[string]map[string]json.RawMessage{"member": {"a": json.RawMessage(`{"id":"a"}`)}, "run": {}}
	}
	changed := base()
	changed["member"]["a"] = json.RawMessage(`{"id":"b"}`)
	added := base()
	added["member"]["b"] = json.RawMessage(`{}`)
	removedKind := base()
	delete(removedKind, "run")
	for name, other := range map[string]map[string]map[string]json.RawMessage{"changed": changed, "added": added, "removed kind": removedKind} {
		if sameRecords(base(), other) {
			t.Errorf("%s compared equal", name)
		}
	}
	if !sameRecords(base(), base()) {
		t.Error("identical records compared unequal")
	}
}

// Each record is its own document: decoding never re-encodes the family, and
// encoding writes every kind, an absent map included, so retired records are
// deleted.
func TestRecordsRoundTrip(t *testing.T) {
	t.Parallel()
	s := &State{Members: map[string]*Member{"m": {ID: "m", Name: "worker"}}, Format: &FormatRecord{Version: swarmFormatVersion}}
	raw := &sessions.CoordinationState{ParentID: "root", Records: map[string]map[string]json.RawMessage{"task": {"stale": json.RawMessage(`{}`)}}}
	if err := encodeState(raw, s); err != nil {
		t.Fatal(err)
	}
	if got := string(raw.Records["member"]["m"]); !strings.Contains(got, `"name":"worker"`) {
		t.Fatalf("member record %s", got)
	}
	if len(raw.Records["task"]) != 0 {
		t.Fatalf("absent map kept records: %v", raw.Records["task"])
	}
	decoded, err := decodeState(raw)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Members["m"].Name != "worker" || decoded.Tasks == nil || len(decoded.Tasks) != 0 {
		t.Fatalf("decoded %+v", decoded)
	}
	raw.Records["member"]["bad"] = json.RawMessage(`{"id":1}`)
	if _, err := decodeState(raw); err == nil || !strings.Contains(err.Error(), "read swarm member") {
		t.Fatalf("malformed record error %v", err)
	}
}

// Shaped like the live session that motivated the cache: dozens of
// executions, steps and tasks carrying tens of kilobytes each.
func benchmarkRecords() *sessions.CoordinationState {
	filler := strings.Repeat("the quick brown fox jumps over the lazy dog ", 500)
	raw := &sessions.CoordinationState{ParentID: "root", Records: map[string]map[string]json.RawMessage{formatKind: {formatID: json.RawMessage(`{"version":2}`)}}}
	put := func(kind, id string, value any) {
		if raw.Records[kind] == nil {
			raw.Records[kind] = map[string]json.RawMessage{}
		}
		data, _ := json.Marshal(value)
		raw.Records[kind][id] = data
	}
	for i := range 40 {
		id := fmt.Sprintf("%032d", i)
		put("member", id, &Member{ID: id, Name: "worker"})
		put("task", id, &Task{ID: id, Description: filler, Result: filler})
		put("execution", id, &Execution{ID: id, Member: id, Status: "completed", Request: AgentRequest{Label: "Test agent", Task: filler}, Result: &AgentResult{Value: filler}})
		put("context", id, &ExecutionContext{ID: id, Owner: id})
	}
	return raw
}

func BenchmarkDecodeState(b *testing.B) {
	raw := benchmarkRecords()
	for b.Loop() {
		if _, err := decodeState(raw); err != nil {
			b.Fatal(err)
		}
	}
}

// measureSharedReads turns off the confirmation that tests add to every read
// and edit, so a benchmark times what a running process does.
func measureSharedReads(b *testing.B) {
	failOnSharedChange = false
	b.Cleanup(func() { failOnSharedChange = true })
}

func BenchmarkStateCacheUnchanged(b *testing.B) {
	measureSharedReads(b)
	raw := benchmarkRecords()
	var cache StateCache
	if _, err := cache.decode(raw); err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		if _, err := cache.decode(raw); err != nil {
			b.Fatal(err)
		}
	}
}

// A read after one record changed decodes that record and nothing else.
func BenchmarkStateCacheOneChanged(b *testing.B) {
	measureSharedReads(b)
	raw := benchmarkRecords()
	var cache StateCache
	if _, err := cache.decode(raw); err != nil {
		b.Fatal(err)
	}
	var id string
	for id = range raw.Records["task"] {
		break
	}
	n := 0
	for b.Loop() {
		n++
		next := &sessions.CoordinationState{Records: map[string]map[string]json.RawMessage{}}
		for kind, group := range raw.Records {
			next.Records[kind] = group
		}
		tasks := map[string]json.RawMessage{}
		for key, value := range raw.Records["task"] {
			tasks[key] = value
		}
		tasks[id] = json.RawMessage(fmt.Sprintf(`{"id":%q,"revision":%d}`, id, n))
		next.Records["task"] = tasks
		if _, err := cache.decode(next); err != nil {
			b.Fatal(err)
		}
	}
}

// A changed family shares every record that did not change with the read
// before it, and a reader that edits a shared record fails the test.
func TestStateSharesUnchangedRecords(t *testing.T) {
	r := runtimeTest(t, idleModel(), 1, 1)
	ctx := context.Background()
	kept, err := r.CreateTask(ctx, "kept", "criteria", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	edited, err := r.CreateTask(ctx, "edited", "criteria", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	before, err := r.read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.update(ctx, func(s *State) error { s.Tasks[edited.ID].Description = "changed"; return nil }); err != nil {
		t.Fatal(err)
	}
	after, err := r.read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Tasks[kept.ID] != before.Tasks[kept.ID] {
		t.Fatal("an unchanged record was decoded again")
	}
	if before.Tasks[edited.ID].Description != "edited" || after.Tasks[edited.ID].Description != "changed" {
		t.Fatalf("update reached the earlier read or missed the later one: %q, %q", before.Tasks[edited.ID].Description, after.Tasks[edited.ID].Description)
	}
	scratch, err := r.readScratch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	scratch.Tasks[kept.ID].Description = "rehearsal"
	if again, err := r.read(ctx); err != nil || again != after {
		t.Fatalf("a scratch read disturbed the shared state: %v", err)
	}
	after.Tasks[kept.ID].Description = "edited by a reader"
	defer func() {
		if got, _ := recover().(string); !strings.Contains(got, "returned by a read was modified") || !strings.Contains(got, "Tasks/"+kept.ID) {
			t.Fatalf("a reader's edit to the shared state panicked with %q", got)
		}
	}()
	_, _ = r.read(ctx)
}
