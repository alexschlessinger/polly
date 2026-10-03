package main

import (
	"math"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
)

func TestFormatCostUSD(t *testing.T) {
	for _, tc := range []struct {
		usd  float64
		want string
	}{
		{0, "$0"},
		{0.0043210, "$0.004321"},
		{1.23456, "$1.235"},
		{12.3456, "$12.35"},
		{0.01, "$0.01"},
		{1234.6, "$1235"},
		{0.000000012345, "$0.00000001235"},
		{math.NaN(), "$0"},
	} {
		if got := formatCostUSD(tc.usd); got != tc.want {
			t.Fatalf("formatCostUSD(%v) = %q, want %q", tc.usd, got, tc.want)
		}
	}
}

func TestTurnRatesCostAdjustsCacheSubsets(t *testing.T) {
	rates := turnRates{in: 2e-6, out: 8e-6, cacheRead: 0.5e-6, hasCacheRead: true, cacheWrite: 2.5e-6, hasCacheWrite: true, known: true}
	// 1000 input of which 600 read and 100 written, 200 output.
	got := rates.cost(1000, 200, 600, 100)
	want := 1000*2e-6 + 200*8e-6 + 600*(0.5e-6-2e-6) + 100*(2.5e-6-2e-6)
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("cost = %v, want %v", got, want)
	}
	// Without cache rates, cached tokens bill at the input rate.
	rates.hasCacheRead, rates.hasCacheWrite = false, false
	if got, want := rates.cost(1000, 200, 600, 100), 1000*2e-6+200*8e-6; math.Abs(got-want) > 1e-12 {
		t.Fatalf("cost without cache rates = %v, want %v", got, want)
	}
}

func TestTurnUsageLiveEstimatesSnapToReportedUsage(t *testing.T) {
	u := turnUsage{rates: turnRates{in: 1e-6, out: 1e-5, known: true}}
	u.project(llm.ProjectionStats{CountedTokens: 1000})
	u.streamed(strings.Repeat("x", 400))
	if in, out, est := u.tokens(); in != 1000 || out != 100 || !est {
		t.Fatalf("streaming tokens = %d/%d est=%v, want 1000/100 estimated", in, out, est)
	}
	if c := u.cost(); !c.known || !c.estimated || math.Abs(c.usd-(1000e-6+100e-5)) > 1e-12 {
		t.Fatalf("live cost = %+v", c)
	}
	// Reported input replaces the projection; output stays the larger of the
	// reported count and the streamed estimate.
	u.progress(llm.UsageUpdate{InputTokens: 900, OutputTokens: 1})
	if in, out, est := u.tokens(); in != 900 || out != 100 || !est {
		t.Fatalf("after progress = %d/%d est=%v", in, out, est)
	}
	u.progress(llm.UsageUpdate{InputTokens: 900, OutputTokens: 120})
	if in, out, est := u.tokens(); in != 900 || out != 120 || est {
		t.Fatalf("after full report = %d/%d est=%v, want measured", in, out, est)
	}
	u.record(900, 120)
	if in, out, est := u.tokens(); in != 900 || out != 120 || est {
		t.Fatalf("after close = %d/%d est=%v", in, out, est)
	}
	// A second iteration adds output and prices cumulative input.
	u.project(llm.ProjectionStats{CountedTokens: 1200})
	u.streamed(strings.Repeat("x", 40))
	if in, out, est := u.tokens(); in != 1200 || out != 130 || !est {
		t.Fatalf("second iteration = %d/%d est=%v", in, out, est)
	}
	u.settle(llm.TokenUsage{TotalInput: 2100, TotalOutput: 135, PeakInput: 1200, ReportedCostUSD: 0.02})
	if c := u.cost(); c != (turnCost{usd: 0.02, known: true}) {
		t.Fatalf("billed cost = %+v, want exact 0.02", c)
	}
	if in, out, est := u.tokens(); in != 1200 || out != 135 || est {
		t.Fatalf("settled tokens = %d/%d est=%v", in, out, est)
	}
}

func TestTurnUsageCostUnknownWithoutRates(t *testing.T) {
	var u turnUsage
	u.project(llm.ProjectionStats{CountedTokens: 10})
	u.record(10, 5)
	if c := u.cost(); c.known {
		t.Fatalf("cost without rates = %+v", c)
	}
}

func TestSessionSpendTotals(t *testing.T) {
	var s sessionSpend
	if s.total().known {
		t.Fatal("empty spend is known")
	}
	s.add(turnCost{usd: 0.5, known: true})
	s.observeTurn(turnCost{usd: 0.25, known: true, estimated: true})
	if got := s.total(); got != (turnCost{usd: 0.75, known: true, estimated: true}) {
		t.Fatalf("with a turn in flight = %+v", got)
	}
	// The turn's final billed cost replaces its live estimate.
	s.finishTurn(turnCost{usd: 0.2, known: true}, true)
	if got := s.total(); math.Abs(got.usd-0.7) > 1e-12 || !got.known || got.estimated {
		t.Fatalf("after the turn = %+v", got)
	}
	// A turn that never reached the model leaves the total exact.
	s.finishTurn(turnCost{}, false)
	if s.total().estimated {
		t.Fatal("unspent turn marked the total as an estimate")
	}
	s.finishTurn(turnCost{}, true)
	if !s.total().estimated {
		t.Fatal("unpriced spend left the total exact")
	}
}

