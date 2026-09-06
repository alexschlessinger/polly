package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
)

func TestLineStatusCapabilities(t *testing.T) {
	for _, tt := range []struct {
		name        string
		tty         bool
		env         map[string]string
		live, color bool
	}{
		{"terminal", true, map[string]string{"TERM": "xterm-256color"}, true, true},
		{"no color retains live status", true, map[string]string{"NO_COLOR": "1"}, true, false},
		{"dumb", true, map[string]string{"TERM": "dumb"}, false, false},
		{"redirected", false, map[string]string{"TERM": "xterm-256color"}, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveLineStatusCapabilities(tt.tty, 80, mapGetenv(tt.env))
			if got.live != tt.live || got.color != tt.color {
				t.Fatalf("capabilities = %+v", got)
			}
		})
	}
}

func activityTestUI(t *testing.T, live bool, config *Config) (*lineTurnUI, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "1")
	ui := newLineTurnUIWithCapabilities(config, nil, outputCapabilities{columns: 80})
	out, status := new(bytes.Buffer), new(bytes.Buffer)
	ui.writer, ui.errWriter = out, status
	ui.stdoutTTY, ui.stderrTTY = false, live
	ui.Start()
	t.Cleanup(ui.Stop)
	return ui, out, status
}

func TestLineActivityLiveSettlesOnceAndLeavesAnswerClean(t *testing.T) {
	ui, out, status := activityTestUI(t, true, &Config{})
	ui.ShowThinking("private reasoning")
	call := messages.ChatMessageToolCall{ID: "call", Name: "read_file", Arguments: `{"path":"main.go"}`}
	ui.AppendToolStart([]messages.ChatMessageToolCall{call})
	ui.AppendToolEnd(call, "private tool result", time.Second, nil)
	ui.AppendAssistantText("the answer")
	ui.RecordTurnTokens(12, 8)
	ui.RecordContextUsage(12, 100, false)
	ui.FinishTextTurn()
	ui.SetTurnOutcome(messages.StopReasonEndTurn, nil)
	ui.Stop()
	if got := out.String(); got != "\nthe answer\n" {
		t.Fatalf("answer = %q", got)
	}
	got := status.String()
	for _, want := range []string{"Thinking", "1 tool", "main.go", "Done", "12 in / 8 out", "ctx 12/100", "\r\x1b[2K"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q: %q", want, got)
		}
	}
	if strings.ContainsAny(got, "✓→") || strings.Contains(got, "private") || strings.Contains(got, "\x1b[2m") {
		t.Fatalf("live status duplicated or leaked output/color: %q", got)
	}
	before := status.Len()
	ui.Stop()
	if status.Len() != before {
		t.Fatal("Stop duplicated status")
	}
	select {
	case <-ui.activity.done:
	default:
		t.Fatal("status timer did not stop")
	}
}

func TestLineActivityChildrenAreAttributedAndConcurrent(t *testing.T) {
	ui, out, status := activityTestUI(t, true, &Config{})
	spawn := []messages.ChatMessageToolCall{
		{ID: "a", Name: "spawn_agent", Arguments: `{"label":"review"}`},
		{ID: "b", Name: "spawn_agent", Arguments: `{"label":"tests"}`},
	}
	ui.AppendToolStart(spawn)
	var wg sync.WaitGroup
	for _, call := range spawn {
		child := &childTurnUI{parent: ui, activity: ui.childActivity(call)}
		wg.Go(func() {
			child.ShowThinking("hidden child reasoning")
			tool := messages.ChatMessageToolCall{ID: "same-provider-id", Name: "read_file", Arguments: `{"path":"a.go"}`}
			child.AppendToolStart([]messages.ChatMessageToolCall{tool})
			child.AppendToolMedia(tool, []transcriptImage{{Alt: "frame.png", Width: 8, Height: 4, Inspection: true}})
			child.AppendToolEnd(tool, "hidden child result", time.Second, nil)
			child.AppendAssistantText("hidden child answer")
			child.FinishTextTurn()
			ui.AppendToolEnd(call, "hidden report", 2*time.Second, nil)
		})
	}
	wg.Wait()
	ui.FinishTextTurn()
	ui.Stop()
	got := status.String()
	for _, want := range []string{"review: viewed", "tests: viewed", "2 tools", "2 agents", "2 images viewed"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q: %q", want, got)
		}
	}
	if len(ui.activity.active) != 0 || strings.Contains(got+out.String(), "hidden") {
		t.Fatalf("unsettled or leaked child: %q", got)
	}
}

