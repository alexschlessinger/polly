# Headless Run Outcome Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give headless `polly` runs a machine-readable outcome — granular exit codes plus an optional flat `key=value` metadata sidecar — without putting the answer in a brittle structured blob.

**Architecture:** The answer stays plain, streamed stdout. A new `StopReasonMaxIterations` + exported `ErrMaxIterations` lets the agent distinguish iteration-cap from real errors. In `cmd/polly`, pure helpers (`classifyOutcome`, `buildMeta`, `writeMetaOut`) live in a new `outcome.go`; `executeTurn` times the run, counts tool calls/errors via the existing `OnToolEnd` callback, writes the sidecar atomically, and returns a typed `exitError` carrying the intended exit code, which `main` maps.

**Tech Stack:** Go, `urfave/cli/v3`, standard library only (`os`, `errors`, `strings`, `time`).

**Reference spec:** `docs/superpowers/specs/2026-06-01-headless-run-outcome-design.md`

**Exit code contract:** `0` end_turn · `2` max_tokens · `3` max_iterations · `1` hard error · `130` interrupted.

---

### Task 1: `StopReasonMaxIterations` + `ErrMaxIterations`

**Files:**
- Modify: `messages/types.go:9-18` (StopReason consts)
- Modify: `llm/agent.go:6` (imports already include `errors`), add package var, and `llm/agent.go:290-299` (loop-exhaustion return)
- Test: `llm/agent_test.go`

- [ ] **Step 1: Write the failing test**

Append to `llm/agent_test.go`:

```go
// alwaysToolUseLLM keeps asking to call a tool so the agent loop never
// terminates on its own — it must hit the MaxIterations cap.
type alwaysToolUseLLM struct{ calls int }

func (a *alwaysToolUseLLM) ChatCompletionStream(_ context.Context, _ *CompletionRequest, processor EventStreamProcessor) <-chan *messages.StreamEvent {
	a.calls++
	ch := make(chan messages.ChatMessage, 1)
	ch <- messages.ChatMessage{
		Role:       messages.MessageRoleAssistant,
		ToolCalls:  []messages.ChatMessageToolCall{{ID: "tc", Name: "noop", Arguments: "{}"}},
		StopReason: messages.StopReasonToolUse,
	}
	close(ch)
	return processor.ProcessMessagesToEvents(ch)
}

func TestAgentMaxIterationsStopReason(t *testing.T) {
	noop := &tools.Func{
		Name: "noop",
		Run:  func(_ context.Context, _ tools.Args) (string, error) { return "ok", nil },
	}
	registry := tools.NewToolRegistry([]tools.Tool{noop})
	agent := NewAgent(&alwaysToolUseLLM{}, registry, AgentConfig{MaxIterations: 2})

	resp, err := agent.Run(context.Background(), &CompletionRequest{
		Messages: messages.User("go"),
	}, nil)

	if !errors.Is(err, ErrMaxIterations) {
		t.Fatalf("err = %v, want ErrMaxIterations", err)
	}
	if resp == nil || resp.Message == nil {
		t.Fatal("expected partial response with a final message")
	}
	if resp.Message.StopReason != messages.StopReasonMaxIterations {
		t.Fatalf("stop reason = %q, want %q", resp.Message.StopReason, messages.StopReasonMaxIterations)
	}
	if resp.IterationCount != 2 {
		t.Fatalf("IterationCount = %d, want 2", resp.IterationCount)
	}
}
```

Add `"errors"` to the test file's import block if not already present.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./llm/ -run TestAgentMaxIterationsStopReason -v`
Expected: FAIL — `ErrMaxIterations` undefined (compile error) and/or stop reason mismatch.

- [ ] **Step 3a: Add the StopReason constant**

In `messages/types.go`, after the `StopReasonError` const (line 18), add:

```go
	// StopReasonMaxIterations indicates the agent hit its iteration cap
	StopReasonMaxIterations StopReason = "max_iterations"
```

