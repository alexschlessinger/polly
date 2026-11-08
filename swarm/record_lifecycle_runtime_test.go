package swarm

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
)

// Guard both parent and child handles. A narrow-looking operation must not
// recover a whole family through Query or a hidden legacy child capability.
type lifecycleRuntimeHandle struct {
	sessions.Session
	lifecycleNarrowOnly
	rows *atomic.Int64
}

func (s lifecycleRuntimeHandle) ReadRecords(ctx context.Context, fn func(sessions.CoordinationReader) error) error {
	return s.CoordinationRecords.ReadRecords(ctx, func(rd sessions.CoordinationReader) error {
		return fn(lifecycleRuntimeReader{rd, s.rows})
	})
}
func (s lifecycleRuntimeHandle) UpdateRecords(ctx context.Context, fn func(sessions.CoordinationTx) error) (sessions.CoordinationCommit, error) {
	return s.CoordinationRecords.UpdateRecords(ctx, func(tx sessions.CoordinationTx) error {
		return fn(lifecycleRuntimeTx{CoordinationTx: tx, lifecycleRuntimeReader: lifecycleRuntimeReader{tx, s.rows}})
	})
}

type lifecycleRuntimeReader struct {
	sessions.CoordinationReader
	rows *atomic.Int64
}

func (r lifecycleRuntimeReader) Get(key sessions.RecordKey) (sessions.CoordinationRecord, bool, error) {
	record, found, err := r.CoordinationReader.Get(key)
	if found {
		r.rows.Add(1)
	}
	return record, found, err
}
func (r lifecycleRuntimeReader) Query(q sessions.RecordQuery) (sessions.RecordPage, error) {
	if q.Index == "" || q.Index == sessions.CoordinationIndexRecords || q.Index == sessions.CoordinationIndexKind && q.Key != "context" {
		return sessions.RecordPage{}, fmt.Errorf("ordinary lifecycle selected a broad partition: %+v", q)
	}
	page, err := r.CoordinationReader.Query(q)
	r.rows.Add(int64(len(page.Records)))
	return page, err
}

type lifecycleRuntimeTx struct {
	sessions.CoordinationTx
	lifecycleRuntimeReader
}

func (tx lifecycleRuntimeTx) Identity() (sessions.CoordinationIdentity, error) {
	return tx.lifecycleRuntimeReader.Identity()
}
func (tx lifecycleRuntimeTx) Get(k sessions.RecordKey) (sessions.CoordinationRecord, bool, error) {
	return tx.lifecycleRuntimeReader.Get(k)
}
func (tx lifecycleRuntimeTx) Query(q sessions.RecordQuery) (sessions.RecordPage, error) {
	return tx.lifecycleRuntimeReader.Query(q)
}
func (tx lifecycleRuntimeTx) Revisions(s []sessions.RevisionScope) ([]sessions.Revision, error) {
	return tx.lifecycleRuntimeReader.Revisions(s)
}

type lifecycleRuntimeStore struct {
	sessions.SessionStore
	rows *atomic.Int64
}

func (s lifecycleRuntimeStore) Acquire(ctx context.Context, name string, opts sessions.AcquireOptions) (sessions.Session, error) {
	handle, err := s.SessionStore.Acquire(ctx, name, opts)
	if err != nil {
		return nil, err
	}
	return lifecycleRuntimeHandle{handle, lifecycleNarrowOnly{handle.(sessions.CoordinationSession), handle.(sessions.CoordinationRecords)}, s.rows}, nil
}

