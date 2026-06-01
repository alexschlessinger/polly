package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
)

func resp(sr messages.StopReason) *llm.AgentResponse {
	return &llm.AgentResponse{Message: &messages.ChatMessage{StopReason: sr}}
}

func TestClassifyOutcome(t *testing.T) {
	cases := []struct {
		name     string
		resp     *llm.AgentResponse
		err      error
		wantStop messages.StopReason
		wantCode int
	}{
		{"end_turn", resp(messages.StopReasonEndTurn), nil, messages.StopReasonEndTurn, 0},
		{"max_tokens", resp(messages.StopReasonMaxTokens), nil, messages.StopReasonMaxTokens, 2},
		{"max_iterations", resp(messages.StopReasonMaxIterations), llm.ErrMaxIterations, messages.StopReasonMaxIterations, 3},
		{"hard_error", nil, errors.New("boom"), messages.StopReasonError, 1},
		{"nil_message", &llm.AgentResponse{}, nil, messages.StopReasonEndTurn, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotStop, gotCode := classifyOutcome(c.resp, c.err)
			if gotStop != c.wantStop || gotCode != c.wantCode {
				t.Fatalf("classifyOutcome = (%q, %d), want (%q, %d)", gotStop, gotCode, c.wantStop, c.wantCode)
			}
		})
	}
}

func TestExitError(t *testing.T) {
	inner := errors.New("api failed")
	e := &exitError{code: 1, err: inner}
	if !errors.Is(e, inner) {
		t.Fatal("exitError should unwrap to its inner error")
	}
	if e.Error() != "api failed" {
		t.Fatalf("Error() = %q, want %q", e.Error(), "api failed")
	}
	e2 := &exitError{code: 2}
	if e2.Error() == "" {
		t.Fatal("exitError with no inner error should still render a message")
	}
}

func TestBuildMeta(t *testing.T) {
	r := &llm.AgentResponse{
		Message:        &messages.ChatMessage{StopReason: messages.StopReasonEndTurn},
		IterationCount: 3,
	}
	m := buildMeta(r, nil, "deepseek/deepseek-v4-flash", 5, 1, 1200, 600, 8123)
	if m.StopReason != messages.StopReasonEndTurn || m.Iterations != 3 ||
		m.ToolCalls != 5 || m.ToolErrors != 1 || m.InputTokens != 1200 ||
		m.OutputTokens != 600 || m.DurationMS != 8123 || m.Model != "deepseek/deepseek-v4-flash" {
		t.Fatalf("buildMeta produced %+v", m)
	}
	if m.Err != "" {
		t.Fatalf("Err should be empty on success, got %q", m.Err)
	}
}

func TestBuildMetaHardError(t *testing.T) {
	m := buildMeta(nil, errors.New("api\nfailed"), "m", 0, 0, 0, 0, 5)
	if m.StopReason != messages.StopReasonError {
		t.Fatalf("StopReason = %q, want error", m.StopReason)
	}
	if strings.Contains(m.Err, "\n") {
		t.Fatalf("Err must be single-line, got %q", m.Err)
	}
	if m.Err == "" {
		t.Fatal("Err should be populated on hard error")
	}
}

func TestWriteMetaOut(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "meta.txt")
	m := metaFields{
		StopReason: messages.StopReasonMaxTokens, Model: "m", Iterations: 2,
		ToolCalls: 4, ToolErrors: 0, InputTokens: 100, OutputTokens: 50, DurationMS: 99,
	}
	if err := writeMetaOut(path, m); err != nil {
		t.Fatalf("writeMetaOut error = %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temp file should not remain after rename")
	}
	body, _ := os.ReadFile(path)
	got := string(body)
	for _, want := range []string{
		"stop_reason=max_tokens\n", "model=m\n", "iterations=2\n",
		"tool_calls=4\n", "tool_errors=0\n", "input_tokens=100\n",
		"output_tokens=50\n", "duration_ms=99\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("meta missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "error=") {
		t.Fatalf("error= line should be absent when no error:\n%s", got)
	}
}

func TestWriteMetaOutErrorLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "meta.txt")
	m := metaFields{StopReason: messages.StopReasonError, Err: "boom"}
	if err := writeMetaOut(path, m); err != nil {
		t.Fatalf("writeMetaOut error = %v", err)
	}
	body, _ := os.ReadFile(path)
	if !strings.Contains(string(body), "error=boom\n") {
		t.Fatalf("expected error=boom line, got:\n%s", body)
	}
}
