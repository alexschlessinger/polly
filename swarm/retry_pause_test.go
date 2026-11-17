package swarm

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/subagent"
)

type retryPauseModel func(context.Context, *llm.CompletionRequest) (messages.ChatMessage, error)

func (f retryPauseModel) ChatCompletionStream(ctx context.Context, req *llm.CompletionRequest, p llm.EventStreamProcessor) <-chan *messages.StreamEvent {
	message, err := f(ctx, req)
	if err != nil {
		events := make(chan *messages.StreamEvent, 1)
		events <- &messages.StreamEvent{Type: messages.EventTypeError, Error: err}
		close(events)
		return events
	}
	return modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage { return message }).ChatCompletionStream(ctx, req, p)
}

func exhaustedRetry(at time.Time) *llm.RetryExhaustedError {
	return &llm.RetryExhaustedError{Err: errors.New("provider temporarily unavailable"), Attempts: 4, RetryCount: 3, NextRetryAt: at}
}

func promoteRetryTest(t *testing.T, r *Runtime) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pause.db")
	r.config.Promote = func(ctx context.Context) error {
		return r.config.Store.(sessions.DurableStore).Promote(ctx, path)
	}
	return path
}

func reopenRetryTest(t *testing.T, r *Runtime, path string) *Runtime {
	t.Helper()
	config := r.config
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.config.Parent.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.config.Store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := sessions.OpenStore(sessions.StoreConfig{Mode: sessions.ModeDisk, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	parent, err := store.Acquire(context.Background(), "parent", sessions.AcquireOptions{ExpectedID: r.ID, ExistingOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { parent.Close() })
	config.Store, config.Parent, config.Promote = store, parent, nil
	recovered, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { recovered.Close() })
	return recovered
}

func TestRetryPauseClassification(t *testing.T) {
	t.Parallel()
	exhausted := exhaustedRetry(time.Now().UTC())
	timeout := *exhausted
	timeout.Err = fmt.Errorf("request timeout: %w", context.DeadlineExceeded)
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"exhausted", exhausted, true},
		{"wrapped", fmt.Errorf("model call: %w", exhausted), true},
		{"permanent", errors.New("bad credentials"), false},
		{"canceled", errors.Join(exhausted, context.Canceled), false},
		{"deadline", errors.Join(exhausted, context.DeadlineExceeded), false},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pause := retryPauseFromError(tc.err)
			if (pause != nil) != tc.want {
				t.Fatalf("pause=%+v err=%v", pause, tc.err)
			}
			if pause != nil && (pause.Attempts != 4 || pause.RetryCount != 3 || pause.Reason != exhausted.Err.Error() || !pause.NextRetryAt.Equal(exhausted.NextRetryAt)) {
				t.Fatalf("lost retry metadata: %+v", pause)
			}
		})
	}
	if pause := retryPauseFromError(&timeout); pause == nil || pause.Reason != timeout.Err.Error() {
		t.Fatalf("live-root request timeout lost pause: %+v", pause)
	}
}

