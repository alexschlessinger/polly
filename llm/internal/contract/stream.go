package contract

import (
	"context"
	"sync"
	"time"

	"github.com/alexschlessinger/pollytool/llm/internal/retry"
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
		observer := retry.Observer(streamCtx)
		streamCtx = retry.WithObserver(streamCtx, func(event retry.RetryEvent) {
			// Backoff is deliberate silence, not a hung provider. In particular,
			// the stall watchdog must not cut a Retry-After short and let the
			// agent resend before the server's eligibility boundary.
			watchdog.backoff(event.Waiting)
			if observer != nil {
				observer(event)
			}
		})
	}
	core := streaming.NewStreamingCore(streamCtx, ch, adapter)
	core.SetDeliveryContext(ctx)
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
	return processor.ProcessMessagesToEvents(ctx, ch)
}

// streamWatchdog bounds a stream two ways: a stall timer the provider
// goroutine resets on every piece of data, and a fixed deadline timer nothing
// resets. Whichever fires first cancels the stream context with its typed
// cause, which EmitError surfaces in place of the resulting read-cancellation
// errors.
type streamWatchdog struct {
	mu                sync.Mutex
	waiting, finished bool
	stallTimeout      time.Duration
	stallAt           time.Time
	stallTimer        *time.Timer
	deadlineTimer     *time.Timer
	ctx               context.Context
	cancel            context.CancelCauseFunc
}

func newStreamWatchdog(parent context.Context, stallTimeout, deadline time.Duration) (*streamWatchdog, context.Context) {
	ctx, cancel := context.WithCancelCause(parent)
	w := &streamWatchdog{stallTimeout: stallTimeout, ctx: ctx, cancel: cancel}
	if stallTimeout > 0 {
		w.stallAt = time.Now().Add(stallTimeout)
		w.stallTimer = time.AfterFunc(stallTimeout, func() {
			w.mu.Lock()
			defer w.mu.Unlock()
			if !w.waiting && !w.finished {
				if left := time.Until(w.stallAt); left > 0 {
					// A touch or retry may have rearmed a timer whose old
					// callback was already waiting for this lock.
					w.stallTimer.Reset(left)
				} else {
					cancel(&streaming.StallError{Timeout: stallTimeout})
				}
			}
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
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stallTimer != nil && !w.waiting && !w.finished {
		w.stallAt = time.Now().Add(w.stallTimeout)
		w.stallTimer.Reset(w.stallTimeout)
	}
}

// backoff pauses only the silence budget. The hard request deadline and parent
// cancellation still apply while waiting, and a new send gets a fresh silence
// interval rather than inheriting time spent honoring a server hint.
func (w *streamWatchdog) backoff(waiting bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.waiting = waiting
	if w.stallTimer == nil || w.finished || w.ctx.Err() != nil {
		return
	}
	if waiting {
		w.stallTimer.Stop()
	} else {
		w.stallAt = time.Now().Add(w.stallTimeout)
		w.stallTimer.Reset(w.stallTimeout)
	}
}

// finish stops the watchdog after the provider function returned. When a
// watchdog cause canceled the stream, it guarantees that error reached the
// channel — a provider that unwound without emitting one would otherwise let
// the processor fabricate a successful completion from the partial state —
// then releases the derived context.
func (w *streamWatchdog) finish(core *streaming.StreamingCore) {
	w.mu.Lock()
	w.finished = true
	if w.stallTimer != nil {
		w.stallTimer.Stop()
	}
	if w.deadlineTimer != nil {
		w.deadlineTimer.Stop()
	}
	w.mu.Unlock()
	if cause := streaming.WatchdogCause(w.ctx); cause != nil {
		core.EmitError(cause)
	}
	w.cancel(nil)
}
