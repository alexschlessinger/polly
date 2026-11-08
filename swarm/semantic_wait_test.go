package swarm

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/sessions"
)

// Hooks surround complete read transactions, never making nested store calls.
// They place the test at the baseline/channel/recheck/block boundaries without
// sleeps or production scheduling hooks.
type semanticWaitSession struct {
	sessions.CoordinationSession
	sessions.CoordinationRecords
	reads         atomic.Int64
	before, after func(int)
}

func (s *semanticWaitSession) CoordinationCapabilities() sessions.CoordinationCapabilities {
	if reporter, ok := s.CoordinationSession.(sessions.CoordinationCapabilityReporter); ok {
		return reporter.CoordinationCapabilities()
	}
	return sessions.CoordinationCapabilities{}
}

func (s *semanticWaitSession) ReadRecords(ctx context.Context, fn func(sessions.CoordinationReader) error) error {
	// Revision-source callbacks are concurrent and are not logical readiness
	// reads. Counting them would shift the barriers and introduce a data race.
	if ctx.Value(notificationWaitReadKey{}) != true {
		return s.CoordinationRecords.ReadRecords(ctx, fn)
	}
	n := int(s.reads.Add(1))
	if s.before != nil {
		s.before(n)
	}
	err := s.CoordinationRecords.ReadRecords(ctx, fn)
	if s.after != nil {
		s.after(n)
	}
	return err
}

func TestSemanticParentWaitBarriers(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"before-baseline", "after-baseline", "channel-before-recheck", "before-block", "cancel"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			r, root := lifecycleRecordFixture(t, 0, false)
			r.active["selected"] = &invocation{}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			entered, resume := make(chan struct{}), make(chan struct{})
			barrier := func() {
				close(entered)
				select {
				case <-resume:
				case <-ctx.Done():
				}
			}
			handle := &semanticWaitSession{CoordinationSession: root, CoordinationRecords: root.(sessions.CoordinationRecords)}
			handle.before = func(n int) {
				if phase == "before-baseline" && n == 1 || phase == "channel-before-recheck" && n == 2 {
					barrier()
				}
			}
			handle.after = func(n int) {
				if phase == "after-baseline" && n == 1 || (phase == "before-block" || phase == "cancel") && n == 2 {
					barrier()
				}
				if phase == "before-baseline" && n == 2 {
					cancel()
				}
			}
			r.parent = handle
			type result struct {
				wake parentWake
				err  error
			}
			done := make(chan result, 1)
			go func() { wake, err := r.waitParent(ctx); done <- result{wake, err} }()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("wait did not reach barrier")
			}
			if phase == "cancel" {
				cancel()
			} else {
				_, err := root.(sessions.CoordinationRecords).UpdateRecords(ctx, func(tx sessions.CoordinationTx) error {
					return tx.Put(sessions.RecordKey{Kind: "task", ID: "news"}, []byte(`{"id":"news","status":"blocked"}`))
				})
				if err != nil {
					t.Fatal(err)
				}
				if phase == "before-block" {
					r.mu.Lock()
					delete(r.active, "selected")
					r.mu.Unlock()
				}
				r.changed()
			}
			close(resume)
			select {
			case got := <-done:
				if phase == "before-baseline" || phase == "cancel" {
					if !errors.Is(got.err, context.Canceled) || got.wake.News {
						t.Fatalf("unexpected wake: %+v", got)
					}
				} else if got.err != nil || !got.wake.News || got.wake.Idle != (phase == "before-block") {
					t.Fatalf("lost news/idle: %+v", got)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("wait lost boundary notification")
			}
		})
	}
}
