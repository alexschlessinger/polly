package sessions_test

import (
	"fmt"
	"testing"

	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
	"github.com/alexschlessinger/pollytool/internal/runtimescale"
	"github.com/alexschlessinger/pollytool/internal/sessiontest"
	"github.com/alexschlessinger/pollytool/sessions"
)

// These are observations of the existing broad API, not promises that future
// narrow reads already exist. SQL rows/allocations are not inferred from results.
func TestRuntimeStorageScaleArchive(t *testing.T) {
	t.Parallel()
	for _, factory := range sessiontest.Factories() {
		for _, size := range []int{0, 4, 16} {
			t.Run(fmt.Sprintf("%s/%d", factory.Name, size), func(t *testing.T) {
				t.Parallel()
				store := runtimescale.CountLegacyStore(factory.New(t).Open())
				entries := sessiontest.SeedArchive(t, store, sessiontest.ArchiveSpec{Sessions: size, Messages: 3, PayloadBytes: 64})
				r := runtimemetrics.New()
				ctx := runtimemetrics.WithRecorder(t.Context(), r)
				rows, err := store.ListSummaries(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if len(rows) != size {
					t.Fatalf("summaries=%d want=%d", len(rows), size)
				}
				byID := make(map[string]sessions.SessionSummary, len(rows))
				for _, row := range rows {
					if row.ID == "" || row.Metadata == nil || row.MessageCount != 3 || row.InUse {
						t.Fatalf("invalid archive summary: %+v", row)
					}
					byID[row.ID] = row
				}
				for _, entry := range entries {
					row, ok := byID[entry.ID]
					if !ok || row.Metadata.Name != entry.Name {
						t.Fatalf("missing archive identity %s", entry.Name)
					}
				}
				if size > 0 {
					handle, err := store.Acquire(ctx, entries[0].Name, sessions.AcquireOptions{ExpectedID: entries[0].ID})
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { handle.Close() })
					history, err := handle.GetHistory(ctx)
					if err != nil || len(history) != 3 {
						t.Fatalf("history=%v err=%v", history, err)
					}
				}
				s := r.Snapshot()
				if s.Count(runtimemetrics.SummaryReads) != 1 || s.Count(runtimemetrics.SummariesRead) != uint64(size) {
					t.Fatal("counted adapter disagrees with returned summaries")
				}
				t.Logf("foundation observation: backend=%s archive=%d summary_calls=%d summaries_materialized=%d transcript_messages=%d (not SQL row counts)", factory.Name, size, s.Count(runtimemetrics.SummaryReads), s.Count(runtimemetrics.SummariesRead), s.Count(runtimemetrics.TranscriptMessagesRead))
			})
		}
	}
}
