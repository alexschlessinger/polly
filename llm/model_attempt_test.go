package llm

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestModelAttemptObservesFailedAndSuccessfulRequests(t *testing.T) {
	var attempts []ModelAttemptEvent
	var retries []RetryEvent
	calls := 0
	model := retryStreamFunc(func(context.Context, *CompletionRequest) <-chan *messages.StreamEvent {
		calls++
		if calls == 1 {
			return retryEvents(&messages.StreamEvent{Type: messages.EventTypeError, Error: io.ErrUnexpectedEOF})
		}
		return retryEvents(retryAnswer())
	})
	agent := NewAgent(model, nil, AgentConfig{})
	defer agent.Close()
	response, err := agent.Run(context.Background(), &CompletionRequest{Model: "test/model", RetryPolicy: fastRetryPolicy(1)}, &AgentCallbacks{
		OnModelAttempt: func(event ModelAttemptEvent) { attempts = append(attempts, event) },
		OnRetry:        func(event RetryEvent) { retries = append(retries, event) },
	})
	if err != nil || len(attempts) != 4 || len(retries) != 2 {
		t.Fatalf("response=%+v err=%v attempts=%+v retries=%+v", response, err, attempts, retries)
	}
	for i := 0; i < 4; i += 2 {
		start, end := attempts[i], attempts[i+1]
		if start.ID == "" || start.ID != end.ID || start.Finished || !end.Finished || start.At.After(end.At) || start.Model != "test/model" || start.Purpose != "conversation" || start.Attempt != i/2 {
			t.Fatalf("attempt lifecycle: %+v %+v", start, end)
		}
	}
	if attempts[0].ID == attempts[2].ID || !errors.Is(attempts[1].Err, io.ErrUnexpectedEOF) || attempts[3].Err != nil || attempts[3].ID != response.Message.DisplayID() || attempts[3].StopReason != messages.StopReasonEndTurn {
		t.Fatalf("attempt identities/errors = %+v, response = %+v", attempts, response.Message)
	}
	if retries[0].Source != "stream" || !retries[0].Waiting || retries[1].Waiting || retries[1].WaitError != nil {
		t.Fatalf("retry transitions = %+v", retries)
	}
}

func TestModelAttemptObserverDoesNotPermitReplay(t *testing.T) {
	calls := 0
	agent := NewAgent(retryStreamFunc(func(context.Context, *CompletionRequest) <-chan *messages.StreamEvent {
		calls++
		return retryEvents(&messages.StreamEvent{Type: messages.EventTypeContent, Content: "visible"}, &messages.StreamEvent{Type: messages.EventTypeError, Error: io.ErrUnexpectedEOF})
	}), nil, AgentConfig{})
	defer agent.Close()
	_, err := agent.Run(context.Background(), &CompletionRequest{RetryPolicy: fastRetryPolicy(1)}, &AgentCallbacks{OnContent: func(string) {}, OnModelAttempt: func(ModelAttemptEvent) {}})
	if !errors.Is(err, io.ErrUnexpectedEOF) || calls != 1 {
		t.Fatalf("observer authorized output replay: calls=%d err=%v", calls, err)
	}
}

func TestModelAttemptCompactionRetainsEnclosingIteration(t *testing.T) {
	var events []ModelAttemptEvent
	ctx := context.WithValue(context.Background(), modelAttemptObserverKey{}, func(event ModelAttemptEvent) { events = append(events, event) })
	ctx = context.WithValue(ctx, modelAttemptIterationKey{}, 7)
	agent := NewAgent(retryStreamFunc(func(context.Context, *CompletionRequest) <-chan *messages.StreamEvent {
		return retryEvents(retryAnswer())
	}), nil, AgentConfig{})
	defer agent.Close()
	_, err := agent.complete(ctx, &CompletionRequest{Model: "test/summary"}, nil)
	if err != nil || len(events) != 2 || events[0].Purpose != "compaction" || events[0].Iteration != 7 || events[0].Model != "test/summary" || !events[1].Finished {
		t.Fatalf("compaction observations = %+v, err=%v", events, err)
	}
}

func TestModelAttemptCancellationKeepsTypedCause(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cause := errors.New("host stopped request")
	var events []ModelAttemptEvent
	agent := NewAgent(retryStreamFunc(func(context.Context, *CompletionRequest) <-chan *messages.StreamEvent {
		cancel(cause)
		return make(chan *messages.StreamEvent)
	}), nil, AgentConfig{})
	defer agent.Close()
	_, err := agent.Run(ctx, &CompletionRequest{}, &AgentCallbacks{OnModelAttempt: func(event ModelAttemptEvent) { events = append(events, event) }})
	if !errors.Is(err, context.Canceled) || len(events) != 2 || events[1].Err != cause || !events[1].Canceled {
		t.Fatalf("cancellation: err=%v events=%+v", err, events)
	}
}

func TestModelAttemptObserverDoesNotReachNestedAgent(t *testing.T) {
	child := NewAgent(retryStreamFunc(func(context.Context, *CompletionRequest) <-chan *messages.StreamEvent {
		return retryEvents(retryAnswer())
	}), nil, AgentConfig{})
	defer child.Close()
	registry := tools.NewToolRegistry([]tools.Tool{&tools.Func{Name: "nested", Run: func(ctx context.Context, _ tools.Args) (string, error) {
		if observer, _ := ctx.Value(modelAttemptObserverKey{}).(func(ModelAttemptEvent)); observer != nil {
			t.Fatal("parent attempt observer leaked into tool context")
		}
		_, err := child.Run(context.WithoutCancel(ctx), &CompletionRequest{Model: "child/model"}, nil)
		return "child done", err
	}}})
	calls := 0
	parent := NewAgent(retryStreamFunc(func(context.Context, *CompletionRequest) <-chan *messages.StreamEvent {
		calls++
		if calls == 1 {
			return retryEvents(&messages.StreamEvent{Type: messages.EventTypeComplete, Message: &messages.ChatMessage{
				Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse,
				ToolCalls: []messages.ChatMessageToolCall{{ID: "nested-1", Name: "nested", Arguments: `{}`}},
			}})
		}
		return retryEvents(retryAnswer())
	}), registry, AgentConfig{})
	defer parent.Close()
	var events []ModelAttemptEvent
	_, err := parent.Run(context.Background(), &CompletionRequest{Model: "parent/model"}, &AgentCallbacks{OnModelAttempt: func(event ModelAttemptEvent) { events = append(events, event) }})
	if err != nil || len(events) != 4 {
		t.Fatalf("nested agent err=%v parent events=%+v", err, events)
	}
	for _, event := range events {
		if event.Model != "parent/model" {
			t.Fatalf("child attempt attributed to parent: %+v", event)
		}
	}
}
