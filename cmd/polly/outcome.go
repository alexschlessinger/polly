package main

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
)

// maxEnumeratedToolErrors caps how many tool-failure lines the trailer emits.
// tool_errors still reports the true total.
const maxEnumeratedToolErrors = 10

// exitError carries the process exit code a run should produce. main() unwraps
// it; any other error maps to exit 1.
type exitError struct {
	code int
	err  error // optional; nil for a nonzero-but-not-an-error outcome (e.g. truncation)
}

func (e *exitError) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	return fmt.Sprintf("exit status %d", e.code)
}

func (e *exitError) Unwrap() error { return e.err }

// turnProgressSavedError wraps a failed turn's error once the work generated
// before the failure has been durably persisted. Outcome classification is
// unchanged — Unwrap keeps errors.Is/As working on the cause — while UIs use
// the marker to label the failure "completed work saved" instead of "not
// saved".
type turnProgressSavedError struct{ cause error }

func (e *turnProgressSavedError) Error() string { return e.cause.Error() }
func (e *turnProgressSavedError) Unwrap() error { return e.cause }

// turnProgressSaved reports whether err records that the failed turn's
// completed work was persisted to the session.
func turnProgressSaved(err error) bool {
	var saved *turnProgressSavedError
	return errors.As(err, &saved)
}

// turnToolStats accumulates per-turn tool telemetry. The agent executes tool
// batches on parallel goroutines, so OnToolEnd fires concurrently; all access
// goes through the mutex.
type turnToolStats struct {
	mu       sync.Mutex
	calls    int
	errors   int
	lastTool string
	failures []string // "name: message" per failure; sanitized at write time
}

func (s *turnToolStats) record(name string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.lastTool = name
	if err != nil {
		s.errors++
		// The trailer enumerates at most maxEnumeratedToolErrors failures;
		// stop accumulating past that so a pathological run stays bounded.
		if len(s.failures) < maxEnumeratedToolErrors {
			s.failures = append(s.failures, name+": "+err.Error())
		}
	}
}

func (s *turnToolStats) snapshot() (calls, errCount int, lastTool string, failures []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, s.errors, s.lastTool, slices.Clone(s.failures)
}

// classifyOutcome maps an agent run's (response, error) to a terminal stop
// reason and the process exit code per the documented contract:
// 0 end_turn, 2 max_tokens, 3 max_iterations, 1 hard error.
func classifyOutcome(resp *llm.AgentResponse, err error) (messages.StopReason, int) {
	switch {
	case errors.Is(err, llm.ErrMaxIterations):
		return messages.StopReasonMaxIterations, 3
	case err != nil:
		return messages.StopReasonError, 1
	case resp != nil && resp.Message != nil && resp.Message.StopReason == messages.StopReasonMaxTokens:
		return messages.StopReasonMaxTokens, 2
	case resp != nil && resp.Message != nil && resp.Message.StopReason != "":
		return resp.Message.StopReason, 0
	default:
		return messages.StopReasonEndTurn, 0
	}
}

// metaFields is the flat run-outcome record emitted as the polly-meta trailer.
type metaFields struct {
	StopReason       messages.StopReason
	Model            string
	Iterations       int
	ToolCalls        int
	ToolErrors       int
	LastTool         string   // last tool invoked this turn (empty if none)
	ToolFailures     []string // "name: message" per tool-runner failure; capped at write time
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	DurationMS       int64
	Err              string // single-line; present only on hard error
}

// buildMeta assembles the trailer record from a turn's final state. stopReason
// must come from the same classifyOutcome call that produced the turn's exit
// code, so the trailer and the exit code cannot diverge.
func buildMeta(stopReason messages.StopReason, resp *llm.AgentResponse, err error, model string, stats *turnToolStats, inTokens, outTokens int, durationMS int64) metaFields {
	iterations := 0
	cacheRead, cacheWrite := 0, 0
	if resp != nil {
		iterations = resp.IterationCount
		cacheRead = resp.PromptCache.ReadInputTokens
		cacheWrite = resp.PromptCache.WriteInputTokens
	}
	errStr := ""
	if stopReason == messages.StopReasonError && err != nil {
		errStr = err.Error()
	}
	toolCalls, toolErrors, lastTool, toolFailures := stats.snapshot()
	return metaFields{
		StopReason:       stopReason,
		Model:            model,
		Iterations:       iterations,
		ToolCalls:        toolCalls,
		ToolErrors:       toolErrors,
		LastTool:         lastTool,
		ToolFailures:     toolFailures,
		InputTokens:      inTokens,
		OutputTokens:     outTokens,
		CacheReadTokens:  cacheRead,
		CacheWriteTokens: cacheWrite,
		DurationMS:       durationMS,
		Err:              errStr,
	}
}

// oneLine collapses newlines so a value can't break the line-oriented format.
func oneLine(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r", " "), "\n", " ")
}

// writeMetaTrailer emits the run outcome as prefixed key=value lines to w
// (stderr in practice). The fixed "polly-meta " sentinel lets a caller extract
// the record from captured stderr (`sed -n 's/^polly-meta //p'`) without it
// colliding with free-form tool-progress chrome. stdout stays the pure answer.
func writeMetaTrailer(w io.Writer, m metaFields) {
	p := func(format string, args ...any) {
		fmt.Fprintf(w, "polly-meta "+format+"\n", args...)
	}
	p("stop_reason=%s", m.StopReason)
	p("model=%s", m.Model)
	p("iterations=%d", m.Iterations)
	p("tool_calls=%d", m.ToolCalls)
	p("tool_errors=%d", m.ToolErrors)
	if m.LastTool != "" {
		p("last_tool=%s", oneLine(m.LastTool))
	}
	for i, f := range m.ToolFailures {
		if i >= maxEnumeratedToolErrors {
			break
		}
		p("tool_error.%d=%s", i+1, oneLine(f))
	}
	p("input_tokens=%d", m.InputTokens)
	p("output_tokens=%d", m.OutputTokens)
	p("cache_read_tokens=%d", m.CacheReadTokens)
	p("cache_write_tokens=%d", m.CacheWriteTokens)
	p("duration_ms=%d", m.DurationMS)
	if m.Err != "" {
		p("error=%s", oneLine(m.Err))
	}
}
