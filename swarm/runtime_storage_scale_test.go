package swarm

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
	"github.com/alexschlessinger/pollytool/internal/runtimescale"
	"github.com/alexschlessinger/pollytool/internal/sessiontest"
	"github.com/alexschlessinger/pollytool/sessions"
)

func TestRuntimeStorageScaleFamily(t *testing.T) {
	t.Parallel()
	for _, factory := range sessiontest.Factories() {
		for _, size := range []int{0, 4, 16} {
			t.Run(fmt.Sprintf("%s/%d", factory.Name, size), func(t *testing.T) {
				t.Parallel()
				store := factory.New(t).Open()
				fixture := sessiontest.SeedFamily(t, store, size, 128)
				raw, err := fixture.ReadFamily(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				decoded, err := decodeState(raw)
				if err != nil {
					t.Fatal(err)
				}
				if len(decoded.Members) != size || len(decoded.Messages) != size || decoded.Format == nil || decoded.Format.Version != swarmFormatVersion {
					t.Fatal("invalid family fixture")
				}
				for _, member := range fixture.Members {
					view, err := store.(sessions.ViewStore).ReadView(t.Context(), sessions.ViewTarget{ID: member.ID}, "")
					if err != nil {
						t.Fatal(err)
					}
					if view.ParentID != raw.ParentID || view.Metadata.SwarmID != raw.ParentID || view.InUse {
						t.Fatalf("invalid child identity or retained lease: %+v", view)
					}
					mail := inbox(decoded, member.ID, true)
					if len(mail) != 1 || mail[0].From != raw.ParentID || len(mail[0].Text) != 128 {
						t.Fatalf("invalid addressed fixture mail: %+v", mail)
					}
				}
				coord := runtimescale.CountLegacySession(fixture.Parent).(sessions.CoordinationSession)
				// Exercise the actual State/cache read path, without starting a
				// scheduler or claiming these idle members are active executions.
				runtime := &Runtime{parent: coord}
				r := runtimemetrics.New()
				ctx := runtimemetrics.WithRecorder(t.Context(), r)
				first, err := runtime.State(ctx)
				if err != nil {
					t.Fatal(err)
				}
				again, err := runtime.State(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if first != again {
					t.Fatal("unchanged state failed to reuse decode")
				}
				counts := r.Snapshot()
				records, bytes := sessiontest.RecordSize(raw)
				if counts.Count(runtimemetrics.CoordinationReads) != 2 || counts.Count(runtimemetrics.CoordinationRecordsRead) != 2*records || counts.Count(runtimemetrics.CoordinationBytesRead) != 2*bytes {
					t.Fatal("legacy adapter observations do not match materialized family")
				}
				if counts.Count(runtimemetrics.RecordsDecoded) != records || counts.Timing(runtimemetrics.StateDecode).Count != 1 {
					t.Fatal("cache hit counted another decode")
				}
				if size > 0 {
					err := coord.UpdateCoordination(ctx, func(state *sessions.CoordinationState) error {
						var mail Mail
						if err := json.Unmarshal(state.Records["mail"]["mail-000000"], &mail); err != nil {
							return err
						}
						mail.Delivered = true
						payload, err := json.Marshal(mail)
						if err != nil {
							return err
						}
						state.Records["mail"][mail.ID] = payload
						return nil
					})
					if err != nil {
						t.Fatal(err)
					}
					// Native legacy writes now decode touched payloads to maintain
					// derived indexes. That real storage work shares this recorder;
					// measure the cache reload separately, rather than assuming
					// UpdateCoordination performs no decoding.
					beforeReload := r.Snapshot()
					if beforeReload.Timing(runtimemetrics.StateDecode).Count != 1 {
						t.Fatal("receipt write unexpectedly decoded runtime state")
					}
					after, err := runtime.State(ctx)
					if err != nil {
						t.Fatal(err)
					}
					if after == first || len(inbox(after, fixture.Members[0].ID, true)) != 0 || len(inbox(first, fixture.Members[0].ID, true)) != 1 {
						t.Fatal("receipt mutation failed to invalidate cache or mutated old snapshot")
					}
					measured := r.Snapshot()
					reloaded := measured.Count(runtimemetrics.RecordsDecoded) - beforeReload.Count(runtimemetrics.RecordsDecoded)
					if reloaded != records || measured.Timing(runtimemetrics.StateDecode).Count != 2 {
						t.Fatalf("cache invalidation decoded=%d state_decodes=%d; want decoded=%d state_decodes=2", reloaded, measured.Timing(runtimemetrics.StateDecode).Count, records)
					}
					t.Logf("receipt write decoded=%d; subsequent cache reload decoded=%d", beforeReload.Count(runtimemetrics.RecordsDecoded)-counts.Count(runtimemetrics.RecordsDecoded), reloaded)
				}
				timing := counts.Timing(runtimemetrics.CoordinationRead)
				t.Logf("foundation observation: backend=%s retained_members=%d records_per_read=%d payload_bytes_per_read=%d unchanged_poll_records=%d read_p50_upper=%s read_p95_upper=%s read_max=%s; decode reused, legacy payloads still fetched (not SQL counts, no active-execution/stress claim)", factory.Name, size, records, bytes, counts.Count(runtimemetrics.CoordinationRecordsRead)/2, timing.Percentile(50), timing.Percentile(95), timing.Max)
			})
		}
	}
}
