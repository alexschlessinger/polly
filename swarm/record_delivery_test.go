package swarm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/internal/runtimemetrics"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/workflow"
)

func TestAtomicRecordSendPinsAndTaskMutations(t *testing.T) {
	t.Parallel()
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%v", legacy), func(t *testing.T) {
			f := recordHotFixture(t, legacy, 32)
			ctx := t.Context()
			// The launch lock cannot become mail authority or serialize Send.
			f.r.launchMu.Lock()
			request, err := f.r.Send(ctx, f.r.ID, f.member, "request", "", "which result?")
			f.r.launchMu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			reply, err := f.r.Send(ctx, f.member, f.r.ID, "reply", request.ID, "this result")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.r.Send(ctx, f.member, f.r.ID, "reply", request.ID, "duplicate"); err == nil {
				t.Fatal("duplicate reply accepted")
			}
			f.read(t, func(rd *recordReader) error {
				m, err := rd.Mail(request.ID)
				if err == nil && (m.ReplyID != reply.ID || m.Start || reply.Start) {
					t.Fatalf("reply/start receipt %+v %+v", m, reply)
				}
				return err
			})
			if _, err := repositoryFor(t, f.parent).Update(ctx, func(tx *recordTx) error {
				m, err := tx.Member(f.member)
				if err != nil {
					return err
				}
				m.Context = "context"
				if err := tx.PutMember(m); err != nil {
					return err
				}
				return tx.PutContext(&ExecutionContext{ID: "context", Owner: f.member, ReadOnly: true, Root: t.TempDir()})
			}); err != nil {
				t.Fatal(err)
			}
			if err := f.r.CancelTask(ctx, "task"); err != nil {
				t.Fatal(err)
			}
			task, err := f.r.CreateTask(ctx, "reviewed task", "", nil, "", CreateTaskOptions{Review: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := f.r.Claim(ctx, f.member, task.ID, task.Revision); err != nil {
				t.Fatal(err)
			}
			if err := f.r.Submit(ctx, f.member, task.ID, task.Revision+1, "submission", ""); err != nil {
				t.Fatal(err)
			}
			if err := f.r.Review(ctx, task.ID, task.Revision+2, true, ""); err != nil {
				t.Fatal(err)
			}
			f.read(t, func(rd *recordReader) error {
				v, err := rd.Task(task.ID)
				if err == nil && (v.Status != "done" || v.AcceptedRevision != task.Revision+2) {
					t.Fatalf("review receipt %+v", v)
				}
				return err
			})
			first, err := f.r.CreateTask(ctx, "first", "", nil, "")
			if err != nil {
				t.Fatal(err)
			}
			second, err := f.r.CreateTask(ctx, "second", "", []string{first.ID}, "")
			if err != nil {
				t.Fatal(err)
			}
			if err := f.r.UpdateTask(ctx, first.ID, 1, "", []string{second.ID}); err == nil {
				t.Fatal("dependency cycle accepted")
			}
			if err := f.r.CancelTask(ctx, first.ID); err != nil {
				t.Fatal(err)
			}
			f.read(t, func(rd *recordReader) error {
				v, err := rd.Task(second.ID)
				if err == nil && (v.Status != "blocked" || v.Revision != 2) {
					t.Fatalf("dependency fanout %+v", v)
				}
				return err
			})
			ref, err := f.child.(sessions.Session).ArtifactStore().Put(ctx, artifacts.Blob{Kind: artifacts.KindText, Data: []byte("pinned evidence")})
			if err != nil {
				t.Fatal(err)
			}
			if err := f.r.pinOutputs(ctx, f.member, []artifacts.Ref{ref}); err != nil {
				t.Fatal(err)
			}
			_, reader, err := f.parent.OpenPublishedArtifact(ctx, ref.ID)
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(reader)
			reader.Close()
			if err != nil || string(data) != "pinned evidence" {
				t.Fatalf("pin: %s %v", data, err)
			}
			if p, err := f.r.Publish(ctx, f.member, Publication{Text: "cannot pin", Artifacts: []artifacts.Ref{{ID: "sha256:missing"}}}); err == nil || p != nil {
				t.Fatalf("failed publication acknowledged %+v %v", p, err)
			}
			if f.nativeParent.legacy.Load() != 0 || f.nativeChild.legacy.Load() != 0 {
				t.Fatal("native wrapper legacy access")
			}
		})
	}
}

