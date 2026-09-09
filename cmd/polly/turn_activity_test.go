package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestTurnCompletionParity(t *testing.T) {
	for _, tc := range []struct {
		name       string
		completion turnCompletion
		want       turnOutcome
	}{
		{"done", turnCompletion{}, turnOutcomeDone},
		{"tokens", turnCompletion{Reason: messages.StopReasonMaxTokens}, turnOutcomeIncomplete},
		{"iterations", turnCompletion{Err: llm.ErrMaxIterations}, turnOutcomeIncomplete},
		{"saved iterations", turnCompletion{Err: &turnProgressSavedError{cause: llm.ErrMaxIterations}, ProgressSaved: true}, turnOutcomeIncomplete},
		{"cancel", turnCompletion{Err: context.Canceled}, turnOutcomeCanceled},
		{"cancel wins", turnCompletion{Reason: messages.StopReasonMaxTokens, Err: errors.Join(context.Canceled, io.ErrClosedPipe)}, turnOutcomeCanceled},
		{"save failure", turnCompletion{Reason: messages.StopReasonMaxTokens, Err: errors.New("save failed")}, turnOutcomeFailed},
		{"iteration and save failure", turnCompletion{Err: errors.Join(llm.ErrMaxIterations, errors.New("save failed"))}, turnOutcomeFailed},
		{"output failure", turnCompletion{Reason: messages.StopReasonMaxTokens, Err: io.ErrClosedPipe}, turnOutcomeFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line, _, _ := activityTestUI(t, false, &Config{})
			r := newManagedREPL(&Config{}, "ctx", 0, 0)
			r.model.beginTurn("question")
			r.model.turnID = 9
			tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config, turnID: r.model.turnID}
			completion := tc.completion
			completion.Elapsed = 7 * time.Second
			for _, ui := range []TurnUI{line, tui} {
				ui.AppendAssistantText("partial answer")
				ui.CompleteTurn(completion)
				ui.CompleteTurn(turnCompletion{Err: errors.New("late failure"), Elapsed: time.Hour})
			}
			r.endTurn(completion.Err)
			if line.activity.outcome != tc.want || r.model.lastOutcome != tc.want {
				t.Fatalf("line=%v TUI=%v want=%v", line.activity.outcome, r.model.lastOutcome, tc.want)
			}
			if line.activity.elapsed != 7*time.Second || r.model.lastElapsed != 7*time.Second {
				t.Fatal("elapsed changed after completion")
			}
			r.model.beginTurn("new question")
			tui.CompleteTurn(turnCompletion{Err: errors.New("stale generation")})
			if r.model.completion != nil {
				t.Fatal("late completion entered a newer turn")
			}
		})
	}
}

func TestHydratedCapsKeepIncompleteOutcome(t *testing.T) {
	for _, reason := range []messages.StopReason{messages.StopReasonMaxTokens, messages.StopReasonMaxIterations} {
		m := newManagedREPL(&Config{}, "ctx", 0, 0).model
		history := []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "question"}, {Role: messages.MessageRoleAssistant, Content: "partial", StopReason: reason}}
		if reason == messages.StopReasonMaxIterations {
			history[1].StopReason = messages.StopReasonToolUse
			history = append(history, interruptedTurnMarker(llm.ErrMaxIterations))
		}
		m.hydrateHistory(history, "ctx")
		if got := plainStyledText(strings.Join(m.flattenTranscript(), "\n")); !strings.Contains(got, "incomplete") || strings.Contains(got, "✓") {
			t.Fatalf("restored cap was called done: %q", got)
		}
	}
}

type failingTurnWriter struct{}

func (failingTurnWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestOutputFailureOverridesTokenCapAfterFlush(t *testing.T) {
	for _, rich := range []bool{false, true} {
		t.Run(fmt.Sprint(rich), func(t *testing.T) {
			message := messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "answer", StopReason: messages.StopReasonMaxTokens}
			state, _ := newInterruptedTurnState(t, &scriptedStreamLLM{responses: []messages.ChatMessage{message}}, nil)
			state.settings.SystemPrompt = "fixture"
			config := &Config{}
			ui := newLineTurnUI(config, nil)
			var status bytes.Buffer
			ui.writer, ui.errWriter = failingTurnWriter{}, &status
			if rich {
				ui.capabilities.surface = outputSurfaceLineANSI
			}
			code, err := executeTurnWithUserMessage(context.Background(), config, state, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "hi"}, nil, nil, ui, false)
			if code != 1 || !errors.Is(err, io.ErrClosedPipe) || ui.activity.outcome != turnOutcomeFailed {
				t.Fatalf("write failure classified as cap: code=%d err=%v outcome=%v", code, err, ui.activity.outcome)
			}
			if strings.Count(status.String(), "✗ failed") != 1 {
				t.Fatalf("trailer missing or duplicated: %s", status.String())
			}
		})
	}
}

