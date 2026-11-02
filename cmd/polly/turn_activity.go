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
	if retryPauseError(c.Err) {
		return turnOutcomePaused
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
	case retryPauseError(err):
		return retryPausedLabel
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
	case turnOutcomePaused:
		return retryPausedLabel
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
// last-writer-wins: the latest request's counted size owns context usage
// until the provider reports its input, which overwrites it. Peak input and
// total output accumulate across the turn's iterations.
//
// While an iteration streams, its input is the request's counted size until
// the provider reports usage, and its output is the larger of the reported
// count and an estimate from the streamed text. The iteration's close
// replaces both with the provider's final counts.
type turnUsage struct {
	// used is the latest request's size, estimated until a provider's
	// count covers it, against budget, how the agent sized the request.
	used             int
	estimated        bool
	budget           contextBudgetDetails
	peakIn, totalOut int
	totalIn          int
	// Price completed calls individually: a billed retry must not hide an
	// earlier call whose tokens can only be estimated from rates.
	completedCost turnCost
	unpricedCost  bool

	requestIn                     int
	requestEstimated              bool
	liveReportedCost              float64
	liveCostReported              bool
	missingAttemptUsage           bool
	live                          bool
	liveIn                        int
	liveInReported                bool
	liveOutReported, liveOutGuess int
	liveCacheRead, liveCacheWrite int

	rates, compactRates turnRates
}

// project takes the size of the next request, in the provider's count where
// one covers it, against the budget the agent sized it to.
func (u *turnUsage) project(stats llm.ProjectionStats) {
	u.used, u.estimated = stats.CountedTokens, !stats.Counted
	u.budget = contextBudgetDetails{window: stats.Window, input: stats.Budget, response: stats.MaxTokens, learned: stats.Learned}
	u.requestIn, u.requestEstimated = stats.CountedTokens, !stats.Counted
	u.beginAttempt()
}

// beginAttempt clears provider fields and text estimates, even when a retry
// reuses the same projection. No usage from a discarded attempt is live again.
func (u *turnUsage) beginAttempt() {
	u.used, u.estimated = u.requestIn, u.requestEstimated
	u.live = true
	u.liveIn, u.liveInReported = u.requestIn, false
	u.liveOutReported, u.liveOutGuess = 0, 0
	u.liveCacheRead, u.liveCacheWrite = 0, 0
	u.liveReportedCost, u.liveCostReported = 0, false
}

// abortAttempt retains observed spend, not speculative text estimates. The
// agent includes the same usage in its content-free usage records at settlement.
func (u *turnUsage) abortAttempt() {
	if !u.live {
		return
	}
	if !u.liveCostReported && !u.liveInReported && u.liveOutReported == 0 {
		u.missingAttemptUsage = true
	}
	in := 0
	if u.liveInReported {
		in = u.liveIn
	}
	u.record(in, u.liveOutReported)
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
	u.liveReportedCost, u.liveCostReported = usage.ReportedCostUSD, usage.CostReported
}

func (u *turnUsage) record(in, out int) {
	if in > 0 {
		u.used, u.estimated = in, false
	}
	u.peakIn = max(u.peakIn, in)
	u.totalOut += out
	u.totalIn += in
	if u.live {
		u.addCost(priceUsage(u.rates, u.liveReportedCost, u.liveCostReported, in, out, u.liveCacheRead, u.liveCacheWrite))
	} else {
		u.addCost(priceUsage(u.rates, 0, false, in, out, 0, 0))
	}
	u.live = false
}

func (u *turnUsage) addCost(cost turnCost) {
	u.completedCost = u.completedCost.plus(cost)
	u.unpricedCost = u.unpricedCost || !cost.known
}

// compacted counts what a compaction summary on model spent; one the
// session's own model made ("") prices as the turn's requests do.
func (u *turnUsage) compacted(model string, usage llm.UsageUpdate) {
	u.totalIn += usage.InputTokens
	u.totalOut += usage.OutputTokens
	rates := u.rates
	if model != "" {
		rates = u.compactRates
	}
	u.addCost(priceUsage(rates, usage.ReportedCostUSD, usage.CostReported, usage.InputTokens, usage.OutputTokens, usage.CacheReadInputTokens, usage.CacheWriteInputTokens))
}

// settle replaces the running tallies with the finished run's usage. Price
// each message separately: aggregate USD does not say which calls were billed.
func (u *turnUsage) settle(response *llm.AgentResponse) {
	usage := response.TokenUsage()
	u.live = false
	u.peakIn, u.totalOut, u.totalIn = usage.PeakInput, usage.TotalOutput, usage.TotalInput
	u.completedCost, u.unpricedCost = turnCost{}, false
	for _, msg := range response.AllMessages {
		if !msg.ReportsUsage() {
			continue
		}
		rates := u.rates
		if msg.IsUsageRecord() && msg.UsageModel() != "" {
			rates = u.compactRates
		}
		cost, reported := msg.GetReportedCost()
		u.addCost(priceUsage(rates, cost, reported, msg.GetInputTokens(), msg.GetOutputTokens(), msg.GetCacheReadInputTokens(), msg.GetCacheWriteInputTokens()))
	}
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

// cost returns the turn's cost: what its provider billed when it reported a
// cost, otherwise an estimate from the model's advertised rates, plus what
// its compaction summaries cost, priced the same way at the compaction
// model's. When only one of the two can be priced, its cost stands for the
// turn's, as an estimate.
func (u *turnUsage) cost() turnCost {
	total := u.completedCost
	missing := u.unpricedCost || u.missingAttemptUsage
	if u.live {
		live := priceUsage(u.rates, u.liveReportedCost, u.liveCostReported, u.liveIn, u.liveOut(), u.liveCacheRead, u.liveCacheWrite)
		total = total.plus(live)
		missing = missing || !live.known
	}
	total.estimated = total.estimated || (total.known && missing)
	return total
}

// priceUsage is what one call costs: reported when a provider billed it,
// otherwise rates' estimate, unknown without rates.
func priceUsage(rates turnRates, cost float64, reported bool, in, out, cacheRead, cacheWrite int) turnCost {
	if reported || cost > 0 {
		return turnCost{usd: cost, known: true}
	}
	if !rates.known {
		return turnCost{}
	}
	return turnCost{usd: rates.cost(in, out, cacheRead, cacheWrite), known: true, estimated: true}
}

// turnCacheUsage weights cache hits by all reported input tokens, not the
// largest context window. Missing cache accounting makes the rate unknown.
type turnCacheUsage struct {
	input, read       int
	reported, missing bool
}

func (c *turnCacheUsage) add(msg messages.ChatMessage) {
	if !msg.ReportsUsage() {
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
