package main

import (
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
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
	failed := turnUsageTestMessage(80, 7, 3).UsageRecord()
	failed.SetPromptCacheUsage(30, 0)
	u.settle(&llm.AgentResponse{AllMessages: []messages.ChatMessage{failed, turnUsageTestMessage(90, 10, 4)}})
	if c := u.cost(); c.usd != 7 || u.totalIn != 170 || u.totalOut != 17 {
		t.Fatalf("settlement double-counted failed spend: cost=%+v usage=%+v", c, u)
	}
}

func TestTurnRetryUnknownUsageLeavesSettledCostEstimated(t *testing.T) {
	u := turnUsage{}
	u.project(llm.ProjectionStats{CountedTokens: 100})
	u.abortAttempt()
	u.settle(&llm.AgentResponse{AllMessages: []messages.ChatMessage{turnUsageTestMessage(90, 10, 4)}})
	if c := u.cost(); c.usd != 4 || !c.known || !c.estimated {
		t.Fatalf("unknown failed usage was treated as exact zero: %+v", c)
	}
}

func TestTurnUsagePricesCallsSeparately(t *testing.T) {
	for _, tc := range []struct {
		name       string
		model      string
		failed     bool
		firstCost  []float64
		secondCost []float64
		noRates    bool
		cache      bool
		want       turnCost
	}{
		{name: "unbilled failure then billed success", failed: true, secondCost: []float64{4}, want: turnCost{usd: 98, known: true, estimated: true}},
		{name: "billed failure then unbilled success", failed: true, firstCost: []float64{3}, want: turnCost{usd: 113, known: true, estimated: true}},
		{name: "successful iterations", secondCost: []float64{4}, want: turnCost{usd: 98, known: true, estimated: true}},
		{name: "own-model summaries", model: "own", secondCost: []float64{4}, want: turnCost{usd: 98, known: true, estimated: true}},
		{name: "compaction-model summaries", model: "cheap/model", secondCost: []float64{4}, want: turnCost{usd: 27.5, known: true, estimated: true}},
		{name: "unpriced failure", failed: true, noRates: true, secondCost: []float64{4}, want: turnCost{usd: 4, known: true, estimated: true}},
		{name: "free failure then billed success", failed: true, firstCost: []float64{0}, secondCost: []float64{4}, want: turnCost{usd: 4, known: true}},
		{name: "unbilled failure then free success", failed: true, secondCost: []float64{0}, want: turnCost{usd: 94, known: true, estimated: true}},
		{name: "cache-priced failure", failed: true, cache: true, secondCost: []float64{4}, want: turnCost{usd: 85.5, known: true, estimated: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := turnUsage{rates: turnRates{known: !tc.noRates, in: 1, out: 2, hasCacheRead: true, cacheRead: 0.5, hasCacheWrite: true, cacheWrite: 1.25},
				compactRates: turnRates{known: true, in: 0.25, out: 0.5}}
			first := turnUsageTestMessage(80, 7, tc.firstCost...)
			if tc.cache {
				first.SetPromptCacheUsage(30, 10)
			}
			second := turnUsageTestMessage(90, 10, tc.secondCost...)
			calls := []messages.ChatMessage{first, second}
			for i, msg := range calls {
				cost, reported := msg.GetReportedCost()
				update := llm.UsageUpdate{InputTokens: msg.GetInputTokens(), OutputTokens: msg.GetOutputTokens(),
					CacheReadInputTokens: msg.GetCacheReadInputTokens(), CacheWriteInputTokens: msg.GetCacheWriteInputTokens(),
					ReportedCostUSD: cost, CostReported: reported}
				if tc.model != "" {
					model := tc.model
					if model == "own" {
						model = ""
					}
					u.compacted(model, update)
					calls[i] = msg.UsageRecord()
					if model != "" {
						calls[i].Metadata[messages.MetadataKeyUsageModel] = model
					}
					continue
				}
				u.project(llm.ProjectionStats{CountedTokens: 100})
				u.progress(update)
				if tc.failed && i == 0 {
					u.streamed("discarded speculative text must not inflate the bill")
					u.abortAttempt()
					calls[i] = msg.UsageRecord()
				} else {
					u.record(update.InputTokens, update.OutputTokens)
				}
			}
			if got := u.cost(); got != tc.want {
				t.Fatalf("live cost = %+v, want %+v", got, tc.want)
			}
			response := &llm.AgentResponse{AllMessages: calls, PersistedMessages: 1}
			for range 2 {
				u.settle(response)
				if got := u.cost(); got != tc.want || u.totalIn != 170 || u.totalOut != 17 {
					t.Fatalf("settled cost = %+v, want %+v; usage=%+v", got, tc.want, u)
				}
			}
		})
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