func TestStderrWriteFailureKeepsDeliveredAnswerExitZero(t *testing.T) {
	for _, rich := range []bool{false, true} {
		t.Run(fmt.Sprint(rich), func(t *testing.T) {
			message := messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "answer", StopReason: messages.StopReasonEndTurn}
			state, _ := newInterruptedTurnState(t, &scriptedStreamLLM{responses: []messages.ChatMessage{message}}, nil)
			state.settings.SystemPrompt = "fixture"
			config := &Config{}
			ui := newLineTurnUI(config, nil)
			var answer bytes.Buffer
			ui.writer, ui.errWriter = &answer, failingTurnWriter{}
			if rich {
				ui.capabilities.surface = outputSurfaceLineANSI
			}
			code, err := executeTurnWithUserMessage(context.Background(), config, state, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "hi"}, nil, nil, ui, false)
			if code != 0 || err != nil || ui.activity.outcome != turnOutcomeDone {
				t.Fatalf("stderr chrome failure changed the outcome: code=%d err=%v outcome=%v", code, err, ui.activity.outcome)
			}
			if !strings.Contains(answer.String(), "answer") {
				t.Fatalf("answer not delivered: %q", answer.String())
			}
		})
	}
}

func TestActivityRawAnswerBytesAcrossDetailAndQuietModes(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		for _, config := range []*Config{{}, {ActivityDetails: true}, {Quiet: true}, {Quiet: true, ActivityDetails: true}} {
			line, out, _ := activityTestUI(t, true, config)
			line.ShowThinking("preview")
			line.AppendAssistantText("before")
			line.AppendWarning("notice")
			line.AppendAssistantText(" continued")
			call := messages.ChatMessageToolCall{ID: "read", Name: "read_file", Arguments: `{"path":"a.go"}`}
			line.AppendToolStart([]messages.ChatMessageToolCall{call})
			line.AppendToolEnd(call, "result", time.Second, nil)
			line.AppendToolMedia(call, []transcriptImage{{Alt: "receipt", Inspection: true}})
			line.AppendAssistantText("after")
			completion := turnCompletion{}
			want := "before continued\nafter\n"
			if interrupted {
				completion.Err = context.Canceled
				want = strings.TrimSuffix(want, "\n")
			} else {
				line.FinishTextTurn()
			}
			line.CompleteTurn(completion)
			line.Stop()
			if out.String() != want {
				t.Fatalf("quiet=%v details=%v interrupted=%v stdout=%q want=%q", config.Quiet, config.ActivityDetails, interrupted, out.String(), want)
			}
		}
	}
}

