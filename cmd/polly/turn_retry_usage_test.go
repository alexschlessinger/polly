package main

import (
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
)

func TestTurnRetryUsagePreservesOnlyObservedFailedSpend(t *testing.T) {
	u := turnUsage{rates: turnRates{known: true, in: 1, out: 2}}
	u.project(llm.ProjectionStats{CountedTokens: 100})
	u.progress(llm.UsageUpdate{InputTokens: 80, OutputTokens: 7, CacheReadInputTokens: 30, ReportedCostUSD: 3})
	u.streamed("speculative text that should not be charged again")
	u.abortAttempt()
	if c := u.cost(); c.usd != 3 || !c.known || u.totalOut != 7 {
		t.Fatalf("failed observed usage lost: cost=%+v usage=%+v", c, u)
	}
	u.beginAttempt()
	if u.liveIn != 100 || u.liveOut() != 0 || u.liveInReported || u.liveCacheRead != 0 || u.liveReportedCost != 0 {
		t.Fatalf("provider fields leaked across attempts: %+v", u)
	}
	if c := u.cost(); c.usd != 103 || !c.estimated {
		t.Fatalf("failed billed cost replaced new attempt estimate: %+v", c)
	}
	u.progress(llm.UsageUpdate{InputTokens: 90, OutputTokens: 10, ReportedCostUSD: 4})
	u.record(90, 10)
	if c := u.cost(); c.usd != 7 || u.totalIn != 170 || u.totalOut != 17 {
		t.Fatalf("accepted call lost or duplicated failed spend: cost=%+v usage=%+v", c, u)
	}
	u.settle(llm.TokenUsage{TotalInput: 170, TotalOutput: 17, PeakInput: 90, CacheRead: 30, ReportedCostUSD: 7})
	if c := u.cost(); c.usd != 7 || u.totalIn != 170 || u.totalOut != 17 {
		t.Fatalf("settlement double-counted failed spend: cost=%+v usage=%+v", c, u)
	}
}

func TestTurnRetryUnknownUsageLeavesSettledCostEstimated(t *testing.T) {
	u := turnUsage{}
	u.project(llm.ProjectionStats{CountedTokens: 100})
	u.abortAttempt()
	u.settle(llm.TokenUsage{TotalInput: 90, TotalOutput: 10, PeakInput: 90, ReportedCostUSD: 4})
	if c := u.cost(); c.usd != 4 || !c.known || !c.estimated {
		t.Fatalf("unknown failed usage was treated as exact zero: %+v", c)
	}
}

func TestMemberRetrySpendResetsAndCountsAbortsOnce(t *testing.T) {
	var s sessionSpend
	m := memberSpend{spend: &s, rates: func() turnRates { return turnRates{known: true, in: 1, out: 2} }}
	m.progress(llm.UsageUpdate{InputTokens: 80, OutputTokens: 7, ReportedCostUSD: 3, CostReported: true})
	m.abortAttempt()
	m.beginAttempt()
	if m.last != (llm.UsageUpdate{}) {
		t.Fatalf("retry retained provider usage: %+v", m.last)
	}
	m.iteration(0, 2, 3)
	if c := s.total(); c.usd != 11 || !c.estimated {
		t.Fatalf("failed or accepted usage miscounted: %+v", c)
	}
	m.beginAttempt()
	m.abortAttempt() // Unknown usage must not be recorded as an exact free call.
	if !s.missing {
		t.Fatal("unknown failed spend was treated as zero")
	}
}