- [ ] **Step 3b: Add the sentinel and update loop exhaustion**

In `llm/agent.go`, add a package-level var near the top (after the imports/const block):

```go
// ErrMaxIterations is returned (with a partial AgentResponse) when the agent
// loop reaches its MaxIterations cap before the model finishes.
var ErrMaxIterations = errors.New("max iterations exceeded")
```

Then replace the loop-exhaustion block at `llm/agent.go:290-299`:

```go
	err := errors.New("max iterations exceeded")
	if cb != nil && cb.OnError != nil {
		cb.OnError(err)
	}
	// Return the partial response so the caller can save the history
	return &AgentResponse{
		Message:        &msgs[len(msgs)-1], // Last message
		AllMessages:    allGenerated,
		IterationCount: a.config.MaxIterations,
	}, err
```

with:

```go
	last := &msgs[len(msgs)-1]
	last.StopReason = messages.StopReasonMaxIterations
	if cb != nil && cb.OnError != nil {
		cb.OnError(ErrMaxIterations)
	}
	// Return the partial response so the caller can save the history
	return &AgentResponse{
		Message:        last,
		AllMessages:    allGenerated,
		IterationCount: a.config.MaxIterations,
	}, ErrMaxIterations
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./llm/ ./messages/ -v 2>&1 | tail -20`
Expected: PASS, including `TestAgentMaxIterationsStopReason` and the existing suite.

- [ ] **Step 5: Commit**

```bash
git add messages/types.go llm/agent.go llm/agent_test.go
git commit -m "agent: distinguish max-iterations with stop reason and sentinel error"
```

---

### Task 2: `classifyOutcome` + `exitError`

**Files:**
- Create: `cmd/polly/outcome.go`
- Test: `cmd/polly/outcome_test.go`

- [ ] **Step 1: Write the failing test**

Create `cmd/polly/outcome_test.go`:

```go
package main

import (
	"errors"
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
	// no inner error: still has a code-only message
	e2 := &exitError{code: 2}
	if e2.Error() == "" {
		t.Fatal("exitError with no inner error should still render a message")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/polly/ -run 'TestClassifyOutcome|TestExitError' -v`
Expected: FAIL — `classifyOutcome` / `exitError` undefined.

- [ ] **Step 3: Write minimal implementation**

Create `cmd/polly/outcome.go`:

```go
package main

import (
	"errors"
	"fmt"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
)

// exitError carries the process exit code a run should produce. main() unwraps
// it; any other error maps to exit 1.
type exitError struct {
	code int
	err  error // optional; nil for a nonzero-but-not-an-error outcome (e.g. truncation)
}

func (e *exitError) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	return fmt.Sprintf("exit status %d", e.code)
}

func (e *exitError) Unwrap() error { return e.err }

// classifyOutcome maps an agent run's (response, error) to a terminal stop
// reason and the process exit code per the documented contract:
// 0 end_turn, 2 max_tokens, 3 max_iterations, 1 hard error.
func classifyOutcome(resp *llm.AgentResponse, err error) (messages.StopReason, int) {
	switch {
	case errors.Is(err, llm.ErrMaxIterations):
		return messages.StopReasonMaxIterations, 3
	case err != nil:
		return messages.StopReasonError, 1
	case resp != nil && resp.Message != nil && resp.Message.StopReason == messages.StopReasonMaxTokens:
		return messages.StopReasonMaxTokens, 2
	case resp != nil && resp.Message != nil && resp.Message.StopReason != "":
		return resp.Message.StopReason, 0
	default:
		return messages.StopReasonEndTurn, 0
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/polly/ -run 'TestClassifyOutcome|TestExitError' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/polly/outcome.go cmd/polly/outcome_test.go
git commit -m "polly: add outcome classification and exitError"
```

---

### Task 3: metadata sidecar (`buildMeta` + `writeMetaOut`)