func TestTurnActivityScopeParity(t *testing.T) {
	withDisplayTTY(t)
	line, _, _ := activityTestUI(t, false, &Config{})
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.model.beginTurn("question")
	tui := &gotuiTurnUI{repl: r, model: r.model, config: r.config}
	read := messages.ChatMessageToolCall{ID: "shared", Name: "read_file", Arguments: `{"path":"a.go"}`}
	spawn := messages.ChatMessageToolCall{ID: "agent", Name: "spawn_agent", Arguments: `{"label":"review"}`}
	for _, ui := range []TurnUI{line, tui} {
		ui.ShowThinking("reasoning")
		ui.AppendToolStart([]messages.ChatMessageToolCall{read, spawn})
		ui.AppendToolEnd(read, "one\ntwo", time.Second, nil)
		ui.AppendToolMedia(read, []transcriptImage{{Alt: "parent", Inspection: true}})
	}
	child := &childTurnUI{parent: line, activity: line.childActivity(spawn)}
	child.ShowThinking("private")
	child.AppendToolStart([]messages.ChatMessageToolCall{read})
	child.AppendToolMedia(read, []transcriptImage{{Alt: "child", Inspection: true}})
	child.RecordTurnTokens(900, 300)
	child.RecordTurnTokens(900, 300)
	child.AppendToolEnd(read, "child result", time.Second, nil)
	child.CompleteTurn(turnCompletion{Err: context.Canceled})
	for _, ui := range []TurnUI{line, tui} {
		ui.AppendToolEnd(spawn, "", time.Second, context.Canceled)
		ui.RecordTurnTokens(100, 30)
		ui.CompleteTurn(turnCompletion{Elapsed: time.Second})
	}
	r.endTurn(nil)
	var dock turnDockState
	for _, trailer := range r.model.turnTrailers.all() {
		dock = trailer.dock
	}
	a, b := line.activity.summary(), r.model.activitySummaryFor(dock)
	if a.Tools != b.Tools || a.Images != b.Images || a.Agents != b.Agents || a.In != b.In || a.Out != b.Out || a.Reasoned != b.Reasoned {
		t.Fatalf("parent summaries differ: line=%+v TUI=%+v", a, b)
	}
	if a.Tools != 1 || a.Images != 1 || a.Agents.Canceled != 1 || a.In != 100 || a.Out != 30 {
		t.Fatalf("wrong parent accounting: %+v", a)
	}
	if launch := line.activity.launches[0]; launch.in != 900 || launch.out != 300 || launch.tools != 1 || launch.images != 1 {
		t.Fatalf("wrong attributed work: %+v", launch)
	}
}

func TestTurnUsageReplacesIterationsAndKeepsLatestProjection(t *testing.T) {
	u := turnUsage{}
	u.project(0, llm.ProjectionStats{RequestEstimatedTokens: 900}, 2000)
	u.record(0, 1000, 30)
	u.project(1, llm.ProjectionStats{RequestEstimatedTokens: 400}, 2000)
	if in, out := u.record(0, 1100, 40); in != 1100 || out != 40 || !u.estimated || u.used != 400 {
		t.Fatalf("old replacement changed new request: %+v %d/%d", u, in, out)
	}
	if in, out := u.record(1, 0, 0); in != 1100 || out != 40 || !u.estimated {
		t.Fatalf("missing usage changed totals/estimate: %+v %d/%d", u, in, out)
	}
	if in, out := u.record(1, 450, 10); in != 1100 || out != 50 || u.estimated || u.used != 450 {
		t.Fatalf("measured usage wrong: %+v %d/%d", u, in, out)
	}
	if _, out := u.record(1, 450, 10); out != 50 {
		t.Fatal("reconciliation doubled usage")
	}
	u.project(2, llm.ProjectionStats{RequestEstimatedTokens: 200}, 2000)
	if u.used != 200 || !u.estimated {
		t.Fatal("shrinking projection was lost")
	}
}

func TestActivityDetailsAreBoundedAndEmittedOnce(t *testing.T) {
	line, out, status := activityTestUI(t, false, &Config{ActivityDetails: true})
	line.ShowThinking(strings.Repeat("old\n", 20000) + "one\ntwo\nthree\nfour\nfive")
	if len(line.activity.details.thought.tail) > reasoningTailLimitRunes {
		t.Fatal("unbounded thought storage")
	}
	for i := range 20 {
		call := messages.ChatMessageToolCall{ID: string(rune('a' + i)), Name: "read_file", Arguments: `{"path":"safe.go","password":"secret"}`}
		line.AppendToolStart([]messages.ChatMessageToolCall{call})
		line.AppendToolEnd(call, "private result\nsecond", time.Second, nil)
	}
	line.AppendAssistantText("partial")
	line.CompleteTurn(turnCompletion{Err: context.Canceled, Elapsed: time.Second})
	line.Stop()
	got := status.String()
	if out.String() != "\npartial" {
		t.Fatalf("details changed interrupted stdout: %q", out.String())
	}
	if strings.Count(got, "  Thought\n") != 1 || strings.Count(got, "  Tools\n") != 1 || strings.Count(got, "safe.go") != 5 || !strings.Contains(got, "… 15 earlier") {
		t.Fatalf("unbounded/repeated details: %s", got)
	}
	if strings.Contains(got, "private result") || strings.Contains(got, "secret") {
		t.Fatal("details exposed raw result or secret arguments")
	}
	if strings.Index(got, "  Tools\n") > strings.Index(got, " · canceled · ") {
		t.Fatal("details followed trailer")
	}
}
