package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
)

func rawRecords(t *testing.T, r *Runtime) map[string]map[string]json.RawMessage {
	t.Helper()
	raw, err := r.parent.ReadCoordination(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return raw.Records
}

func recordRows(records map[string]map[string]json.RawMessage) int {
	n := 0
	for _, group := range records {
		n += len(group)
	}
	return n
}

func idleModel() llm.LLM {
	return modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return answer("unused") })
}

// A root records its format with its first coordination mutation, never at
// open: any swarm row pins the session against expiry.
func TestFormatRecordWrittenLazily(t *testing.T) {
	r := runtimeTest(t, idleModel(), 1, 1)
	ctx := context.Background()
	if rows := recordRows(rawRecords(t, r)); rows != 0 {
		t.Fatalf("fresh root holds %d records", rows)
	}
	if _, err := r.CreateTask(ctx, "describe", "criteria", nil, ""); err != nil {
		t.Fatal(err)
	}
	if got := string(rawRecords(t, r)[formatKind][formatID]); got != `{"version":2}` {
		t.Fatalf("format record %q", got)
	}
}

func TestEmptyStoreInitializes(t *testing.T) {
	for name, records := range map[string]map[string]map[string]json.RawMessage{
		"no records":          {},
		"parent journal only": {"parent_turn": {"root": json.RawMessage(`{"intent":[]}`)}},
	} {
		s, err := decodeState(&sessions.CoordinationState{ParentID: "root", Records: records})
		if err != nil || s.Format != nil {
			t.Fatalf("%s: err=%v format=%+v", name, err, s.Format)
		}
	}
}

func TestUnsupportedFormatForLegacyRecords(t *testing.T) {
	member := map[string]json.RawMessage{"m": json.RawMessage(`{"id":"m","status":"idle"}`)}
	for name, tc := range map[string]struct {
		records  map[string]map[string]json.RawMessage
		wantRoot bool
	}{
		"members without a format record": {records: map[string]map[string]json.RawMessage{"member": member}, wantRoot: true},
		"version 1 members":               {records: map[string]map[string]json.RawMessage{formatKind: {formatID: json.RawMessage(`{"version":1}`)}, "member": member}, wantRoot: true},
		"newer format":                    {records: map[string]map[string]json.RawMessage{formatKind: {formatID: json.RawMessage(`{"version":3}`)}, "member": member}, wantRoot: true},
		"embedded workflow steps":         {records: map[string]map[string]json.RawMessage{formatKind: {formatID: json.RawMessage(`{"version":2}`)}, "workflow": {"w": json.RawMessage(`{"id":"w","steps":[{"id":"s"}]}`)}}},
	} {
		_, err := decodeState(&sessions.CoordinationState{ParentID: "root-1", Records: tc.records})
		if !errors.Is(err, ErrUnsupportedFormat) || tc.wantRoot && !strings.Contains(err.Error(), "root-1") {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// A runtime refuses to open such a root at construction.
	ctx := context.Background()
	store, err := sessions.OpenStore(sessions.StoreConfig{Mode: sessions.ModeMemory})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	parent, err := store.Acquire(ctx, "parent", sessions.AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	if err := parent.(sessions.CoordinationSession).UpdateCoordination(ctx, func(s *sessions.CoordinationState) error {
		s.Records["member"] = member
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	registry := tools.NewToolRegistry(nil, tools.WithUnsafeNoSandbox())
	defer registry.Close()
	_, err = New(Config{Store: store, Parent: parent, Registry: registry, Client: idleModel(), Root: t.TempDir(), Directory: filepath.Join(t.TempDir(), "runtime")})
	if !errors.Is(err, ErrUnsupportedFormat) {
		t.Fatalf("New over legacy records: %v", err)
	}
}

// Reading views never repairs, formats or claims anything.
func TestDisplayOnlyReadsNeverWrite(t *testing.T) {
	r := runtimeTest(t, idleModel(), 1, 1)
	ctx := context.Background()
	if _, err := r.State(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadStateView(ctx, r.config.Store.(sessions.CoordinationViewStore), r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.inspectAgents(ctx, r.ID, tools.Args{}); err != nil {
		t.Fatal(err)
	}
	if rows := recordRows(rawRecords(t, r)); rows != 0 {
		t.Fatalf("display reads wrote %d records", rows)
	}
}