func TestLineActivityPlainWarningsDoNotSplitRedirectedAnswer(t *testing.T) {
	ui, out, status := activityTestUI(t, false, &Config{})
	ui.AppendAssistantText("the ")
	ui.AppendWarning("check\x1b]52;payload\a")
	ui.AppendAssistantText("answer")
	ui.FinishTextTurn()
	ui.Stop()
	if out.String() != "the answer\n" {
		t.Fatalf("status changed stdout: %q", out.String())
	}
	if strings.ContainsAny(status.String(), "\x1b\r\a") || !strings.Contains(status.String(), "Warning: check") {
		t.Fatalf("unsafe plain status: %q", status.String())
	}
}

func TestLineActivityQuietAndSchema(t *testing.T) {
	for _, quiet := range []bool{false, true} {
		t.Run(fmt.Sprint(quiet), func(t *testing.T) {
			ui, out, status := activityTestUI(t, true, &Config{Quiet: quiet, SchemaPath: "schema.json"})
			ui.ShowThinking("hidden")
			ui.AppendAssistantText("hidden")
			ui.pauseActivity()
			ui.SetTurnOutcome(messages.StopReasonEndTurn, nil)
			ui.Stop()
			if out.Len() != 0 {
				t.Fatalf("status polluted schema: %q", out.String())
			}
			if quiet && status.Len() != 0 {
				t.Fatalf("quiet status: %q", status.String())
			}
			if !quiet && !strings.Contains(status.String(), "Done") {
				t.Fatalf("schema lost stderr status: %q", status.String())
			}
		})
	}
}

func TestLineActivityInterruptedToolsAndFailedResults(t *testing.T) {
	ui, _, status := activityTestUI(t, false, &Config{})
	calls := []messages.ChatMessageToolCall{{ID: "1", Name: "read_file"}, {ID: "2", Name: "bash"}}
	ui.AppendToolStart(calls)
	ui.AppendToolEnd(calls[0], "private", time.Second, errors.New("private error"))
	ui.SetTurnOutcome(messages.StopReasonEndTurn, errors.New("interrupted"))
	ui.Stop()
	got := status.String()
	if !strings.Contains(got, "1 unfinished") || !strings.Contains(got, "1 failed") || !strings.Contains(got, "Stopped") || strings.Contains(got, "Done") || strings.Contains(got, "private") {
		t.Fatalf("interrupted status = %q", got)
	}
}

// Model the current-line replacement used by the renderer: only text before a
// newline survives in scrollback; CR replaces the transient status preceding it.
func settledActivityLines(raw string) string {
	var settled []string
	for _, line := range strings.SplitAfter(raw, "\n") {
		if !strings.HasSuffix(line, "\n") {
			continue
		}
		if i := strings.LastIndex(line, "\r"); i >= 0 {
			line = line[i+1:]
		}
		line = strings.ReplaceAll(line, "\x1b[2K", "")
		settled = append(settled, ansiSGRPattern.ReplaceAllString(line, ""))
	}
	return strings.Join(settled, "")
}

func TestLineActivitySuccessfulFanoutLeavesOnlySummary(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(fmt.Sprint(live), func(t *testing.T) {
			ui, _, status := activityTestUI(t, live, &Config{})
			for i := range 5 {
				spawn := messages.ChatMessageToolCall{ID: fmt.Sprint(i), Name: "spawn_agent", Arguments: `{"task":"Read the file /Users/alex/workspace/polly/README.md in full"}`}
				ui.AppendToolStart([]messages.ChatMessageToolCall{spawn})
				child := &childTurnUI{parent: ui, activity: ui.childActivity(spawn)}
				if child.activity.label != fmt.Sprintf("agent %d", i+1) {
					t.Fatalf("verbose unnamed agent: %q", child.activity.label)
				}
				for j := range 2 {
					call := messages.ChatMessageToolCall{ID: fmt.Sprint(j), Name: "read_file", Arguments: `{"path":"README.md"}`}
					child.AppendToolStart([]messages.ChatMessageToolCall{call})
					child.AppendToolEnd(call, strings.Repeat("private result\n", 502), time.Second, nil)
				}
				ui.AppendToolEnd(spawn, "child report\nreply", time.Second, nil)
			}
			ui.FinishTextTurn()
			ui.Stop()
			got := settledActivityLines(status.String())
			for _, forbidden := range []string{"read_file", "spawn_agent", "README", "Read the file", " lines", "✓", "→", "private result", "child report"} {
				if strings.Contains(got, forbidden) {
					t.Fatalf("successful tool detail %q survived in scrollback: %q", forbidden, got)
				}
			}
			if !strings.Contains(got, "10 tools · 5 agents") {
				t.Fatalf("missing aggregate summary: %q", got)
			}
			if strings.Count(got, "\n") > 2 {
				t.Fatalf("fanout flooded scrollback: %q", got)
			}
		})
	}
}

