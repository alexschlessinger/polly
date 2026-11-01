package main

import (
	"bytes"
	"context"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
)

type turnRetryStream func() []*messages.StreamEvent

func (f turnRetryStream) ChatCompletionStream(context.Context, *llm.CompletionRequest, llm.EventStreamProcessor) <-chan *messages.StreamEvent {
	events := f()
	ch := make(chan *messages.StreamEvent, len(events))
	for _, event := range events {
		ch <- event
	}
	close(ch)
	return ch
}

func TestTurnRetryCallbacksDoNotReplayRawOrCustomOutput(t *testing.T) {
	for _, name := range []string{"raw stream", "custom UI", "human terminal", "settled output"} {
		t.Run(name, func(t *testing.T) {
			var out, status bytes.Buffer
			config := &Config{Stream: true}
			line := &lineTurnUI{config: config, writer: &out, errWriter: &status}
			var ui TurnUI = line
			wantCalls := 1
			switch name {
			case "custom UI":
				ui = &collectingTurnUI{}
			case "human terminal":
				line.interactive, line.stdoutTTY = true, true
				wantCalls = 2
			case "settled output":
				line.settledOutput = true
				wantCalls = 2
			}
			calls := 0
			model := turnRetryStream(func() []*messages.StreamEvent {
				calls++
				if calls == 1 {
					return []*messages.StreamEvent{
						{Type: messages.EventTypeContent, Content: "discarded partial"},
						{Type: messages.EventTypeUsage, InputTokens: 80, OutputTokens: 7, CostUSD: 3, CostReported: true},
						{Type: messages.EventTypeError, Error: io.ErrUnexpectedEOF},
					}
				}
				answer := messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "accepted", StopReason: messages.StopReasonEndTurn}
				answer.SetTokenUsage(90, 10)
				answer.SetReportedCost(4)
				return []*messages.StreamEvent{
					{Type: messages.EventTypeContent, Content: "accepted"},
					{Type: messages.EventTypeUsage, InputTokens: 90, OutputTokens: 10, CostUSD: 4, CostReported: true},
					{Type: messages.EventTypeComplete, Message: &answer},
				}
			})
			agent := llm.NewAgent(model, nil, llm.AgentConfig{})
			defer agent.Close()
			execution := &turnExecution{turnUI: ui, config: config, state: &conversationState{}, settledOutput: line.settledOutput}
			cb := execution.callbacks()
			cb.BeforeFirstRequest = nil
			cb.OnRequestProjection = func(_ int, stats llm.ProjectionStats) { execution.usage.project(stats) }
			response, err := agent.Run(context.Background(), &llm.CompletionRequest{RetryPolicy: &llm.RetryPolicy{MaxRetries: 1, MaxElapsed: time.Second, InitialDelay: time.Microsecond, MaxDelay: time.Microsecond}}, cb)
			if calls != wantCalls || (err == nil) != (wantCalls == 2) {
				t.Fatalf("calls=%d err=%v want calls=%d", calls, err, wantCalls)
			}
			if name == "raw stream" && out.String() != "discarded partial" {
				t.Fatalf("raw stream was rewritten/replayed: %q", out.String())
			}
			if name == "custom UI" && ui.(*collectingTurnUI).result() != "discarded partial" {
				t.Fatalf("custom output was replayed: %q", ui.(*collectingTurnUI).result())
			}
			if name == "settled output" && out.Len() != 0 {
				t.Fatalf("unsettled attempt reached buffered stdout: %q", out.String())
			}
			if wantCalls == 2 {
				if execution.usage.totalIn != 170 || execution.usage.totalOut != 17 || execution.usage.cost().usd != 7 {
					t.Fatalf("live failed usage lost or doubled: %+v", execution.usage)
				}
				execution.recordUsage(response)
				if execution.usage.totalIn != 170 || execution.usage.totalOut != 17 || execution.usage.cost().usd != 7 {
					t.Fatalf("settled failed usage lost or doubled: %+v", execution.usage)
				}
				for _, message := range response.AllMessages {
					if strings.Contains(message.Content, "discarded") || message.IsUsageRecord() && len(message.ToolCalls) != 0 {
						t.Fatalf("failed text/actions entered accepted transcript: %+v", message)
					}
				}
			}
		})
	}
}

func TestTUIRetryStaleObserversAndStreamOwnership(t *testing.T) {
	m := newReplModel()
	m.busy, m.turnID = true, 3
	ui := &gotuiTurnUI{model: m, config: &Config{}, turnID: 3}
	ui.ModelAttemptStarted(0, 0)
	ui.AppendAssistantText("failed **markdown")
	m.renderPendingMarkdown()
	failedIndex := m.currentAssistant
	ui.ModelAttemptAborted(0, 0, io.ErrUnexpectedEOF)
	if m.currentAssistant != -1 || m.streamRaw.Len() != 0 || m.streamCodeCache != nil || m.streamShown != 0 || !reflect.DeepEqual(m.streamTypewriter, assistantTypewriter{}) {
		t.Fatal("aborted stream retained typewriter, markdown or cache ownership")
	}
	ui.ModelAttemptStarted(0, 1)
	ui.AppendAssistantText("accepted")
	if m.currentAssistant == failedIndex || len(m.transcript[m.currentAssistant].images) != 0 || m.streamRaw.String() != "accepted" {
		t.Fatal("replacement stream reused failed entry/image ownership")
	}
	m.canceling = true
	ui.ModelRetry(llm.RetryEvent{Waiting: true, RetryCount: 1, NextRetryAt: time.Now().Add(time.Minute)})
	if m.busyLabel() != "canceling" {
		t.Fatalf("retry concealed cancellation: %q", m.busyLabel())
	}
	m.canceling, m.turnID = false, 4
	m.retry = modelRetryStatus{}
	count, raw := len(m.transcript), m.streamRaw.String()
	ui.ModelAttemptAborted(0, 1, io.ErrUnexpectedEOF)
	ui.ModelRetry(llm.RetryEvent{Waiting: true, RetryCount: 2, NextRetryAt: time.Now()})
	ui.ModelAttemptStarted(0, 2)
	if len(m.transcript) != count || m.streamRaw.String() != raw || m.retry.waiting {
		t.Fatal("stale observers modified the new turn")
	}
}