// A compaction summary on a cheaper model is priced at its own rates, and a
// cost only the compaction provider reported does not stand for the turn's.
func TestTurnUsagePricesCompactionAtItsModelsRates(t *testing.T) {
	u := turnUsage{rates: turnRates{known: true, in: 10, out: 10}, compactRates: turnRates{known: true, in: 1, out: 1}}
	u.settle(llm.TokenUsage{
		TotalInput: 1_100, TotalOutput: 110, PeakInput: 100,
		Compaction: llm.CompactionUsage{Model: "cheap/model", Input: 1_000, Output: 100},
	})
	want := u.rates.cost(100, 10, 0, 0) + u.compactRates.cost(1_000, 100, 0, 0)
	if c := u.cost(); !c.known || math.Abs(c.usd-want) > 1e-12 {
		t.Fatalf("cost = %+v, want %v", c, want)
	}
	u.settle(llm.TokenUsage{
		TotalInput: 1_100, TotalOutput: 110, PeakInput: 100, ReportedCostUSD: 0.5,
		Compaction: llm.CompactionUsage{Model: "openrouter/cheap", Input: 1_000, Output: 100, ReportedCostUSD: 0.5},
	})
	want = 0.5 + u.rates.cost(100, 10, 0, 0)
	if c := u.cost(); !c.known || !c.estimated || math.Abs(c.usd-want) > 1e-12 {
		t.Fatalf("cost = %+v, want the summary's billed cost plus the turn's estimate %v", c, want)
	}
}

// Summaries on a compaction model without prices leave the turn's own cost
// standing, as an estimate, rather than making it unknown.
func TestTurnUsageKeepsTheCostItCanPrice(t *testing.T) {
	usage := llm.TokenUsage{
		TotalInput: 1_100, TotalOutput: 110, PeakInput: 100,
		Compaction: llm.CompactionUsage{Model: "ollama/local", Input: 1_000, Output: 100},
	}
	u := turnUsage{rates: turnRates{known: true, in: 10, out: 10}}
	u.settle(usage)
	if c := u.cost(); !c.known || !c.estimated || math.Abs(c.usd-u.rates.cost(100, 10, 0, 0)) > 1e-12 {
		t.Fatalf("cost = %+v, want the turn's requests as an estimate", c)
	}
	u = turnUsage{compactRates: turnRates{known: true, in: 1, out: 1}}
	u.settle(usage)
	if c := u.cost(); !c.known || !c.estimated || math.Abs(c.usd-u.compactRates.cost(1_000, 100, 0, 0)) > 1e-12 {
		t.Fatalf("cost = %+v, want the summaries as an estimate", c)
	}
	u = turnUsage{}
	u.settle(usage)
	if c := u.cost(); c.known {
		t.Fatalf("cost = %+v, want unknown", c)
	}
}

func TestMemberSpendCountsCompactions(t *testing.T) {
	var s sessionSpend
	cheap := turnRates{known: true, in: 1, out: 2}
	m := &memberSpend{spend: &s, rates: func() turnRates { return turnRates{} }, modelRates: func(model string) turnRates {
		if model == "cheap/model" {
			return cheap
		}
		return turnRates{}
	}}
	m.compaction("cheap/model", llm.UsageUpdate{InputTokens: 100, OutputTokens: 10})
	if c := s.total(); !c.known || math.Abs(c.usd-cheap.cost(100, 10, 0, 0)) > 1e-12 {
		t.Fatalf("member spend = %+v, want the summary at the compaction model's rates", c)
	}
	// A summary the member's own model made is priced at its rates.
	own := turnRates{known: true, in: 5, out: 5}
	m.rates = func() turnRates { return own }
	m.compaction("", llm.UsageUpdate{InputTokens: 100, OutputTokens: 10})
	if c := s.total(); !c.known || math.Abs(c.usd-cheap.cost(100, 10, 0, 0)-own.cost(100, 10, 0, 0)) > 1e-12 {
		t.Fatalf("member spend = %+v, want the second summary at the member model's rates", c)
	}
}

// A summary the session's own model made, when the compaction model could
// not, is priced as the turn's requests are.
func TestTurnUsagePricesOwnModelSummariesAsRequests(t *testing.T) {
	u := turnUsage{rates: turnRates{known: true, in: 10, out: 10}, compactRates: turnRates{known: true, in: 1, out: 1}}
	u.compacted("", llm.UsageUpdate{InputTokens: 1_000, OutputTokens: 100})
	u.record(100, 10)
	want := u.rates.cost(1_100, 110, 0, 0)
	if c := u.cost(); !c.known || math.Abs(c.usd-want) > 1e-12 || u.compaction != (llm.CompactionUsage{}) {
		t.Fatalf("cost = %+v with compaction share %+v, want %v", c, u.compaction, want)
	}
}