**Files:**
- Modify: `cmd/polly/outcome.go`
- Test: `cmd/polly/outcome_test.go`

- [ ] **Step 1: Write the failing test**

Append to `cmd/polly/outcome_test.go`:

```go
import (
	// add to the existing import block:
	"os"
	"path/filepath"
	"strings"
)

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
	// No leftover temp file.
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/polly/ -run 'TestBuildMeta|TestWriteMetaOut' -v`
Expected: FAIL — `metaFields` / `buildMeta` / `writeMetaOut` undefined.

- [ ] **Step 3: Write minimal implementation**

Append to `cmd/polly/outcome.go` (and add `"os"`, `"strings"` to its import block):

```go
// metaFields is the flat outcome record written to a --meta-out sidecar.
type metaFields struct {
	StopReason   messages.StopReason
	Model        string
	Iterations   int
	ToolCalls    int
	ToolErrors   int
	InputTokens  int
	OutputTokens int
	DurationMS   int64
	Err          string // single-line; present only on hard error
}

// buildMeta assembles the sidecar record from a run's results. It classifies
// the outcome so stop_reason and the error line stay consistent with the exit
// code.
func buildMeta(resp *llm.AgentResponse, err error, model string, toolCalls, toolErrors, inTokens, outTokens int, durationMS int64) metaFields {
	stopReason, _ := classifyOutcome(resp, err)
	iterations := 0
	if resp != nil {
		iterations = resp.IterationCount
	}
	errStr := ""
	if stopReason == messages.StopReasonError && err != nil {
		errStr = oneLine(err.Error())
	}
	return metaFields{
		StopReason:   stopReason,
		Model:        model,
		Iterations:   iterations,
		ToolCalls:    toolCalls,
		ToolErrors:   toolErrors,
		InputTokens:  inTokens,
		OutputTokens: outTokens,
		DurationMS:   durationMS,
		Err:          errStr,
	}
}

// oneLine collapses newlines so a value can't break the line-oriented format.
func oneLine(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r", " "), "\n", " ")
}

// writeMetaOut writes the record as flat key=value lines, atomically
// (temp file + rename) so a partial write never leaves a half-written sidecar.
func writeMetaOut(path string, m metaFields) error {
	var b strings.Builder
	fmt.Fprintf(&b, "stop_reason=%s\n", m.StopReason)
	fmt.Fprintf(&b, "model=%s\n", m.Model)
	fmt.Fprintf(&b, "iterations=%d\n", m.Iterations)
	fmt.Fprintf(&b, "tool_calls=%d\n", m.ToolCalls)
	fmt.Fprintf(&b, "tool_errors=%d\n", m.ToolErrors)
	fmt.Fprintf(&b, "input_tokens=%d\n", m.InputTokens)
	fmt.Fprintf(&b, "output_tokens=%d\n", m.OutputTokens)
	fmt.Fprintf(&b, "duration_ms=%d\n", m.DurationMS)
	if m.Err != "" {
		fmt.Fprintf(&b, "error=%s\n", m.Err)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/polly/ -run 'TestBuildMeta|TestWriteMetaOut' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/polly/outcome.go cmd/polly/outcome_test.go
git commit -m "polly: add key=value outcome sidecar writer"
```

---

### Task 4: `--meta-out` flag + `Config.MetaOut`

**Files:**
- Modify: `cmd/polly/types.go:54-58` (Input/Output section of `Config`)
- Modify: `cmd/polly/config.go:310-322` (`outputConfigFlags`) and `cmd/polly/config.go:83-90` (`parseConfig`)
- Test: `cmd/polly/config_test.go` (create if absent)

- [ ] **Step 1: Write the failing test**

Create or append to `cmd/polly/config_test.go`:

