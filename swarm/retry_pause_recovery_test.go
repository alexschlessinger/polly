package swarm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/subagent"
)

type retryCheckpointStore struct {
	sessions.SessionStore
	wrap func(coordinatedSession) coordinatedSession
}

func (s *retryCheckpointStore) Acquire(ctx context.Context, name string, opts sessions.AcquireOptions) (sessions.Session, error) {
	handle, err := s.SessionStore.Acquire(ctx, name, opts)
	if err != nil || !opts.ExistingOnly {
		return handle, err
	}
	return s.wrap(handle.(coordinatedSession)), nil
}

type retryCheckpointSession struct {
	coordinatedSession
	before func(*State) error
	after  func()
}

func (s *retryCheckpointSession) UpdateCoordination(ctx context.Context, fn func(*sessions.CoordinationState) error) error {
	paused := false
	err := s.coordinatedSession.UpdateCoordination(ctx, func(raw *sessions.CoordinationState) error {
		if err := fn(raw); err != nil {
			return err
		}
		state, err := decodeState(raw)
		if err != nil {
			return err
		}
		for _, e := range state.Executions {
			if e.Member == raw.ActorID && e.RetryPause != nil {
				paused = true
				if s.before != nil {
					return s.before(state)
				}
			}
		}
		return nil
	})
	if err == nil && paused && s.after != nil {
		s.after()
	}
	return err
}

func TestRetryPauseMemberCheckpointFailureDoesNotFabricatePause(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"rollback", "fenced"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			var calls atomic.Int32
			r := runtimeTest(t, retryPauseModel(func(context.Context, *llm.CompletionRequest) (messages.ChatMessage, error) {
				calls.Add(1)
				return messages.ChatMessage{}, exhaustedRetry(time.Now())
			}), 1, 1)
			if mode == "rollback" {
				r.config.Store = &retryCheckpointStore{SessionStore: r.config.Store, wrap: func(s coordinatedSession) coordinatedSession {
					return &retryCheckpointSession{coordinatedSession: s, before: func(*State) error { return errors.New("final checkpoint rolled back") }}
				}}
			} else {
				r.config.Callbacks = func(_ context.Context, m Member) *llm.AgentCallbacks {
					return &llm.AgentCallbacks{OnError: func(error) {
						if err := r.update(ctx, func(s *State) error {
							e := s.Executions[s.Members[m.ID].Execution]
							e.Generation++
							e.Status, e.Error = "paused", "new generation owns this execution"
							return nil
						}); err != nil {
							t.Error(err)
						}
					}}
				}
			}
			result, err := r.Agent(ctx, "", AgentRequest{Label: "Worker", Task: "inspect", ReadOnly: true})
			var exhausted *llm.RetryExhaustedError
			if !errors.As(err, &exhausted) || errors.Is(err, subagent.ErrRetryPaused) {
				t.Fatalf("failed checkpoint reported confirmed recovery: %v", err)
			}
			s := waitIterationMember(t, ctx, r, result.Session)
			m := s.Members[result.Session]
			e := s.Executions[m.Execution]
			if e.RetryPause != nil || calls.Load() != 1 || mode == "rollback" && e.Status != "failed" {
				t.Fatalf("finish fabricated pause: %+v calls=%d", e, calls.Load())
			}
			if mode == "fenced" && (e.Generation != 2 || e.Error != "new generation owns this execution") {
				t.Fatalf("finish crossed generation fence: %+v", e)
			}
			child, acquireErr := r.acquireMemberSession(ctx, m)
			if acquireErr != nil {
				t.Fatal(acquireErr)
			}
			metadata, metadataErr := child.GetMetadata(ctx)
			child.Close()
			if metadataErr != nil || metadata.SpawnOutcome != sessions.ReportFailed {
				t.Fatalf("unsafe pause metadata: %+v %v", metadata, metadataErr)
			}
			for _, mail := range s.Messages {
				if strings.Contains(mail.Text, "no automatic retry") {
					t.Fatalf("failed checkpoint published recovery notice: %+v", mail)
				}
			}
		})
	}
}

