package llm

import (
	"context"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
)

// ModelAttemptEvent is a local observation, not a provider request identifier.
// Finished distinguishes the terminal observation from the start. Err is the
// stream error, or the context's typed cause when cancellation ended the call.
// Iteration and Attempt are zero-based; compaction attempts have their own
// attempt counter within the enclosing conversation iteration.
type ModelAttemptEvent struct {
	ID         string
	Model      string
	Purpose    string // conversation or compaction
	Iteration  int
	Attempt    int
	At         time.Time
	Finished   bool
	Canceled   bool // the enclosing context, rather than only the stream, ended
	Err        error
	StopReason messages.StopReason
}

type modelAttemptObserverKey struct{}
type modelAttemptIterationKey struct{}
type modelAttemptPurposeKey struct{}

func observeModelAttempt(ctx context.Context, req *CompletionRequest, cb *AgentCallbacks, id string, iteration, attempt int) func(*messages.ChatMessage, error) {
	observer, _ := ctx.Value(modelAttemptObserverKey{}).(func(ModelAttemptEvent))
	if observer == nil && cb != nil {
		observer = cb.OnModelAttempt
	}
	if observer == nil {
		return func(*messages.ChatMessage, error) {}
	}
	purpose, _ := ctx.Value(modelAttemptPurposeKey{}).(string)
	if purpose == "" {
		purpose = "conversation"
	} else if enclosing, ok := ctx.Value(modelAttemptIterationKey{}).(int); ok {
		iteration = enclosing
	}
	event := ModelAttemptEvent{ID: id, Model: req.Model, Purpose: purpose, Iteration: iteration, Attempt: attempt, At: time.Now().UTC()}
	observer(event)
	return func(reply *messages.ChatMessage, err error) {
		event.At, event.Finished, event.Err = time.Now().UTC(), true, err
		if ctx.Err() != nil {
			event.Canceled = true
			event.Err = context.Cause(ctx)
		}
		if reply != nil {
			event.StopReason = reply.StopReason
		}
		observer(event)
	}
}
