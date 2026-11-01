package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/swarm"
)

func savedRetryPauseForTest() error {
	return &swarm.RetryPauseError{Session: "test-session", Pause: swarm.RetryPause{Reason: "service unavailable", Attempts: 2, RetryCount: 1}, Err: &llm.RetryExhaustedError{Err: io.ErrUnexpectedEOF, Attempts: 2, RetryCount: 1}}
}

func TestRetryPauseRequiresConfirmedSaveAndNoLaterFailure(t *testing.T) {
	pause := savedRetryPauseForTest()
	for _, tc := range []struct {
		name string
		err  error
		want turnOutcome
	}{
		{"confirmed", pause, turnOutcomePaused},
		{"wrapped", &turnProgressSavedError{cause: pause}, turnOutcomePaused},
		{"unsaved exhaustion", &llm.RetryExhaustedError{Err: io.ErrUnexpectedEOF}, turnOutcomeFailed},
		{"later save failure", errors.Join(pause, errors.New("save failed")), turnOutcomeFailed},
		{"later output failure", errors.Join(pause, io.ErrClosedPipe), turnOutcomeFailed},
		{"cancel wins", errors.Join(pause, context.Canceled), turnOutcomeCanceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := (turnCompletion{Err: tc.err}).outcome(); got != tc.want {
				t.Fatalf("outcome=%v want %v for %v", got, tc.want, tc.err)
			}
			marker := interruptedTurnMarker(tc.err)
			paused, _ := marker.Metadata[retryPauseMetadataKey].(bool)
			if paused != (tc.want == turnOutcomePaused) {
				t.Fatalf("marker claims wrong durability: %+v", marker)
			}
		})
	}
}

func TestRetryPauseLiveLineAndTUIAgreeWithHydration(t *testing.T) {
	pause := savedRetryPauseForTest()
	line, _, status := activityTestUI(t, true, &Config{})
	line.interactive = true
	line.CompleteTurn(turnCompletion{Err: pause, ProgressSaved: true, Elapsed: time.Second})
	if line.activity.outcome != turnOutcomePaused {
		t.Fatalf("line outcome=%v", line.activity.outcome)
	}
	if got := plainStyledText(status.String()); !strings.Contains(got, retryPausedLabel) || strings.Contains(got, "failed") || strings.Contains(got, "incomplete") {
		t.Fatalf("line contradicts pause: %q", got)
	}
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.model.beginTurn("question")
	r.model.completion = &turnCompletion{Err: pause, ProgressSaved: true, Elapsed: time.Second}
	r.endTurn(pause)
	if r.model.lastOutcome != turnOutcomePaused || r.model.ed.text() != "" || r.model.restoreDraftNext {
		t.Fatalf("pause restored unsent draft: outcome=%v draft=%q restore=%v", r.model.lastOutcome, r.model.ed.text(), r.model.restoreDraftNext)
	}
	live := plainStyledText(strings.Join(r.model.flattenTranscript(), "\n"))
	if !strings.Contains(live, retryPausedLabel) || strings.Contains(live, "failed") || strings.Contains(live, "incomplete") {
		t.Fatalf("TUI contradicts pause: %q", live)
	}
	m := newReplModel()
	m.hydrateHistory([]messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "question"}, interruptedTurnMarker(pause)}, "ctx")
	hydrated := plainStyledText(strings.Join(m.flattenTranscript(), "\n"))
	if !strings.Contains(hydrated, retryPausedLabel) || strings.Contains(hydrated, "✓") || m.ed.text() != "" || m.restoreDraftNext {
		t.Fatalf("hydration contradicts pause: %q draft=%q restore=%v", hydrated, m.ed.text(), m.restoreDraftNext)
	}
	if text := settledAnswer(nil, pause, "test-session"); !strings.HasPrefix(text, "Paused:") || !strings.Contains(text, "test-session") {
		t.Fatalf("settled line answer contradicts pause: %q", text)
	}
}

func TestRetryPauseSavesMarkerWithoutGeneratedMessages(t *testing.T) {
	state, session := newInterruptedTurnState(t, &scriptedStreamLLM{}, nil)
	ctx := context.Background()
	if err := session.AddMessage(ctx, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "question"}); err != nil {
		t.Fatal(err)
	}
	execution := &turnExecution{ctx: ctx, state: state, turnUI: &collectingTurnUI{}, persistAttempted: true}
	err := execution.settlePersistence(&llm.AgentResponse{}, savedRetryPauseForTest())
	if !retryPauseError(err) || !turnProgressSaved(err) {
		t.Fatalf("marker-only pause not saved: %v", err)
	}
	history := testSessionHistory(t, session)
	if len(history) != 2 || history[1].Metadata[retryPauseMetadataKey] != true {
		t.Fatalf("missing marker-only pause: %+v", history)
	}
	// A raw retry exhaustion is not proof a recovery checkpoint committed.
	before := len(history)
	_ = execution.settlePersistence(&llm.AgentResponse{}, fmt.Errorf("unsaved: %w", &llm.RetryExhaustedError{Err: io.ErrUnexpectedEOF}))
	if got := len(testSessionHistory(t, session)); got != before {
		t.Fatalf("unconfirmed pause wrote marker: count=%d want=%d", got, before)
	}
}
