package llm

import (
	"context"
	"time"

	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/skills"
	"github.com/alexschlessinger/pollytool/tools"
)

// LLM interface defines the contract for language model implementations
type LLM interface {
	// Event-based streaming method
	ChatCompletionStream(context.Context, *CompletionRequest, EventStreamProcessor) <-chan *messages.StreamEvent
}

// EventStreamProcessor processes message streams into events
type EventStreamProcessor interface {
	ProcessMessagesToEvents(<-chan messages.ChatMessage) <-chan *messages.StreamEvent
}

// Schema is a type alias so callers can use llm.Schema without importing schema.
type Schema = schema.Schema

// ToolSchema is a type alias so callers can use llm.ToolSchema without importing schema.
type ToolSchema = schema.ToolSchema

// SchemaFor generates a strict JSON schema from a Go struct using reflection.
func SchemaFor(v any) *Schema { return schema.SchemaFor(v) }

// SchemaFromJSON parses a JSON schema string into a strict Schema.
func SchemaFromJSON(s string) *Schema { return schema.SchemaFromJSON(s) }

// Float32Ptr returns a pointer to v. Convenience constructor for optional
// float32 fields like CompletionRequest.Temperature, where nil means "don't
// send the field" (some reasoning models reject `temperature` outright).
func Float32Ptr(v float32) *float32 { return &v }

// CompletionRequest contains all parameters for a completion request
type CompletionRequest struct {
	APIKey  string
	BaseURL string
	// Timeout is the stream stall budget, applied uniformly across providers:
	// a completion is canceled once no provider data has arrived for this
	// long (for a non-streaming call, once it has gone this long without
	// returning). It bounds silence, not total generation time — every chunk
	// resets the clock. Zero disables the watchdog.
	Timeout time.Duration
	// Deadline is the hard per-call wall-clock ceiling: the completion is
	// canceled once it has run this long in total, even while data is still
	// arriving. It backstops endpoints that trickle keepalive data forever.
	// Zero means no ceiling.
	Deadline time.Duration
	// Temperature controls sampling when non-nil. Leave nil to omit the
	// parameter from the upstream request — required for reasoning models
	// (o1, o3, gpt-5.x) which 400 if temperature is supplied at all.
	Temperature *float32
	Model       string
	MaxTokens   int
	// MaxContextTokens limits the deterministic provider-visible projection
	// used by Agent. Direct provider clients ignore it. Zero is unlimited.
	MaxContextTokens int
	// PromptCacheKey groups requests with the same stable agent prefix for
	// provider-side prompt caching. Agent derives one when this is empty.
	PromptCacheKey string
	// CacheSessionID is an opaque, stable per-session routing identity. It is
	// currently used only by providers that support session affinity.
	CacheSessionID string
	Messages       []messages.ChatMessage // Message history
	Tools          []tools.Tool           // Available tools
	ResponseSchema *Schema                // Optional schema for structured output
	ThinkingEffort ThinkingEffort         // Reasoning effort: Off, a named Level, a raw token Budget, or Dynamic
	Stream         *bool                  // nil = streaming (default), false = non-streaming
	Skills         *skills.Catalog        // Optional skill catalog for automatic system prompt augmentation
}

// ResolvedMessages returns a copy of Messages with skill prompt injected.
// No-op when Skills is nil or empty.
func (r *CompletionRequest) ResolvedMessages() []messages.ChatMessage {
	out := make([]messages.ChatMessage, len(r.Messages))
	copy(out, r.Messages)
	if r.Skills == nil || r.Skills.IsEmpty() {
		return out
	}
	basePrompt := ""
	if len(out) > 0 && out[0].Role == messages.MessageRoleSystem {
		basePrompt = out[0].Content
	}
	runtimeSystem := r.Skills.RuntimeSystemPrompt(basePrompt)
	if len(out) > 0 && out[0].Role == messages.MessageRoleSystem {
		out[0].Content = runtimeSystem
		return out
	}
	return append([]messages.ChatMessage{{
		Role:    messages.MessageRoleSystem,
		Content: runtimeSystem,
	}}, out...)
}

// runStream handles the common goroutine scaffolding for ChatCompletionStream.
// Each provider creates its adapter, then passes a function that does the
// provider-specific work with the StreamingCore, using the context runStream
// hands it for every upstream call.
//
// Two watchdog budgets apply uniformly to every provider; zero disables each.
// stallTimeout cancels the stream (streaming.StallError cause) when no
// provider data arrives for that long — it bounds silence, not total
// generation time, and every chunk resets it. deadline cancels the stream
// (streaming.DeadlineError cause) once the whole call has run that long,
// whether or not data is still trickling in.
func runStream(ctx context.Context, stallTimeout, deadline time.Duration, processor EventStreamProcessor, adapter streaming.ProviderAdapter, fn func(context.Context, *streaming.StreamingCore)) <-chan *messages.StreamEvent {
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
