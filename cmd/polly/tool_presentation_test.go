package main

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

func shellExitError(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command("sh", "-c", "exit "+itoa(code)).Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != code {
		t.Fatalf("exit %d command returned %v", code, err)
	}
	return err
}

func itoa(n int) string {
	return string(rune('0' + n))
}

func toolResult(content string, meta map[string]any) messages.ChatMessage {
	return messages.ChatMessage{Role: messages.MessageRoleTool, Content: content, Metadata: meta}
}

// live is a callback's input: the result text plus the error the registry
// returned, before the durable message exists.
func live(call messages.ChatMessageToolCall, content string, err error, duration time.Duration) toolPresentationInput {
	return toolPresentationInput{call: call, result: liveToolResult(call, content, err), err: err, duration: duration, complete: true}
}

func TestNewToolPresentation(t *testing.T) {
	bash := messages.ChatMessageToolCall{ID: "c1", Name: "bash"}
	edit := messages.ChatMessageToolCall{ID: "c2", Name: "edit_file"}
	tracked := map[string]any{messages.MetadataKeyToolSucceeded: true, "tool_data": map[string]any{"tracked": true, "changes": []any{
		map[string]any{"path": "main.go", "additions": float64(3), "deletions": float64(1), "diff": "+x\n-y"},
	}}}
	untracked := map[string]any{messages.MetadataKeyToolSucceeded: true, "tool_data": map[string]any{"exitCode": float64(0), "changes": map[string]any{"tracked": false, "reason": "no git"}}}
	tests := []struct {
		name    string
		in      toolPresentationInput
		want    toolPresentation
		meta    string
		detail  string
		elapsed string
	}{
		{
			name:    "ok with output",
			in:      live(bash, "a\nb\nc\n", nil, 1200*time.Millisecond),
			want:    toolPresentation{outcome: toolOutcomeOK, lines: "3 lines", duration: 1200 * time.Millisecond, hasText: true},
			meta:    "3 lines",
			detail:  "3 lines",
			elapsed: "1.2s",
		},
		{
			name:   "exit code error",
			in:     live(bash, "boom", shellExitError(t, 2), 0),
			want:   toolPresentation{outcome: toolOutcomeFailed, failure: "exit 2", lines: "1 line", hasText: true},
			meta:   "exit 2",
			detail: "failed · exit 2 · 1 line",
		},
		{
			name:   "plain error",
			in:     live(edit, "", errors.New("nope"), 0),
			want:   toolPresentation{outcome: toolOutcomeFailed},
			meta:   "failed",
			detail: "failed",
		},
		{
			name:   "iteration limit",
			in:     live(bash, "", &tools.ToolError{Code: "ITERATION_LIMIT"}, 0),
			want:   toolPresentation{outcome: toolOutcomePaused},
			meta:   "paused · iteration limit",
			detail: "paused · iteration limit",
		},
		{
			name:   "canceled",
			in:     live(bash, "", context.Canceled, 0),
			want:   toolPresentation{outcome: toolOutcomeCanceled},
			meta:   "canceled",
			detail: "canceled",
		},
		{
			name:   "denied drops its duration",
			in:     live(bash, llm.ToolDeniedContent, nil, time.Second),
			want:   toolPresentation{outcome: toolOutcomeDenied, lines: "1 line", duration: time.Second, hasText: true},
			meta:   "denied",
			detail: "denied · 1 line",
		},
		{
			name: "running",
			in:   toolPresentationInput{call: bash},
			want: toolPresentation{outcome: toolOutcomeRunning},
		},
		{
			name: "legacy result without an outcome stays neutral",
			in:   toolPresentationInput{call: bash, result: toolResult("a\nb", nil), complete: true},
			want: toolPresentation{outcome: toolOutcomeUnknown, lines: "2 lines", hasText: true},
		},
		{
			name:    "tracked changes on success",
			in:      toolPresentationInput{call: edit, result: toolResult("ok", tracked), duration: time.Second, complete: true},
			want:    toolPresentation{outcome: toolOutcomeOK, lines: "1 line", duration: time.Second, counts: "+3 −1", hasText: true},
			meta:    "1 line",
			detail:  "1 line",
			elapsed: "1.0s",
		},
		{
			name:   "tracked changes on failure carry no counts",
			in:     toolPresentationInput{call: edit, result: toolResult("", tracked), err: errors.New("nope"), complete: true},
			want:   toolPresentation{outcome: toolOutcomeFailed},
			meta:   "failed",
			detail: "failed",
		},
		{
			name: "untracked command",
			in:   toolPresentationInput{call: bash, result: toolResult("", untracked), complete: true},
			want: toolPresentation{outcome: toolOutcomeOK, untracked: true, untrackedReason: "no git"},
		},
		{
			name: "untracked edit is not a command notice",
			in:   toolPresentationInput{call: edit, result: toolResult("", map[string]any{messages.MetadataKeyToolSucceeded: true, "tool_data": map[string]any{"tracked": false}}), complete: true},
			want: toolPresentation{outcome: toolOutcomeOK},
		},
		{
			name: "hydrated failure from metadata",
			in: toolPresentationInput{call: bash, result: toolResult("err\n", map[string]any{
				messages.MetadataKeyToolSucceeded: false,
				messages.MetadataKeyToolMillis:    1200,
				"tool_data":                       map[string]any{"exitCode": float64(1)},
			}), complete: true},
			want:    toolPresentation{outcome: toolOutcomeFailed, failure: "exit 1", lines: "1 line", duration: 1200 * time.Millisecond, hasText: true},
			meta:    "exit 1",
			detail:  "failed · exit 1 · 1 line",
			elapsed: "1.2s",
		},
		{
			name:    "hydrated success from metadata",
			in:      toolPresentationInput{call: bash, result: toolResult("a\nb", map[string]any{messages.MetadataKeyToolSucceeded: true, messages.MetadataKeyToolMillis: 500}), complete: true},
			want:    toolPresentation{outcome: toolOutcomeOK, lines: "2 lines", duration: 500 * time.Millisecond, hasText: true},
			meta:    "2 lines",
			detail:  "2 lines",
			elapsed: "0.5s",
		},
		{
			name:   "hydrated stream error",
			in:     toolPresentationInput{call: bash, result: toolResult("", map[string]any{messages.MetadataKeyIsError: true}), complete: true},
			want:   toolPresentation{outcome: toolOutcomeFailed},
			meta:   "failed",
			detail: "failed",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := newToolPresentation(tc.in)
			if tc.want.counts != "" && got.changes == nil {
				t.Fatalf("changes dropped: %+v", got)
			}
			got.changes = nil
			if got != tc.want {
				t.Fatalf("presentation = %+v, want %+v", got, tc.want)
			}
			if got.meta() != tc.meta || got.detail() != tc.detail || got.elapsed() != tc.elapsed {
				t.Fatalf("meta %q detail %q elapsed %q, want %q %q %q", got.meta(), got.detail(), got.elapsed(), tc.meta, tc.detail, tc.elapsed)
			}
		})
	}
}