func TestRecordLifecycleWholeExecutionAndFollowup(t *testing.T) {
	t.Parallel()
	var baseline int64
	for _, unrelated := range []int{4, 512} {
		t.Run(fmt.Sprint(unrelated), func(t *testing.T) {
			var calls atomic.Int64
			r := runtimeTest(t, modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
				calls.Add(1)
				return answer("retained result")
			}), 1, 5)
			suspendAutoRelease(t, r)
			if err := r.update(t.Context(), func(s *State) error {
				for n := range unrelated {
					id := fmt.Sprintf("unrelated-%06d", n)
					s.Members[id] = &Member{ID: id, Execution: id, Task: id, AgentName: "/root/" + id}
					s.Executions[id] = &Execution{ID: id, Member: id, Status: "completed", Run: id}
					s.Tasks[id] = &Task{ID: id, Owner: id, Execution: id, Run: id, Status: "done"}
					s.Messages[id] = &Mail{ID: id, From: r.ID, To: id, Delivered: true}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			parent, store, root := r.parent, r.config.Store, r.config.Parent
			var rows atomic.Int64
			handle := lifecycleRuntimeHandle{root, lifecycleNarrowOnly{parent, parent.(sessions.CoordinationRecords)}, &rows}
			r.parent, r.config.Parent = handle, handle
			r.config.Store = lifecycleRuntimeStore{store, &rows}
			defer func() { r.parent, r.config.Parent, r.config.Store = parent, root, store }()
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			first, err := r.Agent(ctx, "", AgentRequest{TaskName: "bounded", Label: "Bounded worker", Task: "inspect", ReadOnly: true, CallID: "first"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.Agent(ctx, "", AgentRequest{TaskName: "bounded", Label: "Bounded worker", Task: "inspect", ReadOnly: true, CallID: "first"}); err != nil {
				t.Fatal(err)
			}
			admitParent(t, r)
			mail, err := r.followupTask(ctx, first.Session, "inspect again", "followup", false)
			if err != nil {
				t.Fatal(err)
			}
			r.mu.Lock()
			active := r.active[first.Session]
			r.mu.Unlock()
			if active != nil {
				select {
				case <-active.done:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			admitParent(t, r)
			replay, err := r.followupTask(ctx, first.Session, "inspect again", "followup", false)
			if err != nil || replay == nil || replay.ID != mail.ID || calls.Load() != 2 {
				t.Fatalf("followup replay: %+v calls=%d err=%v", replay, calls.Load(), err)
			}
			if err := r.readRecords(ctx, func(rd *recordReader) error {
				m, err := rd.Member(first.Session)
				if err != nil {
					return err
				}
				e, err := rd.Execution(m.Execution)
				if err != nil {
					return err
				}
				task, err := rd.Task(m.Task)
				if err != nil {
					return err
				}
				run, err := rd.Run(e.Run)
				if err != nil {
					return err
				}
				if task.Status != "done" || task.Follows != first.Task || e.Status != "completed" || run.Starts != 2 {
					t.Fatalf("lost lifecycle state: task=%+v execution=%+v run=%+v", task, e, run)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if unrelated == 4 {
				baseline = rows.Load()
			} else if got := rows.Load(); got != baseline {
				t.Fatalf("whole-execution record work amplified: %d -> %d", baseline, got)
			}
			t.Logf("unrelated groups=%d two complete executions and replay rows=%d; cold preparation and auto-release excluded, context roots retained", unrelated, rows.Load())
		})
	}
}

func TestRecordLifecyclePreservesPagedSecurityRoots(t *testing.T) {
	t.Parallel()
	r := runtimeTest(t, nilModel(), 1, 1)
	suspendAutoRelease(t, r)
	var roots []string
	root := t.TempDir()
	if err := r.updateRecords(t.Context(), func(tx *recordTx) error {
		for n := range 225 {
			id := fmt.Sprintf("sibling-%03d", n)
			path := filepath.Join(root, id)
			roots = append(roots, path)
			if err := tx.PutContext(&ExecutionContext{ID: id, Root: path, ReadOnly: true}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	parent := r.parent
	r.parent = lifecycleNarrowOnly{parent, parent.(sessions.CoordinationRecords)}
	defer func() { r.parent = parent }()
	refused := errors.New("stop before opening tools")
	var denied []string
	r.config.OpenTools = func(_ context.Context, scope tools.ToolScope) (tools.ToolBinding, error) {
		denied = append([]string(nil), scope.Grant.DeniedReads...)
		return tools.ToolBinding{}, refused
	}
	_, err := r.Agent(t.Context(), "", AgentRequest{Label: "Security roots", Task: "inspect", ReadOnly: true})
	if !errors.Is(err, refused) {
		t.Fatalf("unexpected preparation result: %v", err)
	}
	for _, root := range roots {
		if !slices.Contains(denied, root) {
			t.Fatalf("lost sibling security mask %s", root)
		}
	}
}
