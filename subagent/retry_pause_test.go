package subagent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestToolRetryPauseRequiresConfirmedRecoveryAndKeepsEvidence(t *testing.T) {
	t.Parallel()
	exhausted := &llm.RetryExhaustedError{Err: errors.New("overloaded"), Attempts: 3, RetryCount: 2}
	for _, tc := range []struct {
		name string
		err  error
		code string
	}{
		{"confirmed", errors.Join(ErrRetryPaused, exhausted), "RETRY_EXHAUSTED"},
		{"unsaved", errors.Join(exhausted, errors.New("checkpoint failed")), "AGENT_FAILED"},
		{"raw provider", exhausted, "AGENT_FAILED"},
		{"canceled", errors.Join(ErrRetryPaused, exhausted, context.Canceled), "AGENT_FAILED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool := NewTool(func(context.Context, Request) (Result, error) {
				return Result{Session: "saved-worker", Text: "accepted evidence"}, tc.err
			})
			text, err := tool.Execute(context.Background(), map[string]any{"label": "Worker", "task": "inspect"})
			var toolErr *tools.ToolError
			if !errors.As(err, &toolErr) || toolErr.Code != tc.code {
				t.Fatalf("code=%v want %s", err, tc.code)
			}
			if tc.code == "RETRY_EXHAUSTED" && (!strings.Contains(text, "accepted evidence") || !strings.Contains(text, "saved-worker")) {
				t.Fatalf("pause lost evidence/session: %q", text)
			}
		})
	}
}