func TestToolPresentationLiveMatchesHydrated(t *testing.T) {
	call := messages.ChatMessageToolCall{ID: "c1", Name: "bash"}
	live := newToolPresentation(live(call, "out\n", shellExitError(t, 1), 1200*time.Millisecond))
	hydrated := newToolPresentation(toolPresentationInput{call: call, result: toolResult("out\n", map[string]any{
		messages.MetadataKeyToolSucceeded: false,
		messages.MetadataKeyToolMillis:    1200,
		"tool_data":                       map[string]any{"exitCode": float64(1)},
	}), complete: true})
	if live != hydrated {
		t.Fatalf("live %+v differs from hydrated %+v", live, hydrated)
	}
	if live.inline() != hydrated.inline() || live.inline().meta != "exit 1" || live.inline().duration != "1.2s" {
		t.Fatalf("inline %+v vs %+v", live.inline(), hydrated.inline())
	}
}

func TestToolPresentationInlineGlyphs(t *testing.T) {
	for outcome, want := range map[toolOutcome][2]string{
		toolOutcomeOK: {"✓", "ok"}, toolOutcomeFailed: {"✗", "err"}, toolOutcomeDenied: {"✗", "err"},
		toolOutcomeCanceled: {"✗", "err"}, toolOutcomeRunning: {"→", "run"}, toolOutcomeUnknown: {"·", "muted"},
	} {
		line := toolPresentation{outcome: outcome}.inline()
		if line.glyph != want[0] || line.tone != want[1] || line.modifier != "bold" {
			t.Fatalf("%q inline = %+v", outcome, line)
		}
	}
	if got := untrackedCommandNotice("no git"); got != "Command edits are not tracked here: no git" {
		t.Fatalf("notice = %q", got)
	}
}