```go
package main

import (
	"context"
	"testing"

	"github.com/urfave/cli/v3"
)

func TestParseConfigMetaOut(t *testing.T) {
	var got string
	cmd := &cli.Command{
		Flags: func() []cli.Flag { f, _ := defineFlagsWithGroups(); return f }(),
		Action: func(_ context.Context, c *cli.Command) error {
			got = parseConfig(c).MetaOut
			return nil
		},
	}
	if err := cmd.Run(context.Background(), []string{"polly", "--meta-out", "/tmp/m.txt", "-p", "hi"}); err != nil {
		t.Fatalf("run error = %v", err)
	}
	if got != "/tmp/m.txt" {
		t.Fatalf("MetaOut = %q, want /tmp/m.txt", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/polly/ -run TestParseConfigMetaOut -v`
Expected: FAIL — `MetaOut` field undefined / flag unknown.

- [ ] **Step 3a: Add the Config field**

In `cmd/polly/types.go`, in the `// Input/Output configuration` block (after `SchemaPath string`), add:

```go
	MetaOut    string   // Path to write the run-outcome sidecar (key=value)
```

- [ ] **Step 3b: Add the flag**

In `cmd/polly/config.go`, add to the slice returned by `outputConfigFlags()` (after the `debug` flag):

```go
		&cli.StringFlag{
			Name:  "meta-out",
			Usage: "Write a key=value run-outcome sidecar to this file",
		},
```

- [ ] **Step 3c: Wire it in parseConfig**

In `cmd/polly/config.go`, in `parseConfig` next to `SchemaPath: cmd.String("schema"),`, add:

```go
		MetaOut:    cmd.String("meta-out"),
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/polly/ -run TestParseConfigMetaOut -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/polly/types.go cmd/polly/config.go cmd/polly/config_test.go
git commit -m "polly: add --meta-out flag"
```

---

### Task 5: wire outcome into `executeTurn` and `main`

**Files:**
- Modify: `cmd/polly/polly.go:25-31` (`main`), `cmd/polly/polly.go:388-492` (`executeTurn`)

- [ ] **Step 1: Add tool counters to the callbacks**

In `executeTurn`, declare counters before `state.agent.Run` (just before the `resp, err := state.agent.Run(...)` call at `polly.go:416`):

```go
	var toolCalls, toolErrors int
	turnStart := time.Now()
```

Then in the same `AgentCallbacks` literal, replace the existing `OnToolEnd`:

```go
		OnToolEnd: func(tc messages.ChatMessageToolCall, result string, duration time.Duration, err error) {
			turnUI.AppendToolEnd(tc, result, duration, err)
		},
```

with:

```go
		OnToolEnd: func(tc messages.ChatMessageToolCall, result string, duration time.Duration, err error) {
			toolCalls++
			if err != nil {
				toolErrors++
			}
			turnUI.AppendToolEnd(tc, result, duration, err)
		},
```

- [ ] **Step 2: Hoist the token totals to function scope**

The token totals are currently declared *inside* an `if resp != nil {` block
(`polly.go:468`, `var in, out int`), so the outcome code below can't see them.
Hoist them: just before the `if resp != nil {` token block, add:

```go
	var in, out int
```

and change the line `var in, out int` inside that block to a bare comment or
remove it so the inner loop assigns the function-scoped vars (i.e. the loop body
`if t := m.GetInputTokens(); t > in { in = t }` and `out += ...` now write the
outer `in`/`out`). Leave `turnUI.RecordTurnTokens(in, out)` as is.

- [ ] **Step 3: Replace the tail of executeTurn with outcome handling**

The current tail (`polly.go:471-491`) is:

```go
	if err := persistActiveSkills(state.session, state.skillRuntime, state.skillSources); err != nil {
		return fmt.Errorf("failed to persist active skills: %w", err)
	}
	if err != nil {
		return err
	}

	if resp.Message != nil && resp.Message.StopReason == messages.StopReasonMaxTokens {
		turnUI.AppendWarning(fmt.Sprintf("response truncated (hit %d token limit, use --maxtokens to increase)", config.MaxTokens))
	}

	if config.SchemaPath != "" {
		var content string
		if resp.Message != nil {
			content = resp.Message.Content
		}
		return outputStructured(content, schema)
	}

	turnUI.FinishTextTurn()
	return nil
}
```

