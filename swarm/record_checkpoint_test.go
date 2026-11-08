package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
)

var errHotLegacy = errors.New("hot operation attempted direct legacy access")
var errHotWrite = errors.New("injected record commit failure")

// Reject actual methods, not just zero counters: context-free legacy wrappers
// are deliberately uninstrumented and could otherwise hide a broad fallback.
type hotNativeSession struct {
	sessions.CoordinationSession
	records               sessions.CoordinationRecords
	mode                  string
	allowSnapshot         bool
	mailPosted            time.Time // deterministic input for exact payload-work comparisons
	afterCommit           chan struct{}
	reads, writes, legacy atomic.Int64
}

func hotNative(handle sessions.CoordinationSession) *hotNativeSession {
	return &hotNativeSession{CoordinationSession: handle, records: handle.(sessions.CoordinationRecords)}
}
func (s *hotNativeSession) CoordinationCapabilities() sessions.CoordinationCapabilities {
	return sessions.CoordinationCapabilities{NativeIndexed: true, DurableRevisions: true}
}
func (s *hotNativeSession) ReadCoordination(context.Context) (*sessions.CoordinationState, error) {
	s.legacy.Add(1)
	return nil, errHotLegacy
}
func (s *hotNativeSession) UpdateCoordination(context.Context, func(*sessions.CoordinationState) error) error {
	s.legacy.Add(1)
	return errHotLegacy
}
func (s *hotNativeSession) query(r sessions.CoordinationReader, q sessions.RecordQuery) (sessions.RecordPage, error) {
	if !s.allowSnapshot && (q.Index == "" || q.Index == sessions.CoordinationIndexRecords) {
		return sessions.RecordPage{}, errHotLegacy
	}
	return r.Query(q)
}

type hotNativeReader struct {
	sessions.CoordinationReader
	owner *hotNativeSession
}

func (r hotNativeReader) Query(q sessions.RecordQuery) (sessions.RecordPage, error) {
	return r.owner.query(r.CoordinationReader, q)
}

type hotNativeTx struct {
	sessions.CoordinationTx
	owner *hotNativeSession
}

func (r hotNativeTx) Query(q sessions.RecordQuery) (sessions.RecordPage, error) {
	return r.owner.query(r.CoordinationTx, q)
}
func (r hotNativeTx) Put(key sessions.RecordKey, payload json.RawMessage) error {
	if key.Kind == "mail" && !r.owner.mailPosted.IsZero() {
		// Send uses wall time. RFC3339Nano drops trailing fractional zeros, so
		// otherwise equal workloads can differ by a few bytes after derivation
		// rereads the new mail. Fix the input, not the measured byte assertion.
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(payload, &fields); err != nil {
			return err
		}
		fields["posted"], _ = json.Marshal(r.owner.mailPosted)
		var err error
		payload, err = json.Marshal(fields)
		if err != nil {
			return err
		}
	}
	return r.CoordinationTx.Put(key, payload)
}
func (s *hotNativeSession) ReadRecords(ctx context.Context, fn func(sessions.CoordinationReader) error) error {
	s.reads.Add(1)
	return s.records.ReadRecords(ctx, func(r sessions.CoordinationReader) error { return fn(hotNativeReader{r, s}) })
}
func (s *hotNativeSession) UpdateRecords(ctx context.Context, fn func(sessions.CoordinationTx) error) (sessions.CoordinationCommit, error) {
	s.writes.Add(1)
	commit, err := s.records.UpdateRecords(ctx, func(tx sessions.CoordinationTx) error {
		if err := fn(hotNativeTx{tx, s}); err != nil {
			return err
		}
		if s.mode == "rollback" {
			return errHotWrite
		}
		return nil
	})
	if err == nil && s.afterCommit != nil {
		close(s.afterCommit)
	}
	if err == nil && s.mode == "uncertain" {
		return sessions.CoordinationCommit{Sequence: 999999, Changed: commit.Changed}, errHotWrite
	}
	return commit, err
}

type hotFixture struct {
	r                         *Runtime
	parent, child             sessions.CoordinationSession
	nativeParent, nativeChild *hotNativeSession
	member                    string
	path                      string
}

