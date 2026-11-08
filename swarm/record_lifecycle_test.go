package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

// Only the supplied legacy capability is exposed, including on child handles.
// This adapter deliberately makes no indexed-efficiency claim.
type lifecycleLegacySession struct {
	sessions.Session
	sessions.CoordinationSession
}

type lifecycleLegacyStore struct{ sessions.SessionStore }

func (s lifecycleLegacyStore) Acquire(ctx context.Context, name string, options sessions.AcquireOptions) (sessions.Session, error) {
	handle, err := s.SessionStore.Acquire(ctx, name, options)
	if err != nil {
		return nil, err
	}
	return lifecycleLegacySession{handle, handle.(sessions.CoordinationSession)}, nil
}

func TestRecordLifecycleNativeLegacyResumeAndCallReplay(t *testing.T) {
	t.Parallel()
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%v", legacy), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			var calls atomic.Int32
			r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
				calls.Add(1)
				return answer("retained result")
			}), 1, 5)
			if legacy {
				root := lifecycleLegacySession{r.config.Parent, r.parent}
				r.parent, r.config.Parent = root, root
				r.config.Store = lifecycleLegacyStore{r.config.Store}
			}
			req := AgentRequest{TaskName: "record_worker", Label: "Record worker", Task: "inspect", ReadOnly: true, CallID: "stable-call"}
			first, err := r.Agent(ctx, "", req)
			if err != nil {
				t.Fatal(err)
			}
			duplicate, err := r.Agent(ctx, "", req)
			if err != nil || duplicate.Execution != first.Execution || calls.Load() != 1 {
				t.Fatalf("call replay created work: result=%+v calls=%d err=%v", duplicate, calls.Load(), err)
			}
			admitParent(t, r)
			var generation, starts int
			if err := r.updateRecords(ctx, func(tx *recordTx) error {
				e, err := tx.Execution(first.Execution)
				if err != nil {
					return err
				}
				task, err := tx.Task(first.Task)
				if err != nil {
					return err
				}
				run, err := tx.Run(e.Run)
				if err != nil {
					return err
				}
				generation, starts = e.Generation, run.Starts
				e.Status, task.Status = "paused", "blocked"
				e.Intent = []messages.ChatMessage{{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "uncertain-call", Name: "never_replay", Arguments: `{}`}}}}
				if err := tx.PutTask(task); err != nil {
					return err
				}
				return tx.PutExecution(e)
			}); err != nil {
				t.Fatal(err)
			}
			if err := r.Resume(ctx, first.Session, 0); err != nil {
				t.Fatal(err)
			}
			waitWorkflowIdle(t, r)
			if err := r.readRecords(ctx, func(reader *recordReader) error {
				e, err := reader.Execution(first.Execution)
				if err != nil {
					return err
				}
				run, err := reader.Run(e.Run)
				if err != nil {
					return err
				}
				if e.Generation != generation+1 || run.Starts != starts || len(e.Intent) != 0 || e.Status != "completed" {
					t.Fatalf("resume changed logical start or fence: execution=%+v run=%+v", e, run)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			s, err := r.read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			handle, err := r.acquireMemberSession(ctx, s.Members[first.Session])
			if err != nil {
				t.Fatal(err)
			}
			history, err := handle.GetHistory(ctx)
			closeErr := handle.Close()
			if err != nil || closeErr != nil {
				t.Fatal(errors.Join(err, closeErr))
			}
			receipts := 0
			for _, message := range history {
				if message.ToolCallID == "uncertain-call" {
					receipts++
					if message.Content != llm.ToolInterruptedContent {
						t.Fatalf("uncertain tool was replayed: %+v", message)
					}
				}
			}
			if receipts != 1 || calls.Load() != 2 {
				t.Fatalf("recovery replay: receipts=%d model_calls=%d", receipts, calls.Load())
			}
		})
	}
}

type lifecycleSummaryOnlyStore struct {
	sessions.SessionStore
	sessions.SummaryPageStore
	lookups atomic.Int32
}

func (s *lifecycleSummaryOnlyStore) ListSummaries(context.Context) ([]sessions.SessionSummary, error) {
	return nil, errors.New("member recovery attempted a summary scan")
}

func (s *lifecycleSummaryOnlyStore) GetSummary(ctx context.Context, id string) (*sessions.SessionSummary, error) {
	s.lookups.Add(1)
	return s.SummaryPageStore.GetSummary(ctx, id)
}