Replace it with:

```go
	if perr := persistActiveSkills(state.session, state.skillRuntime, state.skillSources); perr != nil {
		return fmt.Errorf("failed to persist active skills: %w", perr)
	}

	stopReason, code := classifyOutcome(resp, err)
	if config.MetaOut != "" {
		meta := buildMeta(resp, err, config.Model, toolCalls, toolErrors, in, out, time.Since(turnStart).Milliseconds())
		if werr := writeMetaOut(config.MetaOut, meta); werr != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to write meta-out sidecar %s: %v\n", config.MetaOut, werr)
		}
	}

	// Hard error or max-iterations: any partial answer already streamed to
	// stdout; surface the exit code (3 for max-iterations, 1 otherwise).
	if err != nil {
		return &exitError{code: code, err: err}
	}

	if stopReason == messages.StopReasonMaxTokens {
		turnUI.AppendWarning(fmt.Sprintf("response truncated (hit %d token limit, use --maxtokens to increase)", config.MaxTokens))
	}

	if config.SchemaPath != "" {
		var content string
		if resp.Message != nil {
			content = resp.Message.Content
		}
		if oerr := outputStructured(content, schema); oerr != nil {
			return oerr
		}
	} else {
		turnUI.FinishTextTurn()
	}

	// Output is done; signal a nonzero code for incomplete-but-produced runs
	// (e.g. truncation -> 2) without an error message.
	if code != 0 {
		return &exitError{code: code}
	}
	return nil
}
```

Note: `in` and `out` are the function-scoped token totals from Step 2. This
block uses only `fmt` and `os`, both already imported in `polly.go`.

- [ ] **Step 4: Update main() to map exitError**

Replace `cmd/polly/polly.go:25-31`:

```go
func main() {
	command := getCommand()
	if err := command.Run(context.Background(), os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		cleanupAndExit(1)
	}
}
```

with:

```go
func main() {
	command := getCommand()
	if err := command.Run(context.Background(), os.Args); err != nil {
		var ee *exitError
		if errors.As(err, &ee) {
			if ee.err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", ee.err)
			}
			cleanupAndExit(ee.code)
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		cleanupAndExit(1)
	}
}
```

Add `"errors"` to the `cmd/polly/polly.go` import block (it is not currently
imported).

- [ ] **Step 5: Build and run the full cmd/polly suite**

Run: `go build -o polly ./cmd/polly/ && go test ./cmd/polly/ ./llm/ ./messages/ 2>&1 | tail -20`
Expected: build succeeds; all tests PASS.

- [ ] **Step 6: Integration-verify exit codes and sidecar (needs `POLLYTOOL_DEEPSEEKKEY`)**

Run:

```bash
# exit 0 + sidecar on a normal run
./polly -m deepseek/deepseek-v4-flash -p "say hi in three words" --quiet --meta-out /tmp/m0.txt; echo "exit=$?"
cat /tmp/m0.txt   # stop_reason=end_turn, tool_calls=0, ...

# exit 2 on truncation (tiny token cap forces max_tokens)
./polly -m deepseek/deepseek-v4-flash -p "write a long paragraph" --quiet --maxtokens 1 --meta-out /tmp/m2.txt; echo "exit=$?"
grep stop_reason /tmp/m2.txt   # stop_reason=max_tokens
```

Expected: first `exit=0` with `stop_reason=end_turn`; second `exit=2` with `stop_reason=max_tokens`.
(If no key is set, skip this step and rely on the unit tests.)

- [ ] **Step 7: Commit**

```bash
git add cmd/polly/polly.go
git commit -m "polly: write outcome sidecar and map granular exit codes"
```

---

### Task 6: scripts + skill integration

