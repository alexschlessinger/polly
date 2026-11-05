package main

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestTurnRetryResetsNewlineAndUsageState(t *testing.T) {
	var out, status bytes.Buffer
	ui := &lineTurnUI{config: &Config{}, interactive: true, stdoutTTY: true, writer: &out, errWriter: &status}
	execution := &turnExecution{turnUI: ui, config: ui.config, state: &conversationState{}}
	execution.usage.project(llm.ProjectionStats{CountedTokens: 100})
	cb := execution.callbacks()
	cb.OnModelRequest(0, 0)
	cb.OnReasoning("failed reasoning")
	cb.OnUsageProgress(llm.UsageUpdate{InputTokens: 90, OutputTokens: 7, CacheReadInputTokens: 20})
	cb.OnAttemptAborted(0, 0, errors.New("reset"))
	cb.OnModelRequest(0, 1)
	cb.OnContent("\n\naccepted")
	if out.String() != "\n\n\naccepted" { // attempt separator, then both provider newlines
		t.Fatalf("retry retained reasoning newline trim: %q", out.String())
	}
	u := execution.usage
	if u.liveIn != 100 || u.liveInReported || u.liveOutReported != 0 || u.liveCacheRead != 0 || u.totalIn != 90 || u.totalOut != 7 {
		t.Fatalf("retry mixed failed and live usage: %+v", u)
	}
}

func TestTUIRetrySeparatesPartialAndPreservesToolsAndNotices(t *testing.T) {
	m := newReplModel()
	m.busy = true
	ui := &gotuiTurnUI{model: m, config: &Config{ShotScript: "test"}}
	call := messages.ChatMessageToolCall{ID: "completed", Name: "read_file"}
	ui.AppendToolStart([]messages.ChatMessageToolCall{call})
	ui.AppendToolEnd(call, "contents", time.Second, nil)
	ui.ModelAttemptStarted(1, 0)
	ui.AppendAssistantText("failed partial")
	ui.AppendNotice("peer notice during stream")
	ui.AppendAssistantText("failed suffix")
	ui.ModelAttemptAborted(1, 0, errors.New("disconnect"))
	if m.outcomeLabeled {
		t.Fatal("attempt discard consumed the whole-turn outcome")
	}
	ui.ModelAttemptStarted(1, 1)
	ui.AppendAssistantText("accepted reply")
	ui.FinishTextTurn()
	var all strings.Builder
	for _, entry := range m.transcript {
		all.WriteString(entry.text)
		all.WriteString(entry.markdown)
		all.WriteByte('\n')
	}
	for _, want := range []string{"failed partial", "failed suffix", "peer notice during stream", "discarded attempt 1", "not saved", "accepted reply"} {
		if !strings.Contains(all.String(), want) {
			t.Fatalf("missing %q in transcript: %s", want, all.String())
		}
	}
	if got := m.toolRowCount(m.turnToolDisclosureIDs); got != 1 || m.runningTools != 0 {
		t.Fatalf("retry changed completed tools: rows=%d running=%d", got, m.runningTools)
	}
	m.labelTurnOutcome("failed · progress saved")
	if !m.outcomeLabeled {
		t.Fatal("final outcome could not be labeled")
	}
}

func TestTUIRetryRestoresAcceptedCoalescedReasoning(t *testing.T) {
	m := newReplModel()
	m.busy = true
	ui := &gotuiTurnUI{model: m, config: &Config{}}
	ui.ModelAttemptStarted(0, 0)
	ui.ShowThinking("accepted reasoning")
	m.pauseThinkingSegment()
	acceptedID := m.turnReasoningID
	ui.ModelAttemptStarted(1, 0)
	ui.ShowThinking("failed reasoning")
	if m.turnReasoningID != acceptedID {
		t.Fatal("successful rounds no longer coalesce reasoning")
	}
	ui.ModelAttemptAborted(1, 0, errors.New("disconnect"))
	accepted := m.reasoningRecords.get(acceptedID)
	if accepted.unsaved || string(accepted.tail) != "accepted reasoning" {
		t.Fatalf("accepted reasoning was discarded: %+v", accepted)
	}
	var failed *reasoningRecord
	for _, id := range m.turnReasoningIDs {
		if id != acceptedID {
			failed = m.reasoningRecords.get(id)
		}
	}
	if failed == nil || !failed.unsaved || string(failed.tail) != "failed reasoning" {
		t.Fatalf("failed reasoning not isolated: %+v", failed)
	}
	ui.ModelAttemptStarted(1, 1)
	ui.ShowThinking("recovered reasoning")
	if m.turnReasoningID == acceptedID || m.turnReasoningID == failed.id {
		t.Fatal("new attempt reused a settled reasoning record")
	}
	m.completeThinkingTurn(false)
	if accepted.unsaved || !failed.unsaved {
		t.Fatal("successful settlement lost attempt ownership")
	}
}

