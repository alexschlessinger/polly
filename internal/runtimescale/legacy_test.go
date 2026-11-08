package runtimescale

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
	"github.com/alexschlessinger/pollytool/internal/sessiontest"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

func TestRuntimeStorageScaleLegacyTransparency(t *testing.T) {
	t.Parallel()
	for _, factory := range sessiontest.Factories() {
		t.Run(factory.Name, func(t *testing.T) {
			t.Parallel()
			store := CountLegacyStore(factory.New(t).Open())
			fixture := sessiontest.SeedFamily(t, store, 2, 16)
			coord := fixture.Parent.(sessions.CoordinationSession)
			r := runtimemetrics.New()
			ctx := runtimemetrics.WithRecorder(t.Context(), r)
			veto := errors.New("callback veto")
			calls := 0
			err := coord.UpdateCoordination(ctx, func(state *sessions.CoordinationState) error {
				calls++
				state.Records["mail"]["mail-000000"] = json.RawMessage(`{}`)
				state.Append = []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "must not commit"}}
				return veto
			})
			if !errors.Is(err, veto) || calls != 1 {
				t.Fatalf("veto: calls=%d err=%v", calls, err)
			}
			state, err := coord.ReadCoordination(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if string(state.Records["mail"]["mail-000000"]) == `{}` || state.Sequence != 0 {
				t.Fatal("veto changed durable state")
			}
			err = coord.UpdateCoordination(ctx, func(state *sessions.CoordinationState) error {
				calls++
				state.ExpectedSequence = new(int64)
				state.Append = []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "committed"}}
				return nil
			})
			if err != nil || calls != 2 {
				t.Fatalf("commit: calls=%d err=%v", calls, err)
			}
			history, err := fixture.Parent.GetHistory(ctx)
			if err != nil || len(history) != 1 || history[0].Content != "committed" {
				t.Fatalf("history=%v err=%v", history, err)
			}
			if err := coord.UpdateCoordination(ctx, nil); err == nil {
				t.Fatal("nil callback accepted")
			}
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			if err := coord.UpdateCoordination(canceled, func(*sessions.CoordinationState) error { calls++; return nil }); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation: %v", err)
			}
			if calls != 2 {
				t.Fatal("canceled callback invoked")
			}
			s := r.Snapshot()
			if s.Count(runtimemetrics.CoordinationUpdates) != 4 || s.Count(runtimemetrics.CoordinationReads) != 1 || s.Count(runtimemetrics.TranscriptMessagesRead) != 1 {
				t.Fatalf("wrong call/result counts: %+v", s)
			}
			// The wrapper counts API materialization. Only the native SQLite
			// readSwarmRecords seam counts real SELECTs and scanned rows.
			var queries, rows uint64
			if factory.Name == "sqlite" {
				queries, rows = 3, 15 // read + vetoed update + committed update; five records each
			}
			if s.Count(runtimemetrics.SQLStatements) != queries || s.Count(runtimemetrics.RowsRead) != rows {
				t.Fatalf("covered native SQL: queries=%d rows=%d", s.Count(runtimemetrics.SQLStatements), s.Count(runtimemetrics.RowsRead))
			}
		})
	}
}

type sessionOnly struct{ sessions.Session }

func TestRuntimeStorageScaleLegacyCapabilities(t *testing.T) {
	t.Parallel()
	store := CountLegacyStore(sessiontest.File(t).Open())
	if _, ok := any(store).(sessions.ViewStore); ok {
		t.Fatal("legacy adapter advertised optional native capability")
	}
	handle, err := store.Acquire(t.Context(), "plain", sessions.AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { handle.Close() })
	if _, ok := CountLegacySession(&sessionOnly{Session: handle}).(sessions.CoordinationSession); ok {
		t.Fatal("invented coordination capability")
	}
}