**Files:**
- Modify: `polly-agent/run-agent.sh`
- Modify: `polly-agent/SKILL.md`

- [ ] **Step 1: Feed the prompt via stdin and write a per-agent sidecar**

In `polly-agent/run-agent.sh`, add `mkdir -p "$dir/meta"` next to the existing `mkdir -p "$dir/out" "$dir/err"`, then replace the polly invocation:

```bash
"$polly" -m "$model" -p "$prompt" --tool "$tool" --quiet \
  > "$dir/out/$id.txt" 2> "$dir/err/$id.log"
rc=$?
```

with (prompt via stdin avoids `Argument list too long`; `-p` omitted so polly reads stdin):

```bash
printf '%s' "$prompt" | "$polly" -m "$model" --tool "$tool" --quiet \
  --meta-out "$dir/meta/$id.txt" \
  > "$dir/out/$id.txt" 2> "$dir/err/$id.log"
rc=$?
```

- [ ] **Step 2: Verify the scripts end-to-end (needs `POLLYTOOL_DEEPSEEKKEY`)**

Run:

```bash
cd polly-agent
export POLLY_BIN=$(cd .. && pwd)/polly
export POLLY_AGENT_DIR=/tmp/polly-agent-plan
rm -rf "$POLLY_AGENT_DIR"
./run-agent.sh greet "say hello in five words"
echo "exit=$?"
cat "$POLLY_AGENT_DIR/out/greet.txt"     # the answer
cat "$POLLY_AGENT_DIR/meta/greet.txt"    # stop_reason=end_turn, tool_calls=...
cat "$POLLY_AGENT_DIR/status.tsv"        # greet  0  ok
```

Expected: `exit=0`, an answer in `out/greet.txt`, a populated `meta/greet.txt`, and a `greet 0 ok` row.
(If no key is set, skip; the binary behavior is covered by Task 5's unit tests.)

- [ ] **Step 3: Update SKILL.md**

In `polly-agent/SKILL.md`, in the "Launch one sub-agent" section, add `meta/<id>.txt` to the list of files written:

```markdown
- `out/<id>.txt` — the answer
- `err/<id>.log` — tool progress and any error
- `meta/<id>.txt` — outcome sidecar: `stop_reason`, `tool_calls`, `tool_errors`, tokens, `duration_ms`
- a row in `status.tsv` — `<id> <TAB> <exit> <TAB> ok|fail`
```

Replace the exit-codes bullet in "Read the results" with the granular table:

```markdown
- Exit codes (also in `status.tsv`): `0` ok · `2` truncated (`max_tokens`) ·
  `3` iteration cap (`max_iterations`) · `1` hard error · `130` interrupted.
  Find iteration-capped agents with `awk -F'\t' '$2==3' status.tsv`.
- **`ok`/exit 0 still doesn't mean the answer is right.** Read `meta/<id>.txt`:
  a high `tool_errors` (e.g. every call failed) is the machine-readable tell that
  the agent thrashed and likely gave up — the silent-failure signal the exit code
  can't carry. Then sanity-check `out/<id>.txt`.
```

- [ ] **Step 4: Verify the skill text is accurate**

Run: `grep -n "meta/<id>.txt\|tool_errors\|\$2==3" polly-agent/SKILL.md`
Expected: the new lines are present.

- [ ] **Step 5: Commit**

```bash
git add polly-agent/run-agent.sh polly-agent/SKILL.md
git commit -m "polly-agent: stdin prompts, per-agent meta sidecar, exit-code docs"
```

---

## Final verification

- [ ] Run the whole suite: `go test ./... 2>&1 | grep -v '^ok\|no test files'` — expect no output (all pass).
- [ ] `go vet ./cmd/polly/ ./llm/ ./messages/` — clean.
- [ ] `gofmt -l cmd/polly/ llm/ messages/` — no files listed.
- [ ] Confirm the exit-code contract end-to-end (Task 5 Step 5) if a provider key is available.
