package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/control"
	"github.com/alexschlessinger/pollytool/control/controltest"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/urfave/cli/v3"
)

// recordingTurnUI keeps what a turn told its UI.
type recordingTurnUI struct {
	turnUIBase
	mu         sync.Mutex
	text       strings.Builder
	thinking   strings.Builder
	starts     []messages.ChatMessageToolCall
	ends       []string
	results    []messages.ChatMessage
	warnings   []string
	notices    []string
	in, out    int
	used       int
	completion *turnCompletion
	retries    int
}

func (u *recordingTurnUI) ShowThinking(chunk string) {
	u.mu.Lock()
	u.thinking.WriteString(chunk)
	u.mu.Unlock()
}
func (u *recordingTurnUI) AppendAssistantText(content string) {
	u.mu.Lock()
	u.text.WriteString(content)
	u.mu.Unlock()
}
func (u *recordingTurnUI) AppendToolStart(calls []messages.ChatMessageToolCall) {
	u.mu.Lock()
	u.starts = append(u.starts, calls...)
	u.mu.Unlock()
}
func (u *recordingTurnUI) ApproveToolCalls(_ context.Context, _ string, calls []messages.ChatMessageToolCall) []bool {
	return approveAllToolCalls(calls)
}
func (u *recordingTurnUI) AppendToolEnd(call messages.ChatMessageToolCall, result string, _ time.Duration, err error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	outcome := "ok"
	if err != nil {
		outcome = "failed"
	}
	u.ends = append(u.ends, call.Name+":"+outcome+":"+result)
}
func (u *recordingTurnUI) AppendToolResult(_ messages.ChatMessageToolCall, result messages.ChatMessage) {
	u.mu.Lock()
	u.results = append(u.results, result)
	u.mu.Unlock()
}
func (u *recordingTurnUI) AppendToolMedia(messages.ChatMessageToolCall, []style.Image) {}
func (u *recordingTurnUI) AppendWarning(text string) {
	u.mu.Lock()
	u.warnings = append(u.warnings, text)
	u.mu.Unlock()
}
func (u *recordingTurnUI) AppendNotice(text string) {
	u.mu.Lock()
	u.notices = append(u.notices, text)
	u.mu.Unlock()
}
func (u *recordingTurnUI) RecordTurnTokens(in, out int, _ bool) {
	u.mu.Lock()
	u.in, u.out = in, out
	u.mu.Unlock()
}
func (u *recordingTurnUI) RecordContextUsage(used int, _ bool, _ contextBudgetDetails) {
	u.mu.Lock()
	u.used = used
	u.mu.Unlock()
}
func (u *recordingTurnUI) CompleteTurn(c turnCompletion) {
	u.mu.Lock()
	u.completion = &c
	u.mu.Unlock()
}
func (u *recordingTurnUI) ModelRetry(llm.RetryEvent) {
	u.mu.Lock()
	u.retries++
	u.mu.Unlock()
}

// backendRig is a launch on a control daemon in the test's process.
type backendRig struct {
	srv     *controltest.Server
	config  *Config
	backend *remoteBackend
	opener  *conversationOpener
}