func TestRetryPauseCheckpointSurvivesMissingFinish(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	var calls atomic.Int32
	r := runtimeTest(t, retryPauseModel(func(_ context.Context, req *llm.CompletionRequest) (messages.ChatMessage, error) {
		switch calls.Add(1) {
		case 1:
			return iterationTool("published", "swarm_publish", `{"text":"accepted evidence"}`), nil
		case 2:
			return messages.ChatMessage{}, exhaustedRetry(time.Now().Add(-time.Second))
		default:
			count := 0
			for _, m := range req.Messages {
				if m.ToolCallID == "published" {
					count++
				}
			}
			if count != 1 {
				t.Errorf("accepted tool receipt count=%d", count)
			}
			return answer("continued from checkpoint"), nil
		}
	}), 1, 1)
	path := promoteRetryTest(t, r)
	var failed atomic.Bool
	setParent(r, &refreshStoreFault{CoordinationSession: r.parent, match: func(before, _ *State) bool {
		for _, e := range before.Executions {
			if e.RetryPause != nil {
				return true
			}
		}
		return false
	}, before: func(*State) error {
		failed.Store(true)
		return errors.New("crashed before finish commit")
	}})
	request := AgentRequest{Label: "Worker", Task: "inspect", ReadOnly: true, CallID: "saved-call"}
	result, err := r.Agent(ctx, "", request)
	if err == nil || !failed.Load() || errors.Is(err, subagent.ErrRetryPaused) {
		t.Fatalf("finish fault not reported: %v", err)
	}
	s := waitIterationMember(t, ctx, r, result.Session)
	m := s.Members[result.Session]
	e := s.Executions[m.Execution]
	task := s.Tasks[m.Task]
	notice := resultNotice(s, task)
	if e.RetryPause == nil || e.Result == nil || e.Result.Execution != e.ID || e.Result.Task != task.ID || e.Result.Revision != task.Revision || e.Result.Value != "partial finding" || task.Result != "partial finding" || task.Status != "blocked" || notice == nil {
		t.Fatalf("checkpoint lacked atomic pause receipt: execution=%+v task=%+v notice=%+v", e, task, notice)
	}
	executionID, contextID, taskID, revision, noticeID := e.ID, m.Context, m.Task, task.Revision, notice.ID
	r = reopenRetryTest(t, r, path)
	s, err = r.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.Tasks[taskID].Revision != revision || resultNotice(s, s.Tasks[taskID]).ID != noticeID || calls.Load() != 2 {
		t.Fatalf("reopen rewrote receipt or resumed: %+v", s.Tasks[taskID])
	}
	receipt, receiptErr := r.Agent(ctx, "", request)
	if !errors.Is(receiptErr, subagent.ErrRetryPaused) || receipt.Execution != executionID || receipt.Session != result.Session || receipt.Value != "partial finding" || calls.Load() != 2 {
		t.Fatalf("idempotent call lost saved pause or restarted: %+v %v", receipt, receiptErr)
	}
	if _, err := r.FollowupTask(ctx, result.Session, "continue explicitly", "resume"); err != nil {
		t.Fatal(err)
	}
	s = waitIterationMember(t, ctx, r, result.Session)
	if len(s.Executions) != 1 || s.Executions[executionID].Status != "completed" || s.Executions[executionID].Iterations != 3 || s.Members[result.Session].Context != contextID || len(s.Publications) != 1 || calls.Load() != 3 {
		t.Fatalf("recovery replayed or lost identity: %+v", s.Executions[executionID])
	}
}

func TestRetryPauseStopAfterCheckpointClearsRecoveryAuthority(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	checkpoint, release, stopped := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	r := runtimeTest(t, retryPauseModel(func(context.Context, *llm.CompletionRequest) (messages.ChatMessage, error) {
		calls.Add(1)
		return messages.ChatMessage{}, exhaustedRetry(time.Now().Add(time.Hour))
	}), 1, 1)
	r.config.Store = &retryCheckpointStore{SessionStore: r.config.Store, wrap: func(s coordinatedSession) coordinatedSession {
		return &retryCheckpointSession{coordinatedSession: s, after: func() {
			close(checkpoint)
			select {
			case <-release:
			case <-ctx.Done():
			}
		}}
	}}
	setParent(r, &refreshStoreFault{CoordinationSession: r.parent, match: func(_, after *State) bool {
		for _, m := range after.Members {
			if m.Control == MemberControlStopped {
				return true
			}
		}
		return false
	}, after: func() error { close(stopped); return nil }})
	i, err := r.start(ctx, "", AgentRequest{Label: "Worker", Task: "inspect", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-checkpoint:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	stopResult := make(chan error, 1)
	go func() { stopResult <- r.StopMember(ctx, i.member) }()
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	close(release)
	if err := <-stopResult; err != nil {
		t.Fatal(err)
	}
	s := waitIterationMember(t, ctx, r, i.member)
	if e := s.Executions[i.id]; e.RetryPause != nil || s.Members[i.member].Control != MemberControlStopped || errors.Is(i.err, subagent.ErrRetryPaused) {
		t.Fatalf("stop retained retry authority: %+v %v", e, i.err)
	}
	if _, err := r.FollowupTask(ctx, i.member, "continue", "unauthorized"); err == nil || !strings.Contains(err.Error(), "stopped by the user") {
		t.Fatalf("parent resumed user-stopped work: %v", err)
	}
	r.wakeIdleMember(i.member)
	if calls.Load() != 1 {
		t.Fatal("stop race replayed model request")
	}
}

func TestRetryPauseWorkflowRetainsSessionAndEvidence(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	r := runtimeTest(t, retryPauseModel(func(context.Context, *llm.CompletionRequest) (messages.ChatMessage, error) {
		if calls.Add(1) == 1 {
			return iterationTool("published", "swarm_publish", `{"text":"accepted evidence"}`), nil
		}
		return messages.ChatMessage{}, exhaustedRetry(time.Now().Add(time.Hour))
	}), 1, 1)
	report, err := r.RunWorkflow(context.Background(), `polly.defineWorkflow({name:"inspect",inputSchema:polly.schema.object({}),async run(){return await polly.agent({label:"Worker",task:"inspect",readOnly:true})}})`, map[string]any{})
	if err == nil || report == nil || report.Error == nil || report.Error.Code != "retry_exhausted" || report.Error.Session == "" || len(report.Steps) != 1 || report.Steps[0].Error.Code != "retry_exhausted" {
		t.Fatalf("workflow flattened retry pause: %+v %v", report, err)
	}
	if !strings.Contains(fmt.Sprint(report.Error.Result), "partial finding") {
		t.Fatalf("workflow lost partial evidence: %+v", report.Error)
	}
	s := waitIterationMember(t, context.Background(), r, report.Error.Session)
	if e := s.Executions[s.Members[report.Error.Session].Execution]; e.RetryPause == nil || e.Status != "paused" || len(s.Executions) != 1 || calls.Load() != 2 {
		t.Fatalf("workflow replayed/flattened member pause: %+v", e)
	}
}