func TestAtomicRecordBlockUncertainReceipt(t *testing.T) {
	t.Parallel()
	f := recordHotFixture(t, false, 32)
	if _, err := repositoryFor(t, f.parent).Update(t.Context(), func(tx *recordTx) error {
		m, err := tx.Member(f.member)
		if err != nil {
			return err
		}
		m.Controller = "workflow"
		e, err := tx.Execution("execution")
		if err != nil {
			return err
		}
		e.Workflow = "workflow"
		if err := tx.PutMember(m); err != nil {
			return err
		}
		if err := tx.PutExecution(e); err != nil {
			return err
		}
		return tx.PutWorkflowHeader(&workflow.ReportHeader{ID: "workflow", Run: "run", Status: "running"})
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	f.r.workflowCancels["workflow"] = cancel
	f.nativeParent.mode = "uncertain"
	if err := f.r.BlockTask(t.Context(), f.member, "task", 1, "needs parent"); !errors.Is(err, errHotWrite) {
		t.Fatalf("uncertain error hidden: %v", err)
	}
	if cause := context.Cause(ctx); cause == nil || !strings.Contains(cause.Error(), "needs parent") {
		t.Fatalf("durable blocker did not stop workflow: %v", cause)
	}
	if f.nativeParent.reads.Load() == 0 || f.nativeParent.legacy.Load() != 0 {
		t.Fatal("uncertain receipt not inspected narrowly")
	}
	f.nativeParent.mode = ""
}

func TestAtomicRecordAdmissionPriorityAndExactPromotion(t *testing.T) {
	t.Parallel()
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%v", legacy), func(t *testing.T) {
			f := recordHotFixture(t, legacy, 128)
			ctx := t.Context()
			first, err := f.r.Publish(ctx, f.r.ID, Publication{Key: "same", Value: "v", Text: "first evidence"})
			if err != nil {
				t.Fatal(err)
			}
			// Use a different actual author, but not this reader, for confirmation.
			other, err := f.r.config.Store.Acquire(ctx, "other", sessions.AcquireOptions{Parent: "parent"})
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			otherID := other.(sessions.ViewIdentity).ViewID()
			if _, err := repositoryFor(t, f.parent).Update(ctx, func(tx *recordTx) error {
				if err := tx.PutMember(&Member{ID: otherID, Name: "other", Execution: "other-execution", ReadOnly: true}); err != nil {
					return err
				}
				if err := tx.PutExecution(&Execution{ID: "other-execution", Member: otherID, Run: "run", Status: "completed"}); err != nil {
					return err
				}
				return tx.Effects(sessions.CoordinationEffects{Members: []string{otherID}})
			}); err != nil {
				t.Fatal(err)
			}
			duplicate, err := f.r.Publish(ctx, otherID, Publication{Key: "same", Value: "v", Text: "independent evidence"})
			if err != nil {
				t.Fatal(err)
			}
			if duplicate.DuplicateOf != first.ID {
				t.Fatalf("confirmation %+v", duplicate)
			}
			later, err := f.r.Publish(ctx, f.r.ID, Publication{Key: "different", Value: "later", Text: "later evidence"})
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 15; i++ {
				if _, err := f.r.Send(ctx, f.r.ID, f.member, "info", "", fmt.Sprintf("priority-%02d", i)); err != nil {
					t.Fatal(err)
				}
			}
			cb := &llm.AgentCallbacks{}
			f.r.bindCheckpoint(f.checkpointSession(legacy), "execution", 0, 3, cb, nil, nil)
			input, err := cb.AdmitInput(ctx)
			if err != nil {
				t.Fatal(err)
			}
			receipts := checkpointReceipts(input)
			if len(receipts.MailIDs) != 16 || len(receipts.PublicationIDs) != 0 || len(input[0].Content) > admissionBytes {
				t.Fatalf("mail priority %+v", receipts)
			}
			if err := cb.Checkpoint(ctx, llm.AgentCheckpoint{Generated: input}); err != nil {
				t.Fatal(err)
			}
			next, err := cb.AdmitInput(ctx)
			if err != nil {
				t.Fatal(err)
			}
			ids := checkpointReceipts(next).PublicationIDs
			if len(ids) != 2 || ids[0] != first.ID || ids[1] != later.ID {
				t.Fatalf("coalesced order %v", ids)
			}
			generated := append(input, next...)
			if err := cb.Checkpoint(ctx, llm.AgentCheckpoint{Generated: generated}); err != nil {
				t.Fatal(err)
			}
			// The correction withdraws only its author's predecessor. An old
			// duplicate before the reader's mark must now be admitted exactly once.
			correction, err := f.r.Publish(ctx, f.r.ID, Publication{Supersedes: first.ID, Value: "fixed", Text: "corrected"})
			if err != nil {
				t.Fatal(err)
			}
			promoted, err := cb.AdmitInput(ctx)
			if err != nil {
				t.Fatal(err)
			}
			ids = checkpointReceipts(promoted).PublicationIDs
			if len(ids) != 2 || ids[0] != duplicate.ID || ids[1] != correction.ID {
				t.Fatalf("lost older promotion %v", ids)
			}
			generated = append(generated, promoted...)
			if err := cb.Checkpoint(ctx, llm.AgentCheckpoint{Generated: generated}); err != nil {
				t.Fatal(err)
			}
			if again, err := cb.AdmitInput(ctx); err != nil || len(again) != 0 {
				t.Fatalf("duplicate admission %v %v", again, err)
			}
			f.read(t, func(rd *recordReader) error {
				p, err := rd.Publication(duplicate.ID)
				if err == nil && (len(p.AdmittedTo) != 1 || p.AdmittedTo[0] != f.member || p.DuplicateOf != first.ID) {
					t.Fatalf("exact historical receipt %+v", p)
				}
				return err
			})
		})
	}
}