func TestLineActivityUsesTUIPaletteWhenColorSupported(t *testing.T) {
	for _, tt := range []struct {
		name      string
		live      bool
		noColor   string
		wantColor bool
	}{
		{"color terminal", true, "", true},
		{"no color terminal", true, "1", false},
		{"redirected", false, "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ui, _, status := activityTestUI(t, tt.live, &Config{})
			ui.Stop()
			t.Setenv("NO_COLOR", tt.noColor)
			status.Reset()
			ui.Start()
			ui.ShowThinking("private")
			call := messages.ChatMessageToolCall{ID: "1", Name: "read_file"}
			ui.AppendToolStart([]messages.ChatMessageToolCall{call})
			ui.AppendToolEnd(call, "private", time.Second, errors.New("private failure"))
			ui.FinishTextTurn()
			ui.Stop()
			got := status.String()
			if tt.wantColor {
				for _, want := range []string{"\x1b[0;93;1mThinking", "\x1b[0;91;1m", "\x1b[0;32;1mDone", "\x1b[0;94m1 tool"} {
					if !strings.Contains(got, want) {
						t.Fatalf("missing TUI color %q: %q", want, got)
					}
				}
			} else if ansiSGRPattern.MatchString(got) {
				t.Fatalf("unexpected color: %q", got)
			}
		})
	}
}

func TestLineActivityDoesNotAddToolBoundaryNewlinesToPipedAnswer(t *testing.T) {
	ui, out, _ := activityTestUI(t, false, &Config{})
	ui.AppendAssistantText("before")
	call := messages.ChatMessageToolCall{ID: "1", Name: "read_file"}
	ui.AppendToolStart([]messages.ChatMessageToolCall{call})
	ui.AppendToolEnd(call, "ok", time.Second, nil)
	ui.AppendAssistantText("after")
	ui.FinishTextTurn()
	ui.Stop()
	if out.String() != "before\nafter\n" {
		t.Fatalf("activity added answer whitespace: %q", out.String())
	}
}

func TestLineActivityNarrowStatusAndPause(t *testing.T) {
	ui, _, status := activityTestUI(t, false, &Config{})
	ui.toolMu.Lock()
	status.Reset()
	ui.activity.caps = lineStatusCapabilities{live: true, columns: 2}
	ui.renderActivityLocked()
	ui.toolMu.Unlock()
	if got := status.String(); got != "\r\x1b[2K" {
		t.Fatalf("status would wrap narrow terminal: %q", got)
	}
	ui.pauseActivity()
	before := status.String()
	ui.ShowThinking("thinking while output is paused")
	if status.String() != before {
		t.Fatalf("paused status repainted: %q", status.String())
	}
	ui.Stop()
}

func TestLineActivityQuietSuppressesToolsAndImages(t *testing.T) {
	ui, _, status := activityTestUI(t, true, &Config{Quiet: true})
	call := messages.ChatMessageToolCall{ID: "1", Name: "spawn_agent"}
	ui.AppendToolStart([]messages.ChatMessageToolCall{call})
	if child := ui.childActivity(call); child != nil {
		t.Fatal("quiet turn created child display")
	}
	ui.AppendToolMedia(call, []transcriptImage{{Alt: "frame.png", Inspection: true}})
	ui.AppendToolEnd(call, "ok", time.Second, nil)
	ui.Stop()
	if status.Len() != 0 {
		t.Fatalf("quiet activity = %q", status.String())
	}
	ui.AppendWarning("still visible")
	if status.String() != "Warning: still visible\n" {
		t.Fatalf("quiet warning = %q", status.String())
	}
}

