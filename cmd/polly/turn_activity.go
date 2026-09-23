package main

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/swarm"
	"github.com/alexschlessinger/pollytool/tools"
)

// turnCompletion describes the whole turn, including persistence and output.
// It is distinct from finishing an assistant text segment.
type turnCompletion struct {
	Reason        messages.StopReason
	Err           error
	Elapsed       time.Duration
	ProgressSaved bool
	Cache         turnCacheUsage
}

func (c turnCompletion) outcome() turnOutcome {
	if activityCanceled(c.Err) {
		return turnOutcomeCanceled
	}
	if c.Err != nil && !llm.IsIterationLimit(c.Err) {
		return turnOutcomeFailed
	}
	if c.Reason == messages.StopReasonMaxTokens || c.Reason == messages.StopReasonMaxIterations || llm.IsIterationLimit(c.Err) {
		return turnOutcomeIncomplete
	}
	return turnOutcomeDone
}

func activityCanceled(err error) bool {
	var signal *shutdownSignal
	return errors.Is(err, context.Canceled) || errors.As(err, &signal)
}

func toolActivityOutcome(denied bool, err error) string {
	var toolErr *tools.ToolError
	switch {
	case denied:
		return "denied"
	case activityCanceled(err):
		return "canceled"
	case llm.IsIterationLimit(err) || errors.As(err, &toolErr) && toolErr.Code == "ITERATION_LIMIT":
		return "paused · iteration limit"
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
	Cost     turnCost
}

type activityAgentCounts struct {
	Total, Running, Failed, Canceled, Paused, Deferred int
}

// add counts an agent by its presentation. Interrupted and stopped members
// are paused; only launch-call outcomes can be canceled.
func (c *activityAgentCounts) add(p swarm.AgentPresentation) {
	c.Total++
	if p.Deferred {
		c.Deferred++
	}
	switch {
	case p.Busy:
		c.Running++
	case p.Outcome == "failed":
		c.Failed++
	case p.Lifecycle == swarm.LifecyclePaused:
		c.Paused++
	}
}

// addOutcome counts a launch call by its own word: the line UI's launches
// and rows that never attached to a member.
func (c *activityAgentCounts) addOutcome(word string, busy bool) {
	c.Total++
	switch {
	case busy:
		c.Running++
	case word == "failed", word == "denied":
		c.Failed++
	case word == "canceled":
		c.Canceled++
	case strings.HasPrefix(word, "paused"):
		c.Paused++
	}
}

// The agent loop is strictly sequential, so turn bookkeeping is
// last-writer-wins: the latest projection owns context usage until a provider
// reports measured input, which overwrites it. Peak input and total output
// accumulate across the turn's iterations.
//
// While an iteration streams, its input is the projection estimate until the
// provider reports usage, and its output is the larger of the reported count
// and an estimate from the streamed text. The iteration's close replaces both
// with the provider's final counts.
type turnUsage struct {
	used, limit      int
	peakIn, totalOut int
	// totalIn and the cache counts accumulate completed iterations, for
	// pricing.
	totalIn, cacheRead, cacheWrite int

	live                          bool
	liveIn                        int
	liveInReported                bool
	liveOutReported, liveOutGuess int
	liveCacheRead, liveCacheWrite int

	rates        turnRates
	reportedCost float64
	costReported bool
}

func (u *turnUsage) project(stats llm.ProjectionStats, limit int) {
	u.used, u.limit = stats.RequestEstimatedTokens, limit
	u.live = true
	u.liveIn, u.liveInReported = stats.RequestEstimatedTokens, false
	u.liveOutReported, u.liveOutGuess = 0, 0
	u.liveCacheRead, u.liveCacheWrite = 0, 0
}

// streamed counts text the in-flight iteration produced toward its estimated
// output.
func (u *turnUsage) streamed(text string) {
	if u.live {
		u.liveOutGuess += messages.EstimatedStringTokens(text)
	}
}

// progress takes the provider's usage so far for the in-flight iteration.
func (u *turnUsage) progress(usage llm.UsageUpdate) {
	if !u.live {
		return
	}
	if usage.InputTokens > 0 {
		u.liveIn, u.liveInReported = usage.InputTokens, true
	}
	u.liveOutReported = usage.OutputTokens
	u.liveCacheRead, u.liveCacheWrite = usage.CacheReadInputTokens, usage.CacheWriteInputTokens
}

func (u *turnUsage) record(in, out int) {
	if in > 0 {
		u.used = in
	}
	u.peakIn = max(u.peakIn, in)
	u.totalOut += out
	u.totalIn += in
	if u.live {
		u.cacheRead += u.liveCacheRead
		u.cacheWrite += u.liveCacheWrite
	}
	u.live = false
}

// settle replaces the running tallies with the finished run's usage.
func (u *turnUsage) settle(usage llm.TokenUsage) {
	u.live = false
	u.peakIn, u.totalOut, u.totalIn = usage.PeakInput, usage.TotalOutput, usage.TotalInput
	u.cacheRead, u.cacheWrite = usage.CacheRead, usage.CacheWrite
	u.reportedCost, u.costReported = usage.ReportedCostUSD, usage.ReportedCostUSD > 0
}

func (u *turnUsage) liveOut() int {
	return max(u.liveOutReported, u.liveOutGuess)
}

// tokens returns the status row's peak input and total output, and whether
// either includes an estimate for the in-flight iteration.
func (u *turnUsage) tokens() (in, out int, estimated bool) {
	in, out = u.peakIn, u.totalOut
	if u.live {
		in = max(in, u.liveIn)
		out += u.liveOut()
		estimated = !u.liveInReported || u.liveOutGuess > u.liveOutReported
	}
	return in, out, estimated
}

// cost returns the turn's billed cost when the provider reported one, and
// otherwise an estimate from the model's advertised rates.
func (u *turnUsage) cost() turnCost {
	if u.costReported {
		return turnCost{usd: u.reportedCost, known: true}
	}
	if !u.rates.known {
		return turnCost{}
	}
	in, out, cacheRead, cacheWrite := u.totalIn, u.totalOut, u.cacheRead, u.cacheWrite
	if u.live {
		in += u.liveIn
		out += u.liveOut()
		cacheRead += u.liveCacheRead
		cacheWrite += u.liveCacheWrite
	}
	return turnCost{usd: u.rates.cost(in, out, cacheRead, cacheWrite), known: true, estimated: true}
}

// turnCacheUsage weights cache hits by all reported input tokens, not the
// largest context window. Missing cache accounting makes the rate unknown.
type turnCacheUsage struct {
	input, read       int
	reported, missing bool
}

func (c *turnCacheUsage) add(msg messages.ChatMessage) {
	if msg.Role != messages.MessageRoleAssistant {
		return
	}
	input := msg.GetInputTokens()
	if input <= 0 {
		return
	}
	c.input += input
	if _, ok := msg.Metadata[messages.MetadataKeyCacheReadInputTokens]; !ok {
		c.missing = true
		return
	}
	c.reported = true
	c.read += msg.GetCacheReadInputTokens()
}