func TestAtomicRecordUnchangedStateIsRevisionOnly(t *testing.T) {
	t.Parallel()
	f := recordHotFixture(t, false, 1024)
	f.nativeParent.allowSnapshot = true // changed full snapshots are explicitly broad
	first, err := f.r.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	f.nativeParent.allowSnapshot = false
	metric := runtimemetrics.New()
	ctx := runtimemetrics.WithRecorder(t.Context(), metric)
	for i := 0; i < 4; i++ {
		again, err := f.r.State(ctx)
		if err != nil || again != first {
			t.Fatalf("unchanged snapshot %v", err)
		}
	}
	count := metric.Snapshot()
	if count.Count(runtimemetrics.PayloadBytesRead) != 0 || count.Count(runtimemetrics.RecordsDecoded) != 0 || f.nativeParent.legacy.Load() != 0 {
		t.Fatalf("unchanged snapshot decoded payload: %+v", count)
	}
	// A write through the independently opened facade, outside the Runtime
	// cache, must invalidate its durable token.
	if _, err := repositoryFor(t, f.parent).Update(t.Context(), func(tx *recordTx) error {
		task, err := tx.Task("task")
		if err != nil {
			return err
		}
		task.Feedback = "changed"
		return tx.PutTask(task)
	}); err != nil {
		t.Fatal(err)
	}
	f.nativeParent.allowSnapshot = true
	after, err := f.r.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if after == first || first.Tasks["task"].Feedback != "" || after.Tasks["task"].Feedback != "changed" {
		t.Fatal("changed snapshot reused stale state")
	}
}