func TestRecordLifecycleRenamedMemberUsesExactSummary(t *testing.T) {
	t.Parallel()
	r := runtimeTest(t, nilModel(), 1, 3)
	ctx := t.Context()
	first, err := r.Agent(ctx, "", AgentRequest{Label: "Lookup", Task: "inspect", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	var member *Member
	if err := r.readRecords(ctx, func(reader *recordReader) error {
		var err error
		member, err = reader.Member(first.Session)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	child, err := r.acquireMemberSession(ctx, member)
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Rename(ctx, "renamed-exact-lookup"); err != nil {
		t.Fatal(err)
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	reused, err := r.config.Store.Acquire(ctx, member.Name, sessions.AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer reused.Close()
	store := &lifecycleSummaryOnlyStore{SessionStore: r.config.Store, SummaryPageStore: r.config.Store.(sessions.SummaryPageStore)}
	r.config.Store = store
	child, err = r.acquireMemberSession(ctx, member)
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	if child.ViewID() != first.Session || child.ViewID() == reused.(sessions.CoordinationSession).ViewID() || store.lookups.Load() != 1 || member.Name != "renamed-exact-lookup" {
		t.Fatalf("stable-ID recovery changed authority: selected=%s reused=%s lookups=%d member=%+v", child.ViewID(), reused.(sessions.CoordinationSession).ViewID(), store.lookups.Load(), member)
	}
}

type lifecycleNarrowOnly struct {
	sessions.CoordinationSession
	sessions.CoordinationRecords
}

func (s lifecycleNarrowOnly) ReadCoordination(context.Context) (*sessions.CoordinationState, error) {
	return nil, errors.New("ordinary lifecycle attempted a family read")
}

func (s lifecycleNarrowOnly) UpdateCoordination(context.Context, func(*sessions.CoordinationState) error) error {
	return errors.New("ordinary lifecycle attempted a family update")
}

// The fixture measures record lifecycle operations, not scheduler/background
// cleanup or tool authority roots. The growing records are retained unrelated
// members/executions/tasks/mail/followup provenance with large owned payloads.
func lifecycleRecordFixture(t *testing.T, unrelated int, legacy bool) (*Runtime, sessions.CoordinationSession) {
	t.Helper()
	store, err := sessions.OpenStore(sessions.StoreConfig{Mode: sessions.ModeMemory})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := store.Acquire(t.Context(), "lifecycle", sessions.AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { parent.Close(); store.Close() })
	root := parent.(sessions.CoordinationSession)
	if err := root.UpdateCoordination(t.Context(), func(raw *sessions.CoordinationState) error {
		raw.Records = map[string]map[string]json.RawMessage{"format": {"swarm": json.RawMessage(`{"version":2}`)}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	handle := root
	if legacy {
		handle = lifecycleLegacySession{parent, root}
	}
	r := &Runtime{ID: root.ViewID(), parent: handle, prepared: true, ctx: t.Context(), notify: make(chan struct{}), active: map[string]*invocation{}, config: Config{MaxExecutions: 10}}
	if err := r.updateRecords(t.Context(), func(tx *recordTx) error {
		if err := tx.PutRun(&Run{ID: "run", Status: "running", Limit: 10, Starts: 1}); err != nil {
			return err
		}
		for n := -1; n < unrelated; n++ {
			id, payload := fmt.Sprintf("retained-%06d", n), strings.Repeat("x", 2048)
			if n == -1 {
				id, payload = "selected", "selected"
			}
			m := &Member{ID: id, AgentName: "/root/" + id, Label: payload, Execution: id, Task: id, ReadOnly: true}
			e := &Execution{ID: id, Member: id, Run: "run", Generation: 7, Status: "completed", Request: AgentRequest{Task: payload, CallID: id, MaxIterations: 5}}
			task := &Task{ID: id, Owner: id, Execution: id, Run: "run", Requirement: RequirementReviewed, Description: payload, Status: "done", Revision: 1}
			if n == -1 {
				e.Status, task.Status = "running", "running"
			}
			for _, err := range []error{tx.PutMember(m), tx.PutExecution(e), tx.PutTask(task), tx.PutMail(&Mail{ID: id, From: r.ID, To: id, Text: payload, Delivered: true}), tx.PutFollowup(id, &FollowupCall{Member: id, Phase: "launched", Task: id, Execution: id})} {
				if err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return r, root
}

func TestRecordLifecycleSelectedPayloadWork(t *testing.T) {
	t.Parallel()
	var baselineBytes, baselineRows, baselineDecodes uint64
	for _, count := range []int{16, 512} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			r, root := lifecycleRecordFixture(t, count, false)
			r.parent = lifecycleNarrowOnly{root, root.(sessions.CoordinationRecords)}
			recorder := runtimemetrics.New()
			ctx := runtimemetrics.WithRecorder(t.Context(), recorder)
			r.ctx = ctx
			if err := r.StopMember(ctx, "selected"); err != nil {
				t.Fatal(err)
			}
			if err := r.Resume(ctx, "", 2); err != nil {
				t.Fatal(err)
			}
			if _, err := r.followupResult(ctx, &Mail{ID: "selected", To: "selected"}); err != nil {
				t.Fatal(err)
			}
			i := &invocation{id: "selected", member: "selected", generation: 7, result: AgentResult{Value: "result"}}
			if again := r.finish(i); again || i.err != nil {
				t.Fatalf("finish: again=%v err=%v", again, i.err)
			}
			observed := recorder.Snapshot()
			bytes := observed.Count(runtimemetrics.PayloadBytesRead)
			if bytes == 0 {
				t.Fatal("native payload observations were not enabled")
			}
			rows, decodes := observed.Count(runtimemetrics.RowsRead), observed.Count(runtimemetrics.RecordsDecoded)
			if count == 16 {
				baselineBytes, baselineRows, baselineDecodes = bytes, rows, decodes
			} else {
				// Finalization captures Ended/Posted. JSON omits trailing zero
				// nanoseconds, so those newly written timestamp lengths vary.
				// Record/row work is exact; bytes permit only that fixed slack,
				// much smaller than even one unrelated 2-KiB payload.
				if rows != baselineRows || decodes != baselineDecodes || bytes > baselineBytes+32 || bytes+32 < baselineBytes {
					t.Fatalf("unrelated retained records changed selected work: bytes=%d/%d rows=%d/%d decodes=%d/%d", baselineBytes, bytes, baselineRows, rows, baselineDecodes, decodes)
				}
			}
			t.Logf("retained unrelated groups=%d native payload bytes=%d rows=%d decodes=%d (actual selected dependencies; not SQL VM visits or whole-runtime cost)", count, bytes, observed.Count(runtimemetrics.RowsRead), observed.Count(runtimemetrics.RecordsDecoded))
		})
	}
}

type lifecycleUncertainLegacy struct {
	sessions.CoordinationSession
	updates, callbacks int
	failure            error
}

func (s *lifecycleUncertainLegacy) UpdateCoordination(ctx context.Context, fn func(*sessions.CoordinationState) error) error {
	s.updates++
	err := s.CoordinationSession.UpdateCoordination(ctx, func(raw *sessions.CoordinationState) error {
		s.callbacks++
		return fn(raw)
	})
	if err != nil {
		return err
	}
	return s.failure
}

type lifecycleUncertainNative struct {
	sessions.CoordinationSession
	sessions.CoordinationRecords
	updates, callbacks int
	failure            error
}

func (s *lifecycleUncertainNative) UpdateRecords(ctx context.Context, fn func(sessions.CoordinationTx) error) (sessions.CoordinationCommit, error) {
	s.updates++
	commit, err := s.CoordinationRecords.UpdateRecords(ctx, func(tx sessions.CoordinationTx) error {
		s.callbacks++
		return fn(tx)
	})
	if err != nil {
		return commit, err
	}
	return commit, s.failure
}

func TestRecordLifecycleUncertainMutationDoesNotReplayOrSignal(t *testing.T) {
	t.Parallel()
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%v", legacy), func(t *testing.T) {
			t.Parallel()
			r, root := lifecycleRecordFixture(t, 1, legacy)
			failure := errors.New("commit response lost")
			var updates, callbacks *int
			if legacy {
				wrapper := &lifecycleUncertainLegacy{CoordinationSession: root, failure: failure}
				r.parent, updates, callbacks = wrapper, &wrapper.updates, &wrapper.callbacks
			} else {
				wrapper := &lifecycleUncertainNative{CoordinationSession: root, CoordinationRecords: root.(sessions.CoordinationRecords), failure: failure}
				r.parent, updates, callbacks = wrapper, &wrapper.updates, &wrapper.callbacks
			}
			notify := r.notify
			if err := r.StopMember(t.Context(), "selected"); !errors.Is(err, failure) {
				t.Fatalf("lost reply became success: %v", err)
			}
			if *updates != 1 || *callbacks != 1 {
				t.Fatalf("uncertain mutation replayed: updates=%d callbacks=%d", *updates, *callbacks)
			}
			select {
			case <-notify:
				t.Fatal("uncertain commit signaled a successful mutation")
			default:
			}
			if err := r.readRecords(t.Context(), func(reader *recordReader) error {
				m, err := reader.Member("selected")
				if err == nil && m.Control != MemberControlStopped {
					t.Fatal("durable receipt missing after lost reply")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRecordLifecycleFinishGenerationFence(t *testing.T) {
	t.Parallel()
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%v", legacy), func(t *testing.T) {
			t.Parallel()
			r, _ := lifecycleRecordFixture(t, 1, legacy)
			i := &invocation{id: "selected", member: "selected", generation: 6, result: AgentResult{Value: "stale"}}
			if r.finish(i) || i.err == nil || !strings.Contains(i.err.Error(), "fenced") {
				t.Fatalf("stale final accepted: %+v", i)
			}
			if err := r.readRecords(t.Context(), func(reader *recordReader) error {
				e, err := reader.Execution("selected")
				if err != nil {
					return err
				}
				if e.Generation != 7 || e.Status != "running" || e.Result != nil {
					t.Fatalf("stale final mutated current execution: %+v", e)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