func TestTUIRetryPreservesFailedReasoningFenceContext(t *testing.T) {
	m := newReplModel()
	m.beginTurn("explain")
	ui := &gotuiTurnUI{model: m, config: &Config{}}
	ui.ShowThinking("**accepted** reasoning")
	ui.ThinkingEnded()
	accepted := m.currentReasoningRecord()
	ui.ModelAttemptStarted(1, 0)
	ui.ShowThinking("```text\n" + strings.Repeat("payload line\n", 700) + "```\n\n**Important** conclusion.")
	prefix := ui.attempt.shared.tailPrefix
	if prefix == "" || !ui.attempt.shared.tailCut {
		t.Fatal("failed reasoning fixture did not compact inside its fence")
	}
	ui.ModelAttemptAborted(1, 0, errors.New("disconnect"))
	m.refreshReasoningRecords(80)
	if accepted.unsaved || string(accepted.tail) != "**accepted** reasoning" {
		t.Fatal("retry discarded the accepted reasoning prefix")
	}
	failed := m.reasoningRecords.get(m.reasoningOrder[len(m.reasoningOrder)-1])
	if failed == accepted || !failed.unsaved || !failed.tailCut || failed.tailPrefix != prefix {
		t.Fatal("failed reasoning lost its separate unsaved record or compacted fence context")
	}
	got := plainStyledText(m.transcript[failed.transcriptIndex].text)
	if !strings.Contains(got, "not saved") || strings.Contains(got, "**Important**") || !strings.HasSuffix(got, "Important conclusion.") {
		t.Fatalf("failed reasoning fence swallowed the painted conclusion: %q", got)
	}
}

func TestLineRetryCommitsBoundaryOnceAndResetsDetails(t *testing.T) {
	var out, status bytes.Buffer
	ui := &lineTurnUI{config: &Config{}, interactive: true, stdoutTTY: true, writer: &out, errWriter: &status,
		activity: &lineActivity{details: &lineActivityDetails{}}}
	ui.ShowThinking("earlier accepted thought")
	ui.ModelAttemptStarted(1, 0)
	ui.ShowThinking("failed thought")
	ui.AppendAssistantText("failed partial")
	ui.ModelAttemptAborted(1, 0, errors.New("disconnect"))
	ui.ModelAttemptStarted(1, 1)
	ui.AppendAssistantText("accepted reply")
	if out.String() != "failed partial\n\naccepted reply" {
		t.Fatalf("line output was replayed or glued: %q", out.String())
	}
	if strings.Count(status.String(), "discarded attempt 1 · not saved") != 1 {
		t.Fatalf("missing/duplicate boundary: %q", status.String())
	}
	if got := string(ui.activity.details.thought.tail); got != "earlier accepted thought" {
		t.Fatalf("failed reasoning entered settled details: %q", got)
	}
}

func TestLineRetryResetsCommittedStreamOffsets(t *testing.T) {
	columns, height := 70, 12
	ui, out := lineStreamTestUI(t, false, true, &columns, &height)
	ui.ModelAttemptStarted(0, 0)
	ui.AppendAssistantText("failed paragraph\n\nunfinished **tail")
	paintLineStream(ui)
	ui.ModelAttemptAborted(0, 0, errors.New("disconnect"))
	if ui.stream.committed != 0 || ui.markdownBuffer.Len() != 0 || len(ui.stream.displayedImages) != 0 {
		t.Fatalf("stream retained failed attempt offsets: %+v", ui.stream)
	}
	ui.ModelAttemptStarted(0, 1)
	ui.AppendAssistantText("accepted paragraph")
	paintLineStream(ui)
	ui.FinishTextTurn()
	got := lineTestScreen(out.String())
	for _, want := range []string{"failed paragraph", "discarded attempt 1 · not saved", "accepted paragraph"} {
		if strings.Count(got, want) != 1 {
			t.Fatalf("missing or duplicated %q after retry: %q", want, got)
		}
	}
}

func TestRetryWaitStatusIsConcurrentAndClearsWithoutTranscript(t *testing.T) {
	m := newReplModel()
	m.busy = true
	ui := &gotuiTurnUI{model: m, config: &Config{}}
	now := time.Now()
	event := llm.RetryEvent{Err: errors.New("rate\nlimited\x1b[31m"), RetryCount: 2, Delay: time.Second, NextRetryAt: now.Add(time.Second), Waiting: true}
	ui.ModelRetry(event)
	label := m.retry.label(now)
	if !strings.Contains(label, "retry 2 in 1s") || !strings.Contains(label, "rate limited") || strings.ContainsAny(label, "\n\x1b") {
		t.Fatalf("unsafe or incomplete retry status: %q", label)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 100 {
			ui.ModelRetry(event)
		}
	}()
	go func() {
		defer wg.Done()
		for range 100 {
			ui.RecordTurnTokens(10, 2, false)
		}
	}()
	wg.Wait()
	event.Waiting = false
	ui.ModelRetry(event)
	if m.retry.waiting || m.retry.count != 0 || !m.retry.next.IsZero() || len(m.transcript) != 0 {
		t.Fatalf("retry status leaked after wait: %+v, transcript=%+v", m.retry, m.transcript)
	}
}