func TestAtomicRecordPublicationWorkIgnoresUnrelatedHistory(t *testing.T) {
	t.Parallel()
	var baseline runtimemetrics.Snapshot
	for _, n := range []int{0, 128, 1024} {
		t.Run(fmt.Sprintf("unrelated_publications=%d", n), func(t *testing.T) {
			f := recordHotFixture(t, false, 0)
			if _, err := repositoryFor(t, f.parent).Update(t.Context(), func(tx *recordTx) error {
				for i := 0; i < n; i++ {
					p := &Publication{ID: fmt.Sprintf("other-%06d", i), Author: "another-author", Run: "unrelated-run", Key: fmt.Sprintf("unrelated/%d", i), Text: strings.Repeat("history ", 64), Posted: time.Unix(int64(i+1), 0).UTC()}
					if err := tx.PutPublication(p); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			metric := runtimemetrics.New()
			ctx := runtimemetrics.WithRecorder(t.Context(), metric)
			p, err := f.r.Publish(ctx, f.r.ID, Publication{Key: "fresh", Value: "new", Text: "selected evidence"})
			if err != nil {
				t.Fatal(err)
			}
			cb := &llm.AgentCallbacks{}
			f.r.bindCheckpoint(f.nativeChild, "execution", 0, 3, cb, nil, nil)
			input, err := cb.AdmitInput(ctx)
			if err != nil {
				t.Fatal(err)
			}
			ids := checkpointReceipts(input).PublicationIDs
			if len(ids) != 1 || ids[0] != p.ID {
				t.Fatalf("unrelated audience admitted: %v", ids)
			}
			if err := cb.Checkpoint(ctx, llm.AgentCheckpoint{Generated: input}); err != nil {
				t.Fatal(err)
			}
			if again, err := cb.AdmitInput(ctx); err != nil || len(again) != 0 {
				t.Fatalf("unchanged publication polling: %v %v", again, err)
			}
			got := metric.Snapshot()
			if n == 0 {
				baseline = got
			} else {
				for _, counter := range []runtimemetrics.Counter{runtimemetrics.SQLStatements, runtimemetrics.RowsRead, runtimemetrics.RecordsDecoded, runtimemetrics.RecordsEncoded} {
					if got.Count(counter) != baseline.Count(counter) {
						t.Fatalf("unrelated publication growth counter=%v base=%d got=%d", counter, baseline.Count(counter), got.Count(counter))
					}
				}
				// Actual Posted timestamp JSON can lose trailing fractional zeros.
				if got.Count(runtimemetrics.PayloadBytesRead) > baseline.Count(runtimemetrics.PayloadBytesRead)+64 {
					t.Fatal("unrelated publication payload fetched")
				}
			}
			t.Logf("publication publish/admit/receipt/unchanged-poll: unrelated=%d actual audience=1 key fanout=1 SQL=%d rows=%d payload=%d decoded=%d encoded=%d", n, got.Count(runtimemetrics.SQLStatements), got.Count(runtimemetrics.RowsRead), got.Count(runtimemetrics.PayloadBytesRead), got.Count(runtimemetrics.RecordsDecoded), got.Count(runtimemetrics.RecordsEncoded))
		})
	}
}

func TestAtomicRecordDependencyFanoutIsExplicit(t *testing.T) {
	t.Parallel()
	for _, fanout := range []int{1, 24} {
		t.Run(fmt.Sprintf("actual_dependents=%d", fanout), func(t *testing.T) {
			f := recordHotFixture(t, false, 128)
			if _, err := repositoryFor(t, f.parent).Update(t.Context(), func(tx *recordTx) error {
				for i := 0; i < fanout; i++ {
					if err := tx.PutTask(&Task{ID: fmt.Sprintf("dependent-%02d", i), Run: "run", Status: "pending", Revision: 1, Dependencies: []string{"task"}}); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			metric := runtimemetrics.New()
			ctx := runtimemetrics.WithRecorder(t.Context(), metric)
			if err := f.r.CancelTask(ctx, "task"); err != nil {
				t.Fatal(err)
			}
			f.read(t, func(rd *recordReader) error {
				for i := 0; i < fanout; i++ {
					task, err := rd.Task(fmt.Sprintf("dependent-%02d", i))
					if err != nil {
						return err
					}
					if task.Status != "blocked" || task.Revision != 2 {
						t.Fatalf("fanout missed %+v", task)
					}
				}
				return nil
			})
			count := metric.Snapshot()
			t.Logf("actual dependency fanout=%d retained unrelated=256 SQL=%d rows=%d decoded=%d encoded=%d; linear fanout is intended, not an unrelated-history bound", fanout, count.Count(runtimemetrics.SQLStatements), count.Count(runtimemetrics.RowsRead), count.Count(runtimemetrics.RecordsDecoded), count.Count(runtimemetrics.RecordsEncoded))
		})
	}
}

func TestAtomicRecordTaskMutationSchedulesRelease(t *testing.T) {
	t.Parallel()
	f := recordHotFixture(t, false, 0)
	// Model an already-running release worker: the ordinary task method must
	// coalesce a fresh release request even though this component does not run
	// the separate lifecycle sweep.
	f.r.releaseMu.Lock()
	f.r.releaseStopped, f.r.releaseRunning, f.r.releasePending = false, true, false
	f.r.releaseMu.Unlock()
	if err := f.r.CancelTask(t.Context(), "task"); err != nil {
		t.Fatal(err)
	}
	f.r.releaseMu.Lock()
	pending := f.r.releasePending
	f.r.releaseStopped, f.r.releaseRunning = true, false
	f.r.releaseMu.Unlock()
	if !pending {
		t.Fatal("task mutation no longer schedules automatic release")
	}
}

func TestAtomicRecordCrossProcessJournalAndReopen(t *testing.T) {
	const env = "POLLYTOOL_TEST_LOCK_DATABASE"
	if path := os.Getenv(env); path != "" {
		store, err := sessions.OpenStore(sessions.StoreConfig{Mode: sessions.ModeDisk, Path: path})
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		child, err := store.Acquire(t.Context(), "child", sessions.AcquireOptions{ExistingOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		defer child.Close()
		handle := hotNative(child.(sessions.CoordinationSession))
		r := &Runtime{notify: make(chan struct{})}
		cb := &llm.AgentCallbacks{}
		r.bindCheckpoint(handle, "execution", 0, 3, cb, nil, nil)
		intent := messages.ChatMessage{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "remote-effect", Name: "effect", Arguments: `{}`}}}
		if err := cb.JournalToolBatch(t.Context(), llm.AgentCheckpoint{Generated: []messages.ChatMessage{intent}, Iterations: 1}); err != nil {
			t.Fatal(err)
		}
		// Exit at the pre-effect journal boundary; there is no automatic effect
		// replay, and the next process must observe the durable intent.
		return
	}
	t.Parallel()
	f := recordHotFixture(t, false, 16)
	f.nativeParent.allowSnapshot = true
	before, err := f.r.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.child.(sessions.Session).Close(); err != nil {
		t.Fatal(err)
	}
	registry := tools.NewToolRegistry(nil, tools.WithNativeTools(), tools.WithUnsafeNoSandbox())
	defer registry.Close()
	if _, err := registry.LoadToolAuto("bash"); err != nil {
		t.Fatal(err)
	}
	bash, ok := registry.Get("bash")
	if !ok {
		t.Fatal("fixture bash unavailable")
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	command := env + "=" + quote(f.path) + " " + quote(os.Args[0]) + " -test.run='^TestAtomicRecordCrossProcessJournalAndReopen$'"
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	// Use the ordinary process-tool factory and its explicit test policy,
	// not an ad-hoc exec path or a production sandbox exception.
	if output, err := bash.Execute(ctx, map[string]any{"command": command}); err != nil {
		t.Fatalf("helper: %v\n%s", err, output)
	}
	after, err := f.r.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if after == before || len(before.Executions["execution"].Intent) != 0 || len(after.Executions["execution"].Intent) != 1 {
		t.Fatal("cross-process journal revision was stale")
	}
	// Reopen the member and checkpoint the interrupted receipt. The uncertain
	// tool is reported, never executed again by this recovery path.
	child, err := f.r.config.Store.Acquire(t.Context(), "child", sessions.AcquireOptions{ExpectedID: f.member})
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	cb := &llm.AgentCallbacks{}
	f.r.bindCheckpoint(hotNative(child.(sessions.CoordinationSession)), "execution", 0, 3, cb, nil, nil)
	generated := append([]messages.ChatMessage{}, after.Executions["execution"].Intent...)
	generated = append(generated, messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: "remote-effect", ToolName: "effect", Content: llm.ToolInterruptedContent})
	if err := cb.Checkpoint(t.Context(), llm.AgentCheckpoint{Generated: generated}); err != nil {
		t.Fatal(err)
	}
	history, err := child.GetHistory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[1].Content != llm.ToolInterruptedContent {
		t.Fatalf("reopened recovery history %+v", history)
	}
}

func TestAtomicRecordOversizedMailStillAdmitsBoundedPreview(t *testing.T) {
	t.Parallel()
	f := recordHotFixture(t, false, 0)
	if _, err := repositoryFor(t, f.parent).Update(t.Context(), func(tx *recordTx) error {
		mail, err := tx.Mail("mail")
		if err != nil {
			return err
		}
		mail.Text = strings.Repeat("x", 2*admissionBytes)
		return tx.PutMail(mail)
	}); err != nil {
		t.Fatal(err)
	}
	cb := &llm.AgentCallbacks{}
	f.r.bindCheckpoint(f.nativeChild, "execution", 0, 3, cb, nil, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	input, err := cb.AdmitInput(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(input) != 1 || len(input[0].Content) > admissionBytes || !strings.Contains(input[0].Content, "Full message:") || len(checkpointReceipts(input).MailIDs) != 1 {
		t.Fatalf("oversized admission %+v", input)
	}
	if err := cb.Checkpoint(ctx, llm.AgentCheckpoint{Generated: input}); err != nil {
		t.Fatal(err)
	}
	f.read(t, func(rd *recordReader) error {
		mail, err := rd.Mail("mail")
		if err == nil && (!mail.Delivered || len(mail.Text) != 2*admissionBytes) {
			t.Fatal("oversized durable original changed")
		}
		return err
	})
}