func TestRetryPauseMemberReopensAndContinuesWithoutReplay(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	exhausted := exhaustedRetry(time.Now().Add(time.Hour).UTC())
	model := retryPauseModel(func(_ context.Context, req *llm.CompletionRequest) (messages.ChatMessage, error) {
		switch calls.Add(1) {
		case 1:
			return iterationTool("published", "swarm_publish", `{"text":"accepted evidence"}`), nil
		case 2:
			return messages.ChatMessage{}, exhausted
		default:
			var assignment, receipt, interrupted int
			for _, m := range req.Messages {
				if strings.HasPrefix(m.Content, "inspect once\n\nCompletion: ") {
					assignment++
				}
				if m.ToolCallID == "published" {
					receipt++
				}
				if m.ToolCallID == "uncertain" && m.Content == llm.ToolInterruptedContent {
					interrupted++
				}
			}
			if assignment != 1 || receipt != 1 || interrupted != 1 {
				t.Errorf("replayed/lost history: assignment=%d receipt=%d interrupted=%d", assignment, receipt, interrupted)
			}
			return answer("finished from committed history"), nil
		}
	})
	r := runtimeTest(t, model, 1, 1)
	path := promoteRetryTest(t, r)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := r.Agent(ctx, "", AgentRequest{Label: "Retry worker", Task: "inspect once", ReadOnly: true})
	var retryErr *llm.RetryExhaustedError
	if !errors.As(err, &retryErr) {
		t.Fatalf("not exhausted: %v", err)
	}
	s := waitIterationMember(t, ctx, r, result.Session)
	e := s.Executions[result.Execution]
	if e == nil || e.Status != "paused" || e.RetryPause == nil || e.Iterations != 2 || s.Tasks[result.Task].Status != "blocked" {
		t.Fatalf("retry failure was not paused: %+v", e)
	}
	generation := e.Generation
	if p := MemberState(s, s.Members[result.Session]); p.Display != "paused · retry budget exhausted" || p.RetryPause.Attempts != 4 || !p.Attention {
		t.Fatalf("pause presentation: %+v", p)
	}
	if err := r.Resume(ctx, result.Session, 0); err == nil || !strings.Contains(err.Error(), "explicit continuation is available at") {
		t.Fatalf("early resume: %v", err)
	}
	r = reopenRetryTest(t, r, path)
	s, err = r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e = s.Executions[result.Execution]
	if e.RetryPause == nil || e.RetryPause.Reason != exhausted.Err.Error() || !e.RetryPause.NextRetryAt.Equal(exhausted.NextRetryAt) || e.Generation != generation {
		t.Fatalf("reopen lost metadata or resumed: %+v", e)
	}
	if _, err := r.FollowupTask(ctx, result.Session, "continue too soon", "early"); err == nil {
		t.Fatal("early follow-up resumed the member")
	}
	if _, err := r.Send(ctx, r.ID, result.Session, "info", "", "ordinary information"); err != nil {
		t.Fatal(err)
	}
	r.wakeIdleMember(result.Session)
	if calls.Load() != 2 {
		t.Fatal("reopen or mail automatically retried")
	}
	// Make eligibility pass without a timer, and model a crash with an uncertain
	// tool intent. Neither clock eligibility nor that intent authorizes replay.
	if err := r.update(ctx, func(s *State) error {
		e := s.Executions[result.Execution]
		e.RetryPause.NextRetryAt = time.Now().Add(-time.Second).UTC()
		e.Intent = []messages.ChatMessage{iterationTool("uncertain", "swarm_publish", `{"text":"must not execute"}`)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r.wakeIdleMember(result.Session)
	if calls.Load() != 2 {
		t.Fatal("eligibility automatically retried")
	}
	if _, err := r.FollowupTask(ctx, result.Session, "continue from saved evidence", "resume"); err != nil {
		t.Fatal(err)
	}
	s = waitIterationMember(t, ctx, r, result.Session)
	e = s.Executions[result.Execution]
	if e.Status != "completed" || e.RetryPause != nil || e.Iterations != 3 || e.Request.MaxIterations != 5 || e.Generation != generation+1 || len(s.Executions) != 1 || s.Runs[e.Run].Starts != 1 {
		t.Fatalf("continuation reset execution/allowance: %+v", e)
	}
	m := s.Members[result.Session]
	if m.Task != result.Task || m.Context != result.Context || len(s.Publications) != 1 || calls.Load() != 3 {
		t.Fatalf("continuation lost reservation or replayed effects: member=%+v publications=%d calls=%d", m, len(s.Publications), calls.Load())
	}
}

func TestRetryPausePermanentMemberFailureStaysFailed(t *testing.T) {
	t.Parallel()
	r := runtimeTest(t, retryPauseModel(func(context.Context, *llm.CompletionRequest) (messages.ChatMessage, error) {
		return messages.ChatMessage{}, errors.New("invalid provider credentials")
	}), 1, 1)
	result, err := r.Agent(context.Background(), "", AgentRequest{Label: "Worker", Task: "inspect", ReadOnly: true})
	if err == nil {
		t.Fatal("permanent failure succeeded")
	}
	s := waitIterationMember(t, context.Background(), r, result.Session)
	if e := s.Executions[result.Execution]; e.Status != "failed" || e.RetryPause != nil {
		t.Fatalf("permanent failure became recoverable: %+v", e)
	}
}

func TestParentRetryPausePromotesReopensAndClearsOnlyOnCommit(t *testing.T) {
	t.Parallel()
	r := runtimeTest(t, idleModel(), 1, 1)
	path := promoteRetryTest(t, r)
	ctx := context.Background()
	exhausted := exhaustedRetry(time.Now().Add(-time.Second).UTC())
	cb := &llm.AgentCallbacks{}
	r.bindParent(cb, nil)
	prefix := []messages.ChatMessage{answer("accepted prefix")}
	if err := cb.Checkpoint(ctx, llm.AgentCheckpoint{Generated: prefix, Final: true, Err: exhausted}); err != nil {
		t.Fatal(err)
	}
	if mode, _ := r.config.Store.(sessions.DurableStore).Location(); mode != sessions.ModeDisk {
		t.Fatal("root-only retry pause was not promoted")
	}
	if err := r.update(ctx, func(s *State) error {
		s.ParentTurns[r.ID].Intent = []messages.ChatMessage{iterationTool("uncertain", "swarm_publish", `{"text":"must not execute"}`)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r = reopenRetryTest(t, r, path)
	s, err := r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	turn := s.ParentTurns[r.ID]
	if turn == nil || turn.RetryPause == nil || len(turn.Intent) != 0 || turn.RetryPause.Attempts != 4 {
		t.Fatalf("parent recovery lost pause: %+v", turn)
	}
	for _, state := range []*State{nil, s} {
		if p := r.ParentState(state); p.Display != "paused · retry budget exhausted" || p.RetryPause.RetryCount != 3 || p.Busy {
			t.Fatalf("recovered parent presentation: %+v", p)
		}
	}
	var calls atomic.Int32
	model := modelFunc(func(context.Context, *llm.CompletionRequest) messages.ChatMessage {
		calls.Add(1)
		return answer("continued")
	})
	agent := parentAgent(t, r, model, 3)
	if _, err := r.RunParent(ctx, agent, &llm.CompletionRequest{}, nil, func() bool { return false }); err == nil {
		t.Fatal("fenced turn ran")
	}
	if p := r.ParentState(nil); p.RetryPause.Attempts != 4 || calls.Load() != 0 {
		t.Fatalf("uncommitted turn cleared pause: %+v calls=%d", p, calls.Load())
	}
	history, err := r.config.Parent.GetHistory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 3 || history[0].Content != "accepted prefix" || history[2].ToolCallID != "uncertain" || history[2].Content != llm.ToolInterruptedContent {
		t.Fatalf("uncertain intent was lost or replayed: %+v", history)
	}
	history = append(history, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "continue"})
	if _, err := r.RunParent(ctx, agent, &llm.CompletionRequest{Messages: history}, nil, nil); err != nil {
		t.Fatal(err)
	}
	s, err = r.State(ctx)
	if err != nil || s.ParentTurns[r.ID] != nil || len(s.Publications) != 0 || calls.Load() != 1 || r.ParentState(nil).Display != "idle" {
		t.Fatalf("committed continuation failed to clear pause or replayed tools: state=%+v err=%v calls=%d", s, err, calls.Load())
	}
}

func TestParentRetryPauseEarlyContinuationRejected(t *testing.T) {
	t.Parallel()
	r := runtimeTest(t, idleModel(), 1, 1)
	promoteRetryTest(t, r)
	var calls atomic.Int32
	agent := parentAgent(t, r, retryPauseModel(func(context.Context, *llm.CompletionRequest) (messages.ChatMessage, error) {
		if calls.Add(1) != 1 {
			t.Error("early continuation reached provider")
		}
		return messages.ChatMessage{}, exhaustedRetry(time.Now().Add(time.Hour))
	}), 3)
	if _, err := r.RunParent(context.Background(), agent, &llm.CompletionRequest{}, nil, nil); retryPauseFromError(err) == nil || !errors.Is(err, subagent.ErrRetryPaused) {
		t.Fatalf("parent did not confirm durable retry pause: %v", err)
	}
	if _, err := r.RunParent(context.Background(), agent, &llm.CompletionRequest{}, nil, nil); err == nil || !strings.Contains(err.Error(), "no automatic retry") {
		t.Fatalf("early parent continuation: %v", err)
	}
	if p := r.ParentState(nil); p.RetryPause.Attempts != 4 {
		t.Fatalf("early rejection cleared pause: %+v", p)
	}
}

func TestParentRetryPausePromotionFailureDoesNotClaimDurability(t *testing.T) {
	t.Parallel()
	for _, hook := range []bool{false, true} {
		t.Run(fmt.Sprint(hook), func(t *testing.T) {
			r := runtimeTest(t, idleModel(), 1, 1)
			if hook {
				r.config.Promote = func(context.Context) error { return errors.New("disk unavailable") }
			}
			agent := parentAgent(t, r, retryPauseModel(func(context.Context, *llm.CompletionRequest) (messages.ChatMessage, error) {
				return messages.ChatMessage{}, exhaustedRetry(time.Now())
			}), 3)
			_, err := r.RunParent(context.Background(), agent, &llm.CompletionRequest{}, nil, nil)
			if err == nil || !strings.Contains(err.Error(), "recoverable retry pause") || errors.Is(err, subagent.ErrRetryPaused) {
				t.Fatalf("promotion failure hidden: %v", err)
			}
			s, readErr := r.State(context.Background())
			if readErr != nil || s.ParentTurns[r.ID] != nil || r.ParentState(nil).Display != "paused · error" {
				t.Fatalf("failed promotion claimed pause: state=%+v display=%+v err=%v", s, r.ParentState(nil), readErr)
			}
		})
	}
}

func TestParentRetryPauseCheckpointRollbackAndFence(t *testing.T) {
	t.Parallel()
	for _, fenced := range []bool{false, true} {
		t.Run(fmt.Sprint(fenced), func(t *testing.T) {
			r := runtimeTest(t, idleModel(), 1, 1)
			path := promoteRetryTest(t, r)
			if !fenced {
				setParent(r, &refreshStoreFault{CoordinationSession: r.parent, match: func(_, after *State) bool {
					turn := after.ParentTurns[r.ID]
					return turn != nil && turn.RetryPause != nil
				}, before: func(*State) error { return errors.New("checkpoint rollback") }})
			}
			cb := &llm.AgentCallbacks{}
			r.bindParent(cb, func() bool { return !fenced })
			err := cb.Checkpoint(context.Background(), llm.AgentCheckpoint{Final: true, Generated: []messages.ChatMessage{answer("not committed")}, Err: exhaustedRetry(time.Now())})
			if err == nil {
				t.Fatal("failed checkpoint succeeded")
			}
			s, readErr := r.State(context.Background())
			history, historyErr := r.config.Parent.GetHistory(context.Background())
			if readErr != nil || historyErr != nil || s.ParentTurns[r.ID] != nil || len(history) != 0 || r.ParentState(nil).RetryPause.Attempts != 0 {
				t.Fatalf("failed checkpoint stored prefix/pause: state=%+v history=%+v err=%v %v", s, history, readErr, historyErr)
			}
			if !fenced {
				r = reopenRetryTest(t, r, path)
				if p := r.ParentState(nil); p.RetryPause.Attempts != 0 {
					t.Fatalf("reopen fabricated pause: %+v", p)
				}
			}
		})
	}
}
