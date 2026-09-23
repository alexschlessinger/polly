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
	u.project(llm.ProjectionStats{RequestEstimatedTokens: 1000}, 0)
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
	u.project(llm.ProjectionStats{RequestEstimatedTokens: 1200}, 0)
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
	u.project(llm.ProjectionStats{RequestEstimatedTokens: 10}, 0)
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
