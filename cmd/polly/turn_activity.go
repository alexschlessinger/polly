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
	// compaction is what the turn's summaries on the compaction model
	// spent, inside the totals above, priced at compactRates: its own.
	compaction   llm.CompactionUsage
	compactRates turnRates
}

// project takes the size of the next request, in the provider's count where
// one covers it, against the budget the agent sized it to.
func (u *turnUsage) project(stats llm.ProjectionStats) {
	u.used, u.estimated = stats.CountedTokens, !stats.Counted
	u.budget = contextBudgetDetails{window: stats.Window, input: stats.Budget, response: stats.MaxTokens, learned: stats.Learned}
	u.live = true
	u.liveIn, u.liveInReported = stats.CountedTokens, false
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
		u.used, u.estimated = in, false
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

// compacted counts what a compaction summary on model spent; one the
// session's own model made ("") prices as the turn's requests do.
func (u *turnUsage) compacted(model string, usage llm.UsageUpdate) {
	u.totalIn += usage.InputTokens
	u.totalOut += usage.OutputTokens
	u.cacheRead += usage.CacheReadInputTokens
	u.cacheWrite += usage.CacheWriteInputTokens
	u.reportedCost += usage.ReportedCostUSD
	if model == "" {
		return
	}
	c := &u.compaction
	c.Input += usage.InputTokens
	c.Output += usage.OutputTokens
	c.CacheRead += usage.CacheReadInputTokens
	c.CacheWrite += usage.CacheWriteInputTokens
	c.ReportedCostUSD += usage.ReportedCostUSD
}

// settle replaces the running tallies with the finished run's usage.
func (u *turnUsage) settle(usage llm.TokenUsage) {
	u.live = false
	u.peakIn, u.totalOut, u.totalIn = usage.PeakInput, usage.TotalOutput, usage.TotalInput
	u.cacheRead, u.cacheWrite = usage.CacheRead, usage.CacheWrite
	u.reportedCost, u.compaction = usage.ReportedCostUSD, usage.Compaction
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
	c := u.compaction
	in, out, cacheRead, cacheWrite := u.totalIn-c.Input, u.totalOut-c.Output, u.cacheRead-c.CacheRead, u.cacheWrite-c.CacheWrite
	if u.live {
		in, out, cacheRead, cacheWrite = in+u.liveIn, out+u.liveOut(), cacheRead+u.liveCacheRead, cacheWrite+u.liveCacheWrite
	}
	total := priceUsage(u.rates, u.reportedCost-c.ReportedCostUSD, in, out, cacheRead, cacheWrite)
	if c.Input > 0 || c.Output > 0 {
		summaries := priceUsage(u.compactRates, c.ReportedCostUSD, c.Input, c.Output, c.CacheRead, c.CacheWrite)
		partial := total.known != summaries.known
		total = total.plus(summaries)
		total.estimated = total.estimated || partial
	}
	return total
}

// priceUsage is what usage costs: reported when a provider billed it,
// otherwise rates' estimate, unknown without rates.
func priceUsage(rates turnRates, reported float64, in, out, cacheRead, cacheWrite int) turnCost {
	if reported > 0 {
		return turnCost{usd: reported, known: true}
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
