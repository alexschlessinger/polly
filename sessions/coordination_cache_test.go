package sessions

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func setRecord(kind, id, payload string) func(*CoordinationState) error {
	return func(s *CoordinationState) error {
		if s.Records[kind] == nil {
			s.Records[kind] = map[string]json.RawMessage{}
		}
		s.Records[kind][id] = json.RawMessage(payload)
		return nil
	}
}

func readRecord(t *testing.T, c CoordinationSession, kind, id string) string {
	t.Helper()
	state, err := c.ReadCoordination(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return string(state.Records[kind][id])
}

func TestCoordinationReadsReuseTheFamilyInMemory(t *testing.T) {
	ctx := context.Background()
	store, _ := openTestStore(t, ModeDisk, nil, 0)
	parent, err := store.Acquire(ctx, "parent", AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	coord := parent.(CoordinationSession)
	if err = coord.UpdateCoordination(ctx, setRecord("task", "a", `{"n":1}`)); err != nil {
		t.Fatal(err)
	}
	first, err := coord.ReadCoordination(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// A caller editing its copy leaves the family alone.
	first.Records["task"]["a"][0] = '!'
	delete(first.Records["task"], "a")
	first.Records["mail"] = map[string]json.RawMessage{"m": json.RawMessage(`{}`)}
	second, err := coord.ReadCoordination(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(second.Records["task"]["a"]); got != `{"n":1}` || second.Records["mail"] != nil {
		t.Fatalf("a caller's copy reached the family: %q %v", got, second.Records["mail"])
	}
	// A row changed behind the store on its own connection is not a commit
	// the store made, so reads that answer from memory do not see it.
	if err = store.withWrite(ctx, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `UPDATE swarm_records SET payload_json=?`, []byte(`{"n":9}`))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := readRecord(t, coord, "task", "a"); got != `{"n":1}` {
		t.Fatalf("a read scanned the table for a family held in memory: %q", got)
	}
	view, err := store.ReadCoordinationView(ctx, coord.ViewID())
	if err != nil {
		t.Fatal(err)
	}
	if got := string(view.Records["task"]["a"]); got != `{"n":1}` {
		t.Fatalf("the view scanned the table for a family held in memory: %q", got)
	}
	if err = coord.UpdateCoordination(ctx, setRecord("task", "a", `{"n":2}`)); err != nil {
		t.Fatal(err)
	}
	if got := readRecord(t, coord, "task", "a"); got != `{"n":2}` {
		t.Fatalf("read after update = %q", got)
	}
}

func TestCoordinationSeesAnotherConnectionsCommit(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "polly.db")
	open := func() *SQLiteStore {
		store, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		return store
	}
	here, there := open(), open()
	parent, err := here.Acquire(ctx, "parent", AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	coord := parent.(CoordinationSession)
	if err = coord.UpdateCoordination(ctx, setRecord("task", "a", `{"n":1}`)); err != nil {
		t.Fatal(err)
	}
	if got := readRecord(t, coord, "task", "a"); got != `{"n":1}` {
		t.Fatalf("read = %q", got)
	}
	// A member driven through a second connection writes the same family.
	child, err := there.Acquire(ctx, "child", AcquireOptions{Parent: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	member := child.(CoordinationSession)
	if got := readRecord(t, member, "task", "a"); got != `{"n":1}` {
		t.Fatalf("member read = %q", got)
	}
	for n := 2; n <= 4; n++ {
		want := fmt.Sprintf(`{"n":%d}`, n)
		writer, reader := member, coord
		if n%2 == 1 {
			writer, reader = coord, member
		}
		if err = writer.UpdateCoordination(ctx, setRecord("task", "a", want)); err != nil {
			t.Fatal(err)
		}
		if got := readRecord(t, reader, "task", "a"); got != want {
			t.Fatalf("read after the other connection's commit = %q, want %q", got, want)
		}
		view, err := here.ReadCoordinationView(ctx, coord.ViewID())
		if err != nil {
			t.Fatal(err)
		}
		if got := string(view.Records["task"]["a"]); got != want {
			t.Fatalf("view after commit = %q, want %q", got, want)
		}
	}
	// An update builds on the other connection's records, not on memory.
	if err = coord.UpdateCoordination(ctx, setRecord("task", "b", `{}`)); err != nil {
		t.Fatal(err)
	}
	if got := readRecord(t, member, "task", "a"); got != `{"n":4}` {
		t.Fatalf("an update from stale memory overwrote a record: %q", got)
	}
}

func TestCoordinationFailedUpdateLeavesTheFamily(t *testing.T) {
	ctx := context.Background()
	store, _ := openTestStore(t, ModeDisk, nil, 0)
	parent, err := store.Acquire(ctx, "parent", AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	coord := parent.(CoordinationSession)
	if err = coord.UpdateCoordination(ctx, setRecord("task", "a", `{"n":1}`)); err != nil {
		t.Fatal(err)
	}
	// The pin fails after the record change was written inside the transaction.
	err = coord.UpdateCoordination(ctx, func(s *CoordinationState) error {
		s.Records["task"]["a"] = json.RawMessage(`{"n":2}`)
		s.Pins = []string{"sha256:" + fmt.Sprintf("%064x", 1)}
		return nil
	})
	if err == nil {
		t.Fatal("an unowned pin committed")
	}
	if got := readRecord(t, coord, "task", "a"); got != `{"n":1}` {
		t.Fatalf("a rolled-back update stayed in memory: %q", got)
	}
	err = coord.UpdateCoordination(ctx, func(s *CoordinationState) error {
		s.Records["task"]["a"] = json.RawMessage(`not json`)
		return nil
	})
	if err == nil {
		t.Fatal("an invalid record committed")
	}
	if got := readRecord(t, coord, "task", "a"); got != `{"n":1}` {
		t.Fatalf("an invalid record stayed in memory: %q", got)
	}
}

func TestCoordinationUpdateFromWritesNamedRecords(t *testing.T) {
	ctx := context.Background()
	store, path := openTestStore(t, ModeDisk, nil, 0)
	parent, err := store.Acquire(ctx, "parent", AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	coord := parent.(CoordinationSession)
	delta := parent.(CoordinationDeltaSession)
	if err = coord.UpdateCoordination(ctx, func(s *CoordinationState) error {
		s.Records["task"] = map[string]json.RawMessage{"a": json.RawMessage(`{"n":1}`), "b": json.RawMessage(`{"n":1}`)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	known, err := parent.(CoordinationRevisionSession).CoordinationRevision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var committed *CoordinationState
	change := func(s *CoordinationState) error {
		if s.Records != nil {
			t.Error("a delta update copied the family")
		}
		committed = s
		s.Changes = map[string]map[string]json.RawMessage{
			"task": {"a": json.RawMessage(`{"n":2}`), "b": nil},
			"mail": {"m": json.RawMessage(`{}`)},
		}
		return nil
	}
	if err = delta.UpdateCoordinationFrom(ctx, known, change); err != nil {
		t.Fatal(err)
	}
	if committed.Revision == known {
		t.Fatal("a committed change kept the revision")
	}
	check := func(c CoordinationSession) {
		t.Helper()
		state, err := c.ReadCoordination(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if string(state.Records["task"]["a"]) != `{"n":2}` || state.Records["task"]["b"] != nil || string(state.Records["mail"]["m"]) != `{}` {
			t.Fatalf("records after a delta update: %v", state.Records)
		}
	}
	check(coord)
	// The stale caller is refused before its callback runs.
	called := false
	err = delta.UpdateCoordinationFrom(ctx, known, func(*CoordinationState) error { called = true; return nil })
	if !errors.Is(err, ErrCoordinationStale) || called {
		t.Fatalf("stale update: called=%v err=%v", called, err)
	}
	// An invalid record rolls the whole delta back, in memory as on disk.
	err = delta.UpdateCoordinationFrom(ctx, committed.Revision, func(s *CoordinationState) error {
		s.Changes = map[string]map[string]json.RawMessage{"task": {"a": json.RawMessage(`{"n":3}`), "c": json.RawMessage(`not json`)}}
		return nil
	})
	if err == nil {
		t.Fatal("an invalid record committed")
	}
	check(coord)
	// What reached the table is what memory holds.
	if err = parent.Close(); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	again, err := reopened.Acquire(ctx, "parent", AcquireOptions{ExistingOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	check(again.(CoordinationSession))
}

func BenchmarkCoordinationFamily(b *testing.B) {
	for _, records := range []int{100, 5000, 30000} {
		ctx := context.Background()
		store, err := OpenStore(StoreConfig{Mode: ModeDisk, Path: filepath.Join(b.TempDir(), "polly.db")})
		if err != nil {
			b.Fatal(err)
		}
		parent, err := store.Acquire(ctx, "parent", AcquireOptions{})
		if err != nil {
			b.Fatal(err)
		}
		coord := parent.(CoordinationSession)
		payload := json.RawMessage(`{"status":"running","text":"` + strings.Repeat("x", 900) + `"}`)
		if err = coord.UpdateCoordination(ctx, func(s *CoordinationState) error {
			group := map[string]json.RawMessage{}
			for i := range records {
				group[fmt.Sprintf("%08d", i)] = payload
			}
			s.Records["execution"] = group
			return nil
		}); err != nil {
			b.Fatal(err)
		}
		b.Run(fmt.Sprintf("read/%d", records), func(b *testing.B) {
			for b.Loop() {
				if _, err := coord.ReadCoordination(ctx); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("update-one-from/%d", records), func(b *testing.B) {
			known, err := parent.(CoordinationRevisionSession).CoordinationRevision(ctx)
			if err != nil {
				b.Fatal(err)
			}
			n := 0
			for b.Loop() {
				n++
				if err := parent.(CoordinationDeltaSession).UpdateCoordinationFrom(ctx, known, func(s *CoordinationState) error {
					s.Changes = map[string]map[string]json.RawMessage{"execution": {"00000000": json.RawMessage(fmt.Sprintf(`{"n":%d}`, n))}}
					return nil
				}); err != nil {
					b.Fatal(err)
				}
				if known, err = parent.(CoordinationRevisionSession).CoordinationRevision(ctx); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("update-one/%d", records), func(b *testing.B) {
			n := 0
			for b.Loop() {
				n++
				if err := coord.UpdateCoordination(ctx, setRecord("execution", "00000000", fmt.Sprintf(`{"n":%d}`, n))); err != nil {
					b.Fatal(err)
				}
			}
		})
		_ = parent.Close()
		_ = store.Close()
	}
}