func TestLineActivityInterruptedRawAnswerPreservesPartialBytes(t *testing.T) {
	ui, out, _ := activityTestUI(t, false, &Config{})
	ui.AppendAssistantText("partial")
	ui.SetTurnOutcome(messages.StopReasonError, errors.New("interrupted"))
	ui.Stop()
	if out.String() != "partial" {
		t.Fatalf("interruption changed captured answer: %q", out.String())
	}
}

// lockedBuffer captures the approval prompt, written outside toolMu, in the
// same stream as status writes made under it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitForStatusCount(t *testing.T, status *lockedBuffer, want string, count int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for strings.Count(status.String(), want) < count {
		if time.Now().After(deadline) {
			t.Fatalf("status never showed %q x%d: %q", want, count, status.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A child's approval prompt runs inside a parent tool goroutine; siblings must
// keep reporting while it waits on stdin, and their notices must not land on
// the prompt line.
func TestLineActivityApprovalPromptDoesNotBlockSiblings(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "1")
	ui := newLineTurnUIWithCapabilities(&Config{}, nil, outputCapabilities{columns: 80})
	status := new(lockedBuffer)
	stdin, answers := io.Pipe()
	ui.writer, ui.errWriter = io.Discard, status
	ui.stdoutTTY, ui.stderrTTY = false, true
	ui.approver = &toolApprover{reader: bufio.NewReader(stdin), out: status}
	ui.Start()
	t.Cleanup(ui.Stop)
	// Closing stdin releases any prompt still open when an assertion fails,
	// so Stop can take toolMu instead of hanging the test.
	t.Cleanup(func() { answers.Close() })

	spawn := []messages.ChatMessageToolCall{
		{ID: "a", Name: "spawn_agent", Arguments: `{"label":"review"}`},
		{ID: "b", Name: "spawn_agent", Arguments: `{"label":"tests"}`},
	}
	ui.AppendToolStart(spawn)
	review := &childTurnUI{parent: ui, activity: ui.childActivity(spawn[0])}
	tests := &childTurnUI{parent: ui, activity: ui.childActivity(spawn[1])}
	tool := messages.ChatMessageToolCall{ID: "t", Name: "read_file", Arguments: `{"path":"a.go"}`}
	const prompt = "allow? (Y/n/a): "

	reviewApproved := make(chan []bool, 1)
	go func() { reviewApproved <- review.ApproveToolCalls([]messages.ChatMessageToolCall{tool}) }()
	waitForStatusCount(t, status, prompt, 1)

	siblingDone := make(chan struct{})
	go func() {
		defer close(siblingDone)
		tests.ShowThinking("still streaming")
		tests.AppendToolStart([]messages.ChatMessageToolCall{tool})
		tests.AppendToolEnd(tool, "", time.Second, errors.New("boom"))
		ui.AppendToolEnd(spawn[1], "report", time.Second, nil)
	}()
	select {
	case <-siblingDone:
	case <-time.After(5 * time.Second):
		answers.Close()
		t.Fatal("sibling child blocked behind the approval prompt")
	}
	if got := status.String(); !strings.HasSuffix(got, prompt) || strings.Contains(got, "✗") {
		t.Fatalf("prompt overwritten while open: %q", got)
	}

	// A second prompt waits its turn on the shared reader, so the answers
	// line up with the prompts in order.
	testsApproved := make(chan []bool, 1)
	go func() { testsApproved <- tests.ApproveToolCalls([]messages.ChatMessageToolCall{tool}) }()
	fmt.Fprintln(answers, "y")
	if got := <-reviewApproved; len(got) != 1 || !got[0] {
		t.Fatalf("first approval = %v", got)
	}
	waitForStatusCount(t, status, prompt, 2)
	fmt.Fprintln(answers, "n")
	if got := <-testsApproved; len(got) != 1 || got[0] {
		t.Fatalf("second approval = %v", got)
	}

	got := status.String()
	first := strings.Index(got, prompt)
	failed := strings.Index(got, "✗ tests: read_file · failed")
	second := strings.LastIndex(got, prompt)
	if first < 0 || failed < first || second < failed {
		t.Fatalf("queued notice out of order: %q", got)
	}
	if !strings.Contains(got[failed:second], "\r\x1b[2K") {
		t.Fatalf("status did not resume after the prompt: %q", got)
	}
}
