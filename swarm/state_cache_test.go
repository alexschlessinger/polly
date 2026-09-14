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
func BenchmarkStateCacheUnchanged(b *testing.B) {
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
