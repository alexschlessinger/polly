package main

import (
	"errors"
	"fmt"
	"io"
	"strings"

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

// isExitCodeOnly reports whether err is an exitError carrying only a process
// exit code with no underlying error (e.g. a truncated-but-complete run that
// exits 2). Such errors are meaningful only to main() in one-shot mode; the
// interactive REPLs should ignore them rather than print a spurious
// "Error: exit status N" for what is a clean, warning-only outcome.
func isExitCodeOnly(err error) bool {
	var ee *exitError
	return errors.As(err, &ee) && ee.err == nil
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
	StopReason   messages.StopReason
	Model        string
	Iterations   int
	ToolCalls    int
	ToolErrors   int
	LastTool     string   // last tool invoked this turn (empty if none)
	ToolFailures []string // "name: message" per tool-runner failure; capped at write time
	InputTokens  int
	OutputTokens int
	DurationMS   int64
	Err          string // single-line; present only on hard error
}

// buildMeta assembles the sidecar record from a run's results. It classifies
// the outcome so stop_reason and the error line stay consistent with the exit
// code.
func buildMeta(resp *llm.AgentResponse, err error, model string, toolCalls, toolErrors, inTokens, outTokens int, durationMS int64, lastTool string, toolFailures []string) metaFields {
	stopReason, _ := classifyOutcome(resp, err)
	iterations := 0
	if resp != nil {
		iterations = resp.IterationCount
	}
	errStr := ""
	if stopReason == messages.StopReasonError && err != nil {
		errStr = oneLine(err.Error())
	}
	return metaFields{
		StopReason:   stopReason,
		Model:        model,
		Iterations:   iterations,
		ToolCalls:    toolCalls,
		ToolErrors:   toolErrors,
		LastTool:     lastTool,
		ToolFailures: toolFailures,
		InputTokens:  inTokens,
		OutputTokens: outTokens,
		DurationMS:   durationMS,
		Err:          errStr,
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
	p("duration_ms=%d", m.DurationMS)
	if m.Err != "" {
		p("error=%s", m.Err)
	}
}