func recordHotFixture(t *testing.T, legacy bool, retained int) hotFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "records.db")
	store, err := sessions.OpenStore(sessions.StoreConfig{Mode: sessions.ModeDisk, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := store.Acquire(t.Context(), "parent", sessions.AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	registry := tools.NewToolRegistry(nil)
	r, err := New(Config{Store: store, Parent: parent, Registry: registry, OpenTools: tools.NativeOpenTools(registry), Client: doneModel(), Root: t.TempDir(), Directory: filepath.Join(t.TempDir(), "runtime")})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.Acquire(t.Context(), "child", sessions.AcquireOptions{Parent: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	id := child.(sessions.ViewIdentity).ViewID()
	started := time.Unix(200, 0).UTC()
	if err := r.update(t.Context(), func(s *State) error {
		s.Runs["run"] = &Run{ID: "run", Status: "running", Limit: 100}
		s.Members[id] = &Member{ID: id, Name: "child", Execution: "execution", Task: "task", ReadOnly: true}
		s.Executions["execution"] = &Execution{ID: "execution", Member: id, Run: "run", Generation: 3, Status: "running", InputSaved: true, Started: started, ResumedAt: started, ResumedBy: "parent"}
		s.Tasks["task"] = &Task{ID: "task", Run: "run", Owner: id, Execution: "execution", Status: "running", Requirement: RequirementDelivered, Revision: 1}
		s.Messages["mail"] = &Mail{ID: "mail", From: r.ID, To: id, Kind: "info", Text: "exact staged input", Posted: started}
		for i := 0; i < retained; i++ {
			key := fmt.Sprintf("unrelated-%06d", i)
			s.Tasks[key] = &Task{ID: key, Run: "old-run", Status: "done", Revision: 1, Result: string(make([]byte, 512))}
			s.Messages[key] = &Mail{ID: key, From: r.ID, To: "unrelated-reader", Kind: "info", Delivered: true, Text: "retained history", Posted: started}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Isolate the atomic operation boundary. Automatic workspace release is
	// an independently scheduled, still-broad lifecycle path and is tested by
	// its own unchanged suite; these counts are not end-to-end lifecycle claims.
	r.releaseStopped = true
	f := hotFixture{r: r, parent: parent.(sessions.CoordinationSession), child: child.(sessions.CoordinationSession), member: id, path: path}
	f.nativeParent, f.nativeChild = hotNative(f.parent), hotNative(f.child)
	if legacy {
		r.parent = repositoryLegacyOnly{f.parent}
	} else {
		r.parent = f.nativeParent
	}
	t.Cleanup(func() { r.Close(); child.Close(); parent.Close(); registry.Close(); store.Close() })
	return f
}
func (f hotFixture) checkpointSession(legacy bool) sessions.CoordinationSession {
	if legacy {
		return repositoryLegacyOnly{f.child}
	}
	return f.nativeChild
}
func (f hotFixture) read(t *testing.T, fn func(*recordReader) error) {
	t.Helper()
	if err := repositoryFor(t, f.parent).Read(t.Context(), fn); err != nil {
		t.Fatal(err)
	}
}

func TestAtomicRecordCheckpointParity(t *testing.T) {
	t.Parallel()
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%v", legacy), func(t *testing.T) {
			f := recordHotFixture(t, legacy, 64)
			cb := &llm.AgentCallbacks{}
			approved := false
			cb.BeforeToolBatch = func(context.Context, []messages.ChatMessageToolCall) error { approved = true; return nil }
			f.r.bindCheckpoint(f.checkpointSession(legacy), "execution", 2, 3, cb, nil, nil)
			input, err := cb.AdmitInput(t.Context())
			if err != nil || len(input) != 1 {
				t.Fatalf("admit: %v %v", input, err)
			}
			f.read(t, func(rd *recordReader) error {
				m, err := rd.Mail("mail")
				if err == nil && m.Delivered {
					t.Fatal("staging acknowledged mail")
				}
				return err
			})
			calls := []messages.ChatMessageToolCall{{ID: "tool-call", Name: "effect", Arguments: `{}`}}
			if err := cb.BeforeToolBatch(t.Context(), calls); err != nil || !approved {
				t.Fatalf("approval: %v", err)
			}
			intent := messages.ChatMessage{Role: messages.MessageRoleAssistant, ToolCalls: calls}
			if err := cb.JournalToolBatch(t.Context(), llm.AgentCheckpoint{Generated: []messages.ChatMessage{intent}, Iterations: 1}); err != nil {
				t.Fatal(err)
			}
			f.read(t, func(rd *recordReader) error {
				e, err := rd.Execution("execution")
				if err == nil && len(e.Intent) != 1 {
					t.Fatal("pre-effect journal absent")
				}
				return err
			})
			generated := append(input, messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "accepted", Metadata: map[string]any{messages.MetadataKeyInputTokens: 3, messages.MetadataKeyOutputTokens: 5}})
			if err := cb.Checkpoint(t.Context(), llm.AgentCheckpoint{Generated: generated, Iterations: 1, Request: true}); err != nil {
				t.Fatal(err)
			}
			f.read(t, func(rd *recordReader) error {
				e, err := rd.Execution("execution")
				if err != nil {
					return err
				}
				m, err := rd.Mail("mail")
				if err != nil {
					return err
				}
				if !m.Delivered || len(e.Intent) != 0 || e.Iterations != 4 || e.Usage.Samples != 1 || e.Started != time.Unix(200, 0).UTC() || e.ResumedAt != e.Started {
					t.Fatalf("checkpoint: mail=%+v execution=%+v", m, e)
				}
				return nil
			})
			view, err := f.r.config.Store.(sessions.ViewStore).ReadView(t.Context(), sessions.ViewTarget{ID: f.member}, "")
			if err != nil {
				t.Fatal(err)
			}
			if view.Sequence != 2 || view.Metadata.Transcript == nil || *view.Metadata.Transcript != (sessions.TranscriptIdentity{Execution: "execution", Generation: 3, Sequence: 2}) {
				t.Fatalf("transcript identity: %+v", view)
			}
			if !legacy && (f.nativeChild.reads.Load() == 0 || f.nativeChild.writes.Load() == 0 || f.nativeParent.legacy.Load()+f.nativeChild.legacy.Load() != 0) {
				t.Fatal("did not exercise supplied native capability")
			}
		})
	}
}

func TestAtomicRecordUncertainCheckpointDoesNotAcknowledgeOrReplay(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"rollback", "uncertain"} {
		t.Run(mode, func(t *testing.T) {
			f := recordHotFixture(t, false, 16)
			cb := &llm.AgentCallbacks{}
			f.r.bindCheckpoint(f.nativeChild, "execution", 0, 3, cb, nil, nil)
			input, err := cb.AdmitInput(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			c := llm.AgentCheckpoint{Generated: input, Iterations: 1}
			f.nativeChild.mode = mode
			before := f.r.notify
			if err := cb.Checkpoint(t.Context(), c); !errors.Is(err, errHotWrite) {
				t.Fatalf("failed write acknowledged: %v", err)
			}
			select {
			case <-before:
				t.Fatal("failed/uncertain commit notified success")
			default:
			}
			f.nativeChild.mode = ""
			err = cb.Checkpoint(t.Context(), c)
			if (err == nil) != (mode == "rollback") {
				t.Fatalf("first sequence not preserved: %v", err)
			}
			history, err := f.child.(sessions.Session).GetHistory(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if len(history) != 1 {
				t.Fatalf("replayed append: %d", len(history))
			}
			f.read(t, func(rd *recordReader) error {
				mail, err := rd.Mail("mail")
				if err == nil && !mail.Delivered {
					t.Fatal("committed exact receipt missing")
				}
				return err
			})
		})
	}
}

func TestAtomicRecordCheckpointFencingAndJournalVeto(t *testing.T) {
	t.Parallel()
	f := recordHotFixture(t, false, 8)
	cb := &llm.AgentCallbacks{BeforeToolBatch: func(context.Context, []messages.ChatMessageToolCall) error { return errHotWrite }}
	f.r.bindCheckpoint(f.nativeChild, "execution", 0, 2, cb, nil, nil)
	if err := cb.BeforeToolBatch(t.Context(), nil); !errors.Is(err, errHotWrite) {
		t.Fatal(err)
	}
	if f.nativeChild.reads.Load() != 0 {
		t.Fatal("approval veto reached storage")
	}
	if err := cb.JournalToolBatch(t.Context(), llm.AgentCheckpoint{}); err == nil {
		t.Fatal("stale generation journaled")
	}
	if err := cb.Checkpoint(t.Context(), llm.AgentCheckpoint{Generated: []messages.ChatMessage{answer("stale")}}); err == nil {
		t.Fatal("stale generation appended")
	}
	if err := f.child.(sessions.Session).Close(); err != nil {
		t.Fatal(err)
	}
	cb = &llm.AgentCallbacks{}
	f.r.bindCheckpoint(f.nativeChild, "execution", 0, 3, cb, nil, nil)
	if err := cb.BeforeToolBatch(t.Context(), nil); err == nil {
		t.Fatal("closed lease authorized tool effect")
	}
}

func TestAtomicRecordFinalRetryAndStructuredReceipts(t *testing.T) {
	t.Parallel()
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%v", legacy), func(t *testing.T) {
			f := recordHotFixture(t, legacy, 32)
			handle := f.checkpointSession(legacy)
			final := &llm.AgentCallbacks{}
			f.r.bindMemberFinal(handle, "execution", 3, 5, "", final)
			if input, err := final.ContinueAfterFinal(t.Context(), nil); err != nil || len(input) != 1 {
				t.Fatalf("blank final reservation %v %v", input, err)
			}
			if _, err := final.ContinueAfterFinal(t.Context(), nil); !errors.Is(err, ErrEmptyResult) {
				t.Fatalf("blank final retried twice: %v", err)
			}
			if _, err := repositoryFor(t, f.parent).Update(t.Context(), func(tx *recordTx) error {
				e, err := tx.Execution("execution")
				if err != nil {
					return err
				}
				e.Request.Schema = map[string]any{"type": "object"}
				return tx.PutExecution(e)
			}); err != nil {
				t.Fatal(err)
			}
			structured := &structuredResultState{task: "task", accepted: &StructuredCompletion{Task: "task", Value: map[string]any{"result": "typed"}}, acceptedText: `{"result":"typed"}`}
			cb := &llm.AgentCallbacks{}
			f.r.bindCheckpoint(handle, "execution", 0, 3, cb, structured, nil)
			if err := cb.Checkpoint(t.Context(), llm.AgentCheckpoint{Generated: []messages.ChatMessage{answer("wrong receipt")}}); err == nil {
				t.Fatal("typed completion without retained receipt committed")
			}
			if err := cb.Checkpoint(t.Context(), llm.AgentCheckpoint{Generated: []messages.ChatMessage{answer(structured.acceptedText)}}); err != nil {
				t.Fatal(err)
			}
			f.read(t, func(rd *recordReader) error {
				e, err := rd.Execution("execution")
				if err == nil && (e.Completion == nil || !e.EmptyFinalRetried) {
					t.Fatalf("final bookkeeping %+v", e)
				}
				return err
			})
			// A subsequent exhausted boundary atomically saves the partial result,
			// paused execution, blocked task and parent notice; no model replay.
			cb = &llm.AgentCallbacks{}
			f.r.bindCheckpoint(handle, "execution", 1, 3, cb, nil, nil)
			cause := &llm.RetryExhaustedError{Err: errors.New("transient"), Attempts: 2, RetryCount: 1, NextRetryAt: time.Unix(999, 0).UTC()}
			if err := cb.Checkpoint(t.Context(), llm.AgentCheckpoint{Generated: []messages.ChatMessage{answer("partial")}, Final: true, Err: cause}); err != nil {
				t.Fatal(err)
			}
			f.read(t, func(rd *recordReader) error {
				e, err := rd.Execution("execution")
				if err != nil {
					return err
				}
				task, err := rd.Task("task")
				if err != nil {
					return err
				}
				if e.Status != "paused" || e.RetryPause == nil || e.RetryPause.Attempts != 2 || task.Status != "blocked" || task.Result != "partial" || e.Result == nil || e.Result.Revision != task.Revision {
					t.Fatalf("retry receipt execution=%+v task=%+v", e, task)
				}
				mail, err := rd.Mails(sessions.RecordQuery{Index: sessions.CoordinationIndexMailPending, Key: f.r.ID, Limit: 16})
				if err != nil {
					return err
				}
				if len(mail.Records) != 1 || mail.Records[0].Value.Task != "task" {
					t.Fatalf("retry notice %+v", mail)
				}
				return nil
			})
		})
	}
}

func TestAtomicRecordCheckpointNotifiesOutsideLiveTailLock(t *testing.T) {
	t.Parallel()
	f := recordHotFixture(t, false, 0)
	cb := &llm.AgentCallbacks{}
	invocation := &invocation{id: "execution", member: f.member, generation: 3}
	binding := invocation.bindTranscript(cb, f.member)
	f.r.bindCheckpoint(f.nativeChild, "execution", 0, 3, cb, nil, binding)
	f.nativeChild.afterCommit = make(chan struct{})
	f.r.mu.Lock()
	done := make(chan error, 1)
	go func() {
		done <- cb.Checkpoint(t.Context(), llm.AgentCheckpoint{Generated: []messages.ChatMessage{answer("saved")}})
	}()
	select {
	case <-f.nativeChild.afterCommit:
	case <-time.After(5 * time.Second):
		f.r.mu.Unlock()
		<-done
		t.Fatal("checkpoint did not commit")
	}
	liveRead := make(chan int64, 1)
	go func() {
		binding.live.mu.Lock()
		liveRead <- binding.live.snapshot.BaseSequence
		binding.live.mu.Unlock()
	}()
	var sequence int64
	blocked := false
	select {
	case sequence = <-liveRead:
	case <-time.After(5 * time.Second):
		blocked = true
	}
	f.r.mu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if blocked {
		<-liveRead
		t.Fatal("notification held live-tail mutex while waiting for Runtime lock")
	}
	if sequence != 1 {
		t.Fatalf("live tail not acknowledged before notification: %d", sequence)
	}
}

func TestAtomicRecordNativeWorkDoesNotGrowWithHistory(t *testing.T) {
	t.Parallel()
	var baseline runtimemetrics.Snapshot
	for _, n := range []int{0, 128, 1024} {
		t.Run(fmt.Sprintf("unrelated_records=%d", 2*n), func(t *testing.T) {
			f := recordHotFixture(t, false, n)
			f.nativeParent.mailPosted = time.Unix(200, 0).UTC()
			f.nativeChild.mailPosted = f.nativeParent.mailPosted
			metric := runtimemetrics.New()
			ctx := runtimemetrics.WithRecorder(t.Context(), metric)
			cb := &llm.AgentCallbacks{}
			f.r.bindCheckpoint(f.nativeChild, "execution", 0, 3, cb, nil, nil)
			input, err := cb.AdmitInput(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := cb.BeforeToolBatch(ctx, nil); err != nil {
				t.Fatal(err)
			}
			if err := cb.JournalToolBatch(ctx, llm.AgentCheckpoint{}); err != nil {
				t.Fatal(err)
			}
			if err := cb.Checkpoint(ctx, llm.AgentCheckpoint{Generated: input}); err != nil {
				t.Fatal(err)
			}
			if _, err := f.r.Send(ctx, f.member, f.r.ID, "info", "", "addressed"); err != nil {
				t.Fatal(err)
			}
			got := metric.Snapshot()
			if n == 0 {
				baseline = got
			} else {
				for _, counter := range []runtimemetrics.Counter{runtimemetrics.SQLStatements, runtimemetrics.RowsRead, runtimemetrics.PayloadBytesRead, runtimemetrics.RecordsDecoded, runtimemetrics.RecordsEncoded} {
					if got.Count(counter) != baseline.Count(counter) {
						t.Fatalf("unrelated growth counter=%v base=%d got=%d", counter, baseline.Count(counter), got.Count(counter))
					}
				}
			}
			t.Logf("native checkpoint+admission+journal+Send: retained unrelated=%d SQL=%d rows=%d payload=%d decodes=%d encodes=%d; actual audience=1 staged mail=1, no throughput or combined-scale claim", 2*n, got.Count(runtimemetrics.SQLStatements), got.Count(runtimemetrics.RowsRead), got.Count(runtimemetrics.PayloadBytesRead), got.Count(runtimemetrics.RecordsDecoded), got.Count(runtimemetrics.RecordsEncoded))
		})
	}
}

func TestAtomicRecordParentJournalFormatException(t *testing.T) {
	t.Parallel()
	for _, extra := range []bool{false, true} {
		t.Run(fmt.Sprintf("extra=%v", extra), func(t *testing.T) {
			r := runtimeTest(t, doneModel(), 1, 1)
			if extra {
				if err := r.parent.UpdateCoordination(t.Context(), func(raw *sessions.CoordinationState) error {
					raw.Records["custom"] = map[string]json.RawMessage{"forbidden": json.RawMessage(`{}`)}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			h := hotNative(r.parent)
			h.allowSnapshot = true
			cb := &llm.AgentCallbacks{}
			r.bindCheckpoint(h, "", 0, 0, cb, nil, nil)
			err := cb.JournalToolBatch(t.Context(), llm.AgentCheckpoint{Generated: []messages.ChatMessage{answer("intent")}})
			if extra {
				if err == nil {
					t.Fatal("formatless nonjournal records accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if r.prepared {
				t.Fatal("parent journal promoted/prepared swarm")
			}
			if err := cb.Checkpoint(t.Context(), llm.AgentCheckpoint{Generated: []messages.ChatMessage{answer("accepted")}}); err != nil {
				t.Fatal(err)
			}
			raw, err := r.parent.ReadCoordination(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if len(raw.Records["format"]) != 0 || len(raw.Records["parent_turn"]) != 0 {
				t.Fatalf("journal bootstrapped swarm: %+v", raw.Records)
			}
		})
	}
}
