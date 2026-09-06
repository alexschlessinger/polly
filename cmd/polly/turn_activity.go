package main

import (
	"context"
	"errors"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
)

// turnCompletion describes the whole turn, including persistence and output.
// It is distinct from finishing an assistant text segment.
type turnCompletion struct {
	Reason        messages.StopReason
	Err           error
	Elapsed       time.Duration
	ProgressSaved bool
}

func (c turnCompletion) outcome() turnOutcome {
	if activityCanceled(c.Err) {
		return turnOutcomeCanceled
	}
	if c.Err != nil && !onlyIterationLimit(c.Err) {
		return turnOutcomeFailed
	}
	if c.Reason == messages.StopReasonMaxTokens || c.Reason == messages.StopReasonMaxIterations || onlyIterationLimit(c.Err) {
		return turnOutcomeIncomplete
	}
	return turnOutcomeDone
}

func activityCanceled(err error) bool {
	var signal *shutdownSignal
	return errors.Is(err, context.Canceled) || errors.As(err, &signal)
}

// A joined persistence/output error must not become a recoverable iteration
// limit just because another branch of its error chain is that sentinel.
func onlyIterationLimit(err error) bool {
	if err == llm.ErrMaxIterations {
		return true
	}
	if multi, ok := err.(interface{ Unwrap() []error }); ok {
		children := multi.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !onlyIterationLimit(child) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return onlyIterationLimit(wrapped.Unwrap())
	}
	return false
}

func toolActivityOutcome(denied bool, err error) string {
	switch {
	case denied:
		return "denied"
	case activityCanceled(err):
		return "canceled"
	case err != nil:
		return "failed"
	default:
		return "done"
	}
}

func turnOutcomeLabel(outcome turnOutcome) string {
	switch outcome {
	case turnOutcomeCanceled:
		return "canceled"
	case turnOutcomeFailed:
		return "failed"
	case turnOutcomeIncomplete:
		return "incomplete"
	default:
		return "done"
	}
}

// These values describe parent-turn activity, independently of either UI's
// disclosure records, terminal rows, or child runtime ownership.
type turnActivitySummary struct {
	Reasoned bool
	Thought  time.Duration
	Tools    int
	Images   int
	Agents   activityAgentCounts
	Outcome  turnOutcome
	Elapsed  time.Duration
	In, Out  int
}

type activityAgentCounts struct {
	Total, Running, Failed, Canceled int
}

func (c *activityAgentCounts) add(status string, active bool) {
	c.Total++
	switch {
	case active:
		c.Running++
	case status == "failed", status == "denied":
		c.Failed++
	case status == "canceled":
		c.Canceled++
	}
}

type iterationUsage struct{ in, out int }

// The latest projected request owns context usage, even when it fails before
// reporting measured usage. Token totals retain completed iterations only.
type turnUsage struct {
	iterations     map[int]iterationUsage
	latest         int
	projected      bool
	used           int
	limit          int
	estimated      bool
	projectionUsed int
}

func (u *turnUsage) project(iteration int, stats llm.ProjectionStats, limit int) {
	u.latest, u.projected = iteration, true
	u.used, u.limit, u.estimated = stats.RequestEstimatedTokens, limit, true
	u.projectionUsed = stats.RequestEstimatedTokens
}

func (u *turnUsage) record(iteration, in, out int) (int, int) {
	if u.iterations == nil {
		u.iterations = make(map[int]iterationUsage)
	}
	u.iterations[iteration] = iterationUsage{max(0, in), max(0, out)}
	if iteration == u.latest {
		if in > 0 {
			u.used, u.estimated = in, false
		} else {
			u.used, u.estimated = u.projectionUsed, true
		}
	}
	var peak, total int
	for _, usage := range u.iterations {
		peak = max(peak, usage.in)
		total += usage.out
	}
	return peak, total
}
