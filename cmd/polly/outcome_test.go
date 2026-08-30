package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"sync"
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
		// The progress-saved wrapper must not change how a failure classifies.
		{"max_iterations_progress_saved", resp(messages.StopReasonMaxIterations), &turnProgressSavedError{cause: llm.ErrMaxIterations}, messages.StopReasonMaxIterations, 3},
		{"hard_error_progress_saved", nil, &turnProgressSavedError{cause: errors.New("boom")}, messages.StopReasonError, 1},
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

func TestTurnToolStatsConcurrentRecord(t *testing.T) {
	stats := &turnToolStats{}
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stats.record("bash", errors.New("boom"))
		}()
	}
	wg.Wait()
	calls, errCount, lastTool, failures := stats.snapshot()
	if calls != 100 || errCount != 100 {
		t.Fatalf("lost updates: calls=%d errors=%d, want 100/100", calls, errCount)
	}
	if lastTool != "bash" {
		t.Fatalf("lastTool = %q, want bash", lastTool)
	}
	if len(failures) != maxEnumeratedToolErrors {
		t.Fatalf("failures accumulation should stop at %d, got %d", maxEnumeratedToolErrors, len(failures))
	}
}

func TestBuildMeta(t *testing.T) {
	r := &llm.AgentResponse{
		Message:        &messages.ChatMessage{StopReason: messages.StopReasonEndTurn},
		IterationCount: 3,
		PromptCache:    llm.PromptCacheStats{ReadInputTokens: 900, WriteInputTokens: 300},
	}
	stats := &turnToolStats{}
	stats.record("read", nil)
	stats.record("read", nil)
	stats.record("read", nil)
	stats.record("fetch", errors.New("timeout"))
	stats.record("bash", nil)
	m := buildMeta(messages.StopReasonEndTurn, r, nil, "deepseek/deepseek-v4-flash", stats, 1200, 600, 8123)
	if m.StopReason != messages.StopReasonEndTurn || m.Iterations != 3 ||
		m.ToolCalls != 5 || m.ToolErrors != 1 || m.InputTokens != 1200 ||
		m.OutputTokens != 600 || m.CacheReadTokens != 900 || m.CacheWriteTokens != 300 ||
		m.DurationMS != 8123 || m.Model != "deepseek/deepseek-v4-flash" {
		t.Fatalf("buildMeta produced %+v", m)
	}
	if m.LastTool != "bash" || len(m.ToolFailures) != 1 || m.ToolFailures[0] != "fetch: timeout" {
		t.Fatalf("buildMeta did not carry tool detail: %+v", m)
	}
	if m.Err != "" {
		t.Fatalf("Err should be empty on success, got %q", m.Err)
	}
}

func TestBuildMetaHardError(t *testing.T) {
	m := buildMeta(messages.StopReasonError, nil, errors.New("api failed"), "m", &turnToolStats{}, 0, 0, 5)
	if m.StopReason != messages.StopReasonError {
		t.Fatalf("StopReason = %q, want error", m.StopReason)
	}
	if m.Err == "" {
		t.Fatal("Err should be populated on hard error")
	}
}

func trailer(m metaFields) string {
	var b bytes.Buffer
	writeMetaTrailer(&b, m)
	return b.String()
}

func TestWriteMetaTrailerPrefixesEveryLine(t *testing.T) {
	got := trailer(metaFields{
		StopReason: messages.StopReasonMaxTokens, Model: "m", Iterations: 2,
		ToolCalls: 4, ToolErrors: 0, InputTokens: 100, OutputTokens: 50,
		CacheReadTokens: 80, CacheWriteTokens: 20, DurationMS: 99,
	})
	for _, line := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
		if !strings.HasPrefix(line, "polly-meta ") {
			t.Fatalf("every trailer line must start with the sentinel, got %q", line)
		}
	}
	for _, want := range []string{
		"polly-meta stop_reason=max_tokens\n", "polly-meta model=m\n",
		"polly-meta iterations=2\n", "polly-meta tool_calls=4\n",
		"polly-meta input_tokens=100\n", "polly-meta cache_read_tokens=80\n",
		"polly-meta cache_write_tokens=20\n", "polly-meta duration_ms=99\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("trailer missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "error=") || strings.Contains(got, "last_tool=") || strings.Contains(got, "tool_error.") {
		t.Fatalf("absent fields must be omitted:\n%s", got)
	}
}

func TestWriteMetaTrailerToolFailures(t *testing.T) {
	got := trailer(metaFields{
		StopReason: messages.StopReasonEndTurn, LastTool: "bash",
		ToolFailures: []string{"bash: exit status 1", "fetch: timeout"},
	})
	for _, want := range []string{
		"polly-meta last_tool=bash\n",
		"polly-meta tool_error.1=bash: exit status 1\n",
		"polly-meta tool_error.2=fetch: timeout\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("trailer missing %q in:\n%s", want, got)
		}
	}
}

func TestWriteMetaTrailerErrorLine(t *testing.T) {
	got := trailer(metaFields{StopReason: messages.StopReasonError, Err: "boom"})
	if !strings.Contains(got, "polly-meta error=boom\n") {
		t.Fatalf("expected error line, got:\n%s", got)
	}
	// The writer owns line-format sanitization: a multi-line error message
	// must collapse to one trailer line.
	got = trailer(metaFields{StopReason: messages.StopReasonError, Err: "api\nfailed"})
	if !strings.Contains(got, "polly-meta error=api failed\n") {
		t.Fatalf("expected collapsed error line, got:\n%s", got)
	}
}

func TestWriteMetaTrailerCapsFailures(t *testing.T) {
	fails := make([]string, 15)
	for i := range fails {
		fails[i] = fmt.Sprintf("bash: err %d", i)
	}
	got := trailer(metaFields{StopReason: messages.StopReasonEndTurn, ToolFailures: fails})
	if strings.Contains(got, "tool_error.11=") {
		t.Fatalf("must cap at 10 failures:\n%s", got)
	}
	if !strings.Contains(got, "tool_error.10=") {
		t.Fatalf("expected first 10 failures:\n%s", got)
	}
}
