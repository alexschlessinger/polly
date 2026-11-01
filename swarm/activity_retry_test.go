package swarm

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestActivityRetryOptInRespectsExternalDisplays(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		cb   llm.AgentCallbacks
		want bool
	}{
		{"no host", llm.AgentCallbacks{}, true},
		{"content", llm.AgentCallbacks{OnContent: func(string) {}}, false},
		{"reasoning", llm.AgentCallbacks{OnReasoning: func(string) {}}, false},
		{"commentary", llm.AgentCallbacks{OnCommentary: func(messages.AssistantText, bool) {}}, false},
		{"status only", llm.AgentCallbacks{OnRetry: func(llm.RetryEvent) {}}, true},
		{"display with abort", llm.AgentCallbacks{OnContent: func(string) {}, OnAttemptAborted: func(int, int, error) {}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := &invocation{}
			i.bindActivity(&tc.cb, 0, 0)
			if (tc.cb.OnAttemptAborted != nil) != tc.want {
				t.Fatalf("abort opt-in = %v, want %v", tc.cb.OnAttemptAborted != nil, tc.want)
			}
		})
	}
}

func TestActivityRetryWaitAndAbortComposeHostCallbacks(t *testing.T) {
	t.Parallel()
	i := &invocation{}
	aborted, notified := 0, 0
	failure := errors.New("rate limited")
	cb := &llm.AgentCallbacks{
		OnAttemptAborted: func(iteration, attempt int, err error) {
			if iteration != 2 || attempt != 0 || err != failure {
				t.Errorf("wrong abort callback: %d/%d %v", iteration, attempt, err)
			}
			aborted++
		},
		OnRetry: func(llm.RetryEvent) { notified++ },
	}
	i.bindActivity(cb, time.Minute, time.Hour)
	cb.OnModelRequest(2, 0)
	next := time.Now().Add(time.Second)
	event := llm.RetryEvent{Err: failure, RetryCount: 3, NextRetryAt: next, Delay: time.Second, Waiting: true}
	cb.OnRetry(event) // HTTP wait: not a new stream attempt.
	a := i.activity.snapshot
	if a.Phase != "retrying" || a.RetryCount != 3 || !a.NextRetryAt.Equal(next) || a.Attempt != 1 || a.RequestActive || a.RetryReason != failure.Error() {
		t.Fatalf("missing HTTP wait: %+v", a)
	}
	event.Waiting = false
	cb.OnRetry(event)
	a = i.activity.snapshot
	if a.RetryCount != 0 || !a.NextRetryAt.IsZero() || a.RetryReason != "" || !a.RequestActive || a.Attempt != 1 {
		t.Fatalf("HTTP wait did not clear: %+v", a)
	}
	cb.OnContent("failed partial")
	cb.OnUsageProgress(llm.UsageUpdate{OutputTokens: 4})
	cb.OnAttemptAborted(2, 0, failure)
	a = i.activity.snapshot
	if aborted != 1 || notified != 2 || a.RequestActive || a.StreamedBytes != 0 || a.OutputTokens != 0 {
		t.Fatalf("abort did not reset/compose: %+v, aborted=%d notified=%d", a, aborted, notified)
	}
	event.Waiting = true
	cb.OnRetry(event)
	event.Waiting = false // cancellation also clears the countdown.
	cb.OnRetry(event)
	if i.activity.snapshot.RequestActive || i.activity.snapshot.RetryCount != 0 {
		t.Fatalf("stream wait resurrected failed request: %+v", i.activity.snapshot)
	}
}

func TestActivityRetryConcurrentAndStaleWaitNotifications(t *testing.T) {
	t.Parallel()
	i := &invocation{}
	cb := &llm.AgentCallbacks{}
	i.bindActivity(cb, 0, 0)
	cb.OnModelRequest(0, 0)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 100 {
			cb.OnRetry(llm.RetryEvent{Waiting: true, RetryCount: 1, NextRetryAt: time.Now()})
			cb.OnRetry(llm.RetryEvent{})
		}
	}()
	go func() {
		defer wg.Done()
		for range 100 {
			cb.OnStreamActivity(0, 0)
		}
	}()
	wg.Wait()
	next := &llm.AgentCallbacks{}
	i.bindActivity(next, 0, 0)
	next.OnModelRequest(0, 0)
	cb.OnRetry(llm.RetryEvent{Waiting: true, RetryCount: 99, NextRetryAt: time.Now()})
	cb.OnAttemptAborted(0, 0, errors.New("old generation"))
	if a := i.activity.snapshot; a.Phase != "requesting" || a.RetryCount != 0 || !a.RequestActive {
		t.Fatalf("old invocation changed current activity: %+v", a)
	}
}