func newBackendRig(t *testing.T, script controltest.Script) *backendRig {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", filepath.Join(dir, "home"))
	t.Setenv(control.RuntimeDirVar, filepath.Join(dir, "runtime"))
	t.Setenv("POLLYTOOL_OPENAIKEY", "test-key")
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(filepath.Join(work, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)
	srv := controltest.New(t, script)
	config := &Config{Backend: backendConfig{URL: srv.URL()}, Launch: Settings{Model: "openai/test-model", MaxIterations: 8, ToolTimeout: time.Minute}}
	backend, err := newRemoteBackend(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { backend.Close() })
	return &backendRig{srv: srv, config: config, backend: backend, opener: &conversationOpener{config: config, sessionStore: backend.store, backend: backend}}
}

func (rig *backendRig) open(t *testing.T, name string) *conversationState {
	t.Helper()
	state, err := rig.opener.open(context.Background(), name, rig.config.Launch.clone(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	return state
}

func TestBackendTurnTellsTheUIWhatTheBackendDid(t *testing.T) {
	call := control.LineToolCall{ID: "call_1", Name: "bash", Arguments: `{"command":"echo hi"}`, DisplayID: "d1"}
	rig := newBackendRig(t, func(ctx context.Context, tt *controltest.Turn) control.Reply {
		tt.Text(control.TextReasoning, "thinking it over")
		tt.Text(control.TextAnswer, "\n\nlet me look")
		tt.EndText()
		iter := uint32(0)
		tt.Message(controltest.Assistant("let me look", call), &iter, &control.LineUsage{InputTokens: 900, OutputTokens: 20, TokensReported: true})
		tt.Tool(control.ToolEvent{ID: call.ID, Display: "d1", Name: "bash", Summary: "echo hi", State: "start", Of: 1})
		tt.Tool(control.ToolEvent{ID: call.ID, Name: "bash", State: "end", Of: 1, Ns: uint64(time.Millisecond)})
		tt.Message(controltest.ToolResult(call, "hi", true), nil, nil)
		tt.Notice(control.Notice{Kind: "note", Feature: "compaction", Text: "compacted 3 messages"})
		tt.Commit()
		tt.Text(control.TextAnswer, "all done")
		tt.EndText()
		iter = 1
		tt.Message(controltest.Assistant("all done"), &iter, &control.LineUsage{InputTokens: 1000, OutputTokens: 5, TokensReported: true})
		tt.Commit()
		return control.Reply{Stop: control.StopEndTurn, Usage: control.Spent{InputTokens: 1900, OutputTokens: 25}, ModelCalls: 2, ToolCalls: 1, Context: control.ContextSize{Used: 1000, Size: 200000}}
	})
	state := rig.open(t, "first-try")
	ui := &recordingTurnUI{}
	code, err := runRemoteTurn(context.Background(), rig.config, state, messages.ChatMessage{Role: "user", Content: "look around"}, nil, nil, ui)
	if err != nil || code != 0 {
		t.Fatalf("turn = %d, %v", code, err)
	}
	if got := ui.text.String(); got != "let me lookall done" {
		t.Fatalf("answer text = %q (the newline after reasoning is trimmed)", got)
	}
	if ui.thinking.String() != "thinking it over" {
		t.Fatalf("thinking = %q", ui.thinking.String())
	}
	if len(ui.starts) != 1 || ui.starts[0].Arguments != `{"command":"echo hi"}` || ui.starts[0].DisplayID != "d1" {
		t.Fatalf("tool starts = %+v (the call's whole arguments come from its reply)", ui.starts)
	}
	if len(ui.ends) != 1 || ui.ends[0] != "bash:ok:hi" || len(ui.results) != 1 {
		t.Fatalf("tool ends = %v, results %d", ui.ends, len(ui.results))
	}
	if len(ui.notices) != 1 || ui.notices[0] != "compacted 3 messages" {
		t.Fatalf("notices = %v", ui.notices)
	}
	if ui.in != 1000 || ui.out != 25 || ui.used != 1000 {
		t.Fatalf("tokens = %d in, %d out, %d used", ui.in, ui.out, ui.used)
	}
	if ui.completion == nil || ui.completion.Reason != messages.StopReasonEndTurn || ui.completion.Err != nil {
		t.Fatalf("completion = %+v", ui.completion)
	}
	// The session was new: the prompt made it under the id drawn here and
	// the name the launch gave.
	prompts := rig.srv.Prompts()
	if len(prompts) != 1 || prompts[0].Model != "openai/test-model" || prompts[0].MaxIterations != 8 {
		t.Fatalf("prompts = %+v", prompts)
	}
	var content struct{ Role, Content string }
	_ = json.Unmarshal(prompts[0].Content, &content)
	if content.Role != "user" || content.Content != "look around" {
		t.Fatalf("prompt content = %s", prompts[0].Content)
	}
	sess := rig.srv.ByName("first-try")
	if sess == nil || sess.ID != state.remote.session.ID() || state.remote.session.Pending() {
		t.Fatalf("session made = %+v", sess)
	}
	// Its transcript reads back through the store.
	history, err := state.session.GetHistory(context.Background())
	if err != nil || len(history) != 4 || history[2].ToolCallID != "call_1" {
		t.Fatalf("history = %d messages, %v", len(history), err)
	}
	// A second turn names the session by its id.
	if _, err := runRemoteTurn(context.Background(), rig.config, state, messages.ChatMessage{Role: "user", Content: "again"}, nil, nil, &recordingTurnUI{}); err != nil {
		t.Fatal(err)
	}
	if p := rig.srv.Prompts(); len(p) != 2 || p[1].Session.ID != state.remote.session.ID() {
		t.Fatalf("second prompt = %+v", p[1].Session)
	}
}

func TestBackendTurnCancelStopsItInTheBackend(t *testing.T) {
	started := make(chan struct{})
	rig := newBackendRig(t, func(ctx context.Context, tt *controltest.Turn) control.Reply {
		tt.Text(control.TextAnswer, "working")
		close(started)
		<-ctx.Done()
		return control.Reply{Stop: control.StopCancelled}
	})
	state := rig.open(t, "stoppable")
	ctx, cancel := context.WithCancelCause(context.Background())
	errInterrupt := errors.New("interrupted by the user")
	go func() {
		<-started
		cancel(errInterrupt)
	}()
	ui := &recordingTurnUI{}
	code, err := runRemoteTurn(ctx, rig.config, state, messages.ChatMessage{Role: "user", Content: "go"}, nil, nil, ui)
	if code != 130 || !errors.Is(err, errInterrupt) {
		t.Fatalf("turn = %d, %v", code, err)
	}
	stops := rig.srv.Stops()
	if len(stops) == 0 || stops[0].Agent != state.remote.session.ID() {
		t.Fatalf("stops = %+v", stops)
	}
	if ui.text.String() != "working" {
		t.Fatalf("text = %q", ui.text.String())
	}
}

func TestBackendTurnStoppedElsewhereExits130(t *testing.T) {
	rig := newBackendRig(t, func(ctx context.Context, tt *controltest.Turn) control.Reply {
		return control.Reply{Stop: control.StopCancelled}
	})
	state := rig.open(t, "elsewhere")
	code, err := runRemoteTurn(context.Background(), rig.config, state, messages.ChatMessage{Role: "user", Content: "go"}, nil, nil, &recordingTurnUI{})
	if code != 130 || !errors.Is(err, errStoppedElsewhere) {
		t.Fatalf("turn = %d, %v", code, err)
	}
}

func TestBackendTurnCatchesUpAfterItsConnectionEnds(t *testing.T) {
	var rig *backendRig
	rig = newBackendRig(t, func(ctx context.Context, tt *controltest.Turn) control.Reply {
		tt.Text(control.TextAnswer, "before the drop")
		tt.EndText()
		iter := uint32(0)
		tt.Message(controltest.Assistant("before the drop"), &iter, nil)
		tt.Commit()
		// The daemon drops the connection as one that fell behind, and the
		// turn goes on: what it keeps from here is read from the store.
		rig.srv.DropConnections(true)
		time.Sleep(100 * time.Millisecond)
		tt.Text(control.TextAnswer, "said after")
		tt.EndText()
		tt.Message(controltest.Assistant("said after"), nil, nil)
		tt.Commit()
		time.Sleep(200 * time.Millisecond)
		return control.Reply{Stop: control.StopEndTurn}
	})
	state := rig.open(t, "dropped")
	ui := &recordingTurnUI{}
	code, err := runRemoteTurn(context.Background(), rig.config, state, messages.ChatMessage{Role: "user", Content: "go"}, nil, nil, ui)
	if err != nil || code != 0 {
		t.Fatalf("turn = %d, %v (notices %v)", code, err, ui.notices)
	}
	if !strings.Contains(ui.text.String(), "before the drop") || !strings.Contains(ui.text.String(), "said after") {
		t.Fatalf("text = %q", ui.text.String())
	}
	if len(ui.notices) == 0 || !strings.Contains(ui.notices[0], "fell behind") {
		t.Fatalf("notices = %v", ui.notices)
	}
}

func TestBackendTurnRefusedAsBusyNamesAttach(t *testing.T) {
	rig := newBackendRig(t, nil)
	rig.srv.Add(&controltest.Session{Name: "busy-one", Running: true})
	state := rig.open(t, "busy-one")
	_, err := runRemoteTurn(context.Background(), rig.config, state, messages.ChatMessage{Role: "user", Content: "go"}, nil, nil, &recordingTurnUI{})
	if err == nil || !strings.Contains(err.Error(), "--attach busy-one") {
		t.Fatalf("busy prompt = %v", err)
	}
}

func TestBackendConfigFailsClosed(t *testing.T) {
	// Nothing of the user's own: no saved setup, no exported policy.
	t.Setenv("HOME", t.TempDir())
	for _, name := range []string{envVarSandbox, envVarBackend, envVarWorkspace} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	parse := func(args ...string) error {
		cmd := getCommand()
		var got error
		cmd.Action = func(ctx context.Context, c *cli.Command) error {
			got = validateBackendConfig(c, parseConfig(c))
			return nil
		}
		if err := cmd.Run(context.Background(), normalizeCommandArgs(append([]string{"polly"}, args...))); err != nil {
			return err
		}
		return got
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--backend", "unix:///tmp/x.sock", "--sandbox", "workspace"}, "polly's sandbox does not reach"},
		{[]string{"--backend", "unix:///tmp/x.sock", "--writepath", "/tmp"}, "polly's sandbox does not reach"},
		{[]string{"--backend", "unix:///tmp/x.sock", "--confirm"}, "--confirm"},
		{[]string{"--backend", "unix:///tmp/x.sock", "--delete", "x"}, "--delete"},
		{[]string{"--backend", "unix:///tmp/x.sock", "-m", "gemini/flash"}, "not served by the control backend"},
		{[]string{"--backend", "ssh://box/sock"}, "not supported"},
		{[]string{"--workspace", "ssh:box"}, "pass --backend"},
		{[]string{"--attach"}, "pass --backend"},
		{[]string{"--backend", "unix:///tmp/x.sock", "--attach", "--stop", "x"}, "one or the other"},
	} {
		err := parse(tc.args...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: %v, want %q", tc.args, err, tc.want)
		}
	}
	for _, ok := range [][]string{
		{"--backend", "unix:///tmp/x.sock", "-m", "anthropic/claude-opus-5"},
		{"--backend", "unix:///tmp/x.sock", "--list"},
		{"--backend", "exec://sh", "--attach"},
		{"--backend", "unix:///tmp/x.sock", "--stop", "kept-one"},
	} {
		if err := parse(ok...); err != nil {
			t.Errorf("%v: %v", ok, err)
		}
	}
}

func TestBackendSessionRefusesInProcessCommands(t *testing.T) {
	rig := newBackendRig(t, nil)
	state := rig.open(t, "commands")
	var replies []string
	ctx := &replCommandContext{state: state, reply: func(line string) error {
		replies = append(replies, line)
		return nil
	}}
	for _, line := range []string{"/compact", "/sandbox", "/rename x", "/clear", "/spawn look"} {
		replies = nil
		handled, _, err := defaultReplCommands.dispatch(line, ctx)
		if !handled || err != nil || len(replies) != 1 || !strings.Contains(replies[0], "not available on a --backend session") {
			t.Fatalf("%s: handled %v, err %v, replies %v", line, handled, err, replies)
		}
	}
}

// The headless TUI paints on process-wide screen state, so it runs in a
// process of its own: this test runs itself again as that process.
const backendTUIChildVar = "POLLY_TEST_BACKEND_TUI_CHILD"

func TestBackendHeadlessTUIRunsATurnAndListsSessions(t *testing.T) {
	if os.Getenv(backendTUIChildVar) != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestBackendHeadlessTUIRunsATurnAndListsSessions$", "-test.count=1")
		cmd.Env = append(os.Environ(), backendTUIChildVar+"=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("the headless TUI run failed: %v\n%s", err, out)
		}
		return
	}
	rig := newBackendRig(t, controltest.Say("all is well here"))
	rig.srv.Add(&controltest.Session{Name: "older-one", Model: "openai/test-model", LastUsed: time.Now().Add(-time.Hour), Lines: []json.RawMessage{json.RawMessage(`{"role":"user","content":"long ago"}`)}})
	rig.opener.followRunning = true
	scriptPath := filepath.Join(t.TempDir(), "script.txt")
	if err := os.WriteFile(scriptPath, []byte(":ready 20\nhow is it\n:wait \"all is well here\" 20\n/compact\n:wait \"not available\" 10\n/sessions\n:wait \"older-one\" 10\n:key esc\n:quit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script, err := loadHeadlessRun(scriptPath, 100, 30, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	state := rig.open(t, "tui-one")
	opener := &sessionOpener{prepare: rig.opener.prepare, open: rig.opener.open, newName: func(ctx context.Context) (string, error) {
		return generateSessionName(ctx, rig.backend.store)
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := runManagedREPL(ctx, rig.config, openResult{name: "tui-one", store: rig.backend.store, state: state}, opener, script); err != nil {
		t.Fatal(err)
	}
	if len(rig.srv.Prompts()) != 1 {
		t.Fatalf("prompts = %d", len(rig.srv.Prompts()))
	}
}

func TestBackendTurnTellsLinesItMissedWhileAway(t *testing.T) {
	var rig *backendRig
	dropped := make(chan struct{})
	rig = newBackendRig(t, func(ctx context.Context, tt *controltest.Turn) control.Reply {
		// The connection is gone before these are told: they are the
		// store's to read once the launch is back.
		rig.srv.DropConnections(false)
		close(dropped)
		iter := uint32(0)
		tt.Message(controltest.Assistant("kept while away"), &iter, nil)
		tt.Commit()
		time.Sleep(300 * time.Millisecond)
		return control.Reply{Stop: control.StopEndTurn}
	})
	state := rig.open(t, "away")
	ui := &recordingTurnUI{}
	code, err := runRemoteTurn(context.Background(), rig.config, state, messages.ChatMessage{Role: "user", Content: "go"}, nil, nil, ui)
	if err != nil || code != 0 {
		t.Fatalf("turn = %d, %v", code, err)
	}
	if !strings.Contains(ui.text.String(), "kept while away") {
		t.Fatalf("text = %q, notices %v", ui.text.String(), ui.notices)
	}
}
