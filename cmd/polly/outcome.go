package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
)

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

// metaFields is the flat outcome record written to a --meta-out sidecar.
type metaFields struct {
	StopReason   messages.StopReason
	Model        string
	Iterations   int
	ToolCalls    int
	ToolErrors   int
	InputTokens  int
	OutputTokens int
	DurationMS   int64
	Err          string // single-line; present only on hard error
}

// buildMeta assembles the sidecar record from a run's results. It classifies
// the outcome so stop_reason and the error line stay consistent with the exit
// code.
func buildMeta(resp *llm.AgentResponse, err error, model string, toolCalls, toolErrors, inTokens, outTokens int, durationMS int64) metaFields {
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

// writeMetaOut writes the record as flat key=value lines, atomically
// (temp file + rename) so a partial write never leaves a half-written sidecar.
func writeMetaOut(path string, m metaFields) error {
	var b strings.Builder
	fmt.Fprintf(&b, "stop_reason=%s\n", m.StopReason)
	fmt.Fprintf(&b, "model=%s\n", m.Model)
	fmt.Fprintf(&b, "iterations=%d\n", m.Iterations)
	fmt.Fprintf(&b, "tool_calls=%d\n", m.ToolCalls)
	fmt.Fprintf(&b, "tool_errors=%d\n", m.ToolErrors)
	fmt.Fprintf(&b, "input_tokens=%d\n", m.InputTokens)
	fmt.Fprintf(&b, "output_tokens=%d\n", m.OutputTokens)
	fmt.Fprintf(&b, "duration_ms=%d\n", m.DurationMS)
	if m.Err != "" {
		fmt.Fprintf(&b, "error=%s\n", m.Err)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
