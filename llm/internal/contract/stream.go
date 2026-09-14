package contract

import (
	"context"
	"time"

	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
)

// RunStream handles the common goroutine scaffolding for ChatCompletionStream.
// Each provider creates its adapter, then passes a function that does the
// provider-specific work with the StreamingCore, using the context RunStream
// hands it for every upstream call.
//
// Two watchdog budgets apply uniformly to every provider; zero disables each.
// stallTimeout cancels the stream (streaming.StallError cause) when no
// provider data arrives for that long — it bounds silence, not total
// generation time, and every chunk resets it. deadline cancels the stream
// (streaming.DeadlineError cause) once the whole call has run that long,
// whether or not data is still trickling in.
func RunStream(ctx context.Context, stallTimeout, deadline time.Duration, processor EventStreamProcessor, adapter streaming.ProviderAdapter, fn func(context.Context, *streaming.StreamingCore)) <-chan *messages.StreamEvent {
	ch := make(chan messages.ChatMessage, 10)
	streamCtx := ctx
	var watchdog *streamWatchdog
	if stallTimeout > 0 || deadline > 0 {
		watchdog, streamCtx = newStreamWatchdog(ctx, stallTimeout, deadline)
	}
	core := streaming.NewStreamingCore(streamCtx, ch, adapter)
	if watchdog != nil {
		core.SetActivityNotifier(watchdog.touch)
	}
	go func() {
		defer close(ch)
		fn(streamCtx, core)
		if watchdog != nil {
			watchdog.finish(core)
		}
	}()
	return processor.ProcessMessagesToEvents(ch)
}

// streamWatchdog bounds a stream two ways: a stall timer the provider
// goroutine resets on every piece of data, and a fixed deadline timer nothing
// resets. Whichever fires first cancels the stream context with its typed
// cause, which EmitError surfaces in place of the resulting read-cancellation
// errors.
type streamWatchdog struct {
	stallTimeout  time.Duration
	stallTimer    *time.Timer
	deadlineTimer *time.Timer
	ctx           context.Context
	cancel        context.CancelCauseFunc
}

func newStreamWatchdog(parent context.Context, stallTimeout, deadline time.Duration) (*streamWatchdog, context.Context) {
	ctx, cancel := context.WithCancelCause(parent)
	w := &streamWatchdog{stallTimeout: stallTimeout, ctx: ctx, cancel: cancel}
	if stallTimeout > 0 {
		w.stallTimer = time.AfterFunc(stallTimeout, func() {
			cancel(&streaming.StallError{Timeout: stallTimeout})
		})
	}
	if deadline > 0 {
		w.deadlineTimer = time.AfterFunc(deadline, func() {
			cancel(&streaming.DeadlineError{Deadline: deadline})
		})
	}
	return w, ctx
}

// touch pushes the stall deadline out; invoked on every piece of provider
// data. The hard deadline deliberately never moves.
func (w *streamWatchdog) touch() {
	if w.stallTimer != nil {
		w.stallTimer.Reset(w.stallTimeout)
	}
}

// finish stops the watchdog after the provider function returned. When a
// watchdog cause canceled the stream, it guarantees that error reached the
// channel — a provider that unwound without emitting one would otherwise let
// the processor fabricate a successful completion from the partial state —
// then releases the derived context.
func (w *streamWatchdog) finish(core *streaming.StreamingCore) {
	if w.stallTimer != nil {
		w.stallTimer.Stop()
	}
	if w.deadlineTimer != nil {
		w.deadlineTimer.Stop()
	}
	if cause := streaming.WatchdogCause(w.ctx); cause != nil {
		core.EmitError(cause)
	}
	w.cancel(nil)
}
