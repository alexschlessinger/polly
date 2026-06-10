package main

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
)

type passthroughSandbox struct{}

func (passthroughSandbox) Wrap(cmd *exec.Cmd) error {
	return nil
}

func TestReplCommandRegistryDispatchAliasesAndHelp(t *testing.T) {
	registry := newReplCommandRegistry()
	ran := false
	registry.register(replCommand{
		name:    "/one",
		aliases: []string{"/uno"},
		usage:   "/one",
		summary: "run one",
		run: func(ctx *replCommandContext, args []string) replCommandResult {
			ran = true
			if len(args) != 2 || args[1] != "arg" {
				t.Fatalf("args = %v, want [/uno arg]", args)
			}
			return replCommandResult{}
		},
	})

	handled, quit, err := registry.dispatch("/uno arg", &replCommandContext{registry: registry})
	if err != nil {
		t.Fatalf("dispatch error = %v", err)
	}
	if !handled || quit || !ran {
		t.Fatalf("handled=%v quit=%v ran=%v, want handled non-quit execution", handled, quit, ran)
	}

	handled, _, err = registry.dispatch("/missing", &replCommandContext{registry: registry})
	if err != nil {
		t.Fatalf("unknown dispatch error = %v", err)
	}
	if handled {
		t.Fatal("unknown command should not be handled")
	}

	help := strings.Join(registry.helpLines(), "\n")
	if !strings.Contains(help, "/one, /uno") || !strings.Contains(help, "run one") {
		t.Fatalf("help missing registered command: %q", help)
	}
}

func TestGetCommandShowsStableFlagBackedSettings(t *testing.T) {
	r := newManagedREPL(&Config{
		Settings: Settings{
			Model:            "openai/gpt-5.4",
			Temperature:      0.7,
			MaxTokens:        1234,
			MaxHistoryTokens: 5678,
			ThinkingEffort:   "medium",
			SystemPrompt:     "be useful",
			ToolTimeout:      3 * time.Second,
			SkillDirs:        []string{"/tmp/skills"},
		},
	}, "ctx", 0, 0)

	if handled, quit := r.runCommand("/get model"); !handled || quit {
		t.Fatalf("/get model handled=%v quit=%v", handled, quit)
	}
	if got := strings.Join(r.model.transcript, "\n"); !strings.Contains(got, "model: openai/gpt-5.4") {
		t.Fatalf("/get model output = %q", got)
	}

	r.model.transcript = nil
	if handled, quit := r.runCommand("/get all"); !handled || quit {
		t.Fatalf("/get all handled=%v quit=%v", handled, quit)
	}
	got := strings.Join(r.model.transcript, "\n")
	for _, want := range []string{"settings:", "temp: 0.70", "maxtokens: 1234", "tooltimeout: 3s", "skilldir: /tmp/skills"} {
		if !strings.Contains(got, want) {
			t.Fatalf("/get all missing %q in %q", want, got)
		}
	}

	r.model.transcript = nil
	r.runCommand("/get unknown")
	if got := strings.Join(r.model.transcript, "\n"); !strings.Contains(got, "unknown key: unknown") {
		t.Fatalf("/get unknown output = %q", got)
	}
}

func TestToolsCommandListNamespaceAndShow(t *testing.T) {
	registry := tools.NewToolRegistry([]tools.Tool{
		&tools.Func{Name: "git__status", Desc: "Show git status", Params: schema.Params{"path": schema.S("Repository path")}, Required: []string{"path"}},
		&tools.Func{Name: "git__diff", Desc: "Show git diff"},
		tools.NewBashTool(""),
	})
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.state = &conversationState{toolRegistry: registry}

	if handled, quit := r.runCommand("/tools list git"); !handled || quit {
		t.Fatalf("/tools list handled=%v quit=%v", handled, quit)
	}
	got := strings.Join(r.model.transcript, "\n")
	if !strings.Contains(got, "git__status") || !strings.Contains(got, "git__diff") || strings.Contains(got, "bash") {
		t.Fatalf("/tools list git output = %q", got)
	}

	r.model.transcript = nil
	if handled, quit := r.runCommand("/tools show git__status"); !handled || quit {
		t.Fatalf("/tools show handled=%v quit=%v", handled, quit)
	}
	got = strings.Join(r.model.transcript, "\n")
	for _, want := range []string{"name: git__status", "type: native", "source: builtin", "description: Show git status", "required: path"} {
		if !strings.Contains(got, want) {
			t.Fatalf("/tools show missing %q in %q", want, got)
		}
	}

	r.model.transcript = nil
	r.runCommand("/tools show missing")
	if got := strings.Join(r.model.transcript, "\n"); !strings.Contains(got, "tool not found: missing") {
		t.Fatalf("/tools show missing output = %q", got)
	}
}

// stubSandboxRegistry returns a registry whose factory hands out no-op
// sandboxes, with bash loaded (and therefore sandboxed).
func stubSandboxRegistry(t *testing.T) *tools.ToolRegistry {
	t.Helper()
	factory := func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		return probeFailSandbox{}, nil
	}
	registry := tools.NewToolRegistry(nil, tools.WithSandboxFactory(factory, sandbox.Config{}))
	if _, err := registry.LoadToolAuto("bash"); err != nil {
		t.Fatalf("LoadToolAuto(bash) error = %v", err)
	}
	return registry
}

func TestGetSandboxSetting(t *testing.T) {
	r := newManagedREPL(&Config{NoSandbox: true}, "ctx", 0, 0)
	if handled, quit := r.runCommand("/get sandbox"); !handled || quit {
		t.Fatalf("/get sandbox handled=%v quit=%v", handled, quit)
	}
	if got := strings.Join(r.model.transcript, "\n"); !strings.Contains(got, "sandbox: disabled (--nosandbox)") {
		t.Fatalf("/get sandbox output = %q", got)
	}

	registry := stubSandboxRegistry(t)
	registry.Register(&tools.Func{Name: "plain", Desc: "no sandbox support"})
	r = newManagedREPL(&Config{}, "ctx", 0, 0)
	r.state = &conversationState{toolRegistry: registry}
	r.runCommand("/get sandbox")
	got := strings.Join(r.model.transcript, "\n")
	if !strings.Contains(got, "active") || !strings.Contains(got, "1 sandboxed, 0 not") {
		t.Fatalf("/get sandbox output = %q", got)
	}

	r.model.transcript = nil
	r.runCommand("/get all")
	if got := strings.Join(r.model.transcript, "\n"); !strings.Contains(got, "sandbox: active") {
		t.Fatalf("/get all missing sandbox row: %q", got)
	}
}

func TestToolsListMarksSandboxed(t *testing.T) {
	registry := stubSandboxRegistry(t)
	registry.Register(&tools.Func{Name: "plain", Desc: "no sandbox support"})
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.state = &conversationState{toolRegistry: registry}

	if handled, quit := r.runCommand("/tools list"); !handled || quit {
		t.Fatalf("/tools list handled=%v quit=%v", handled, quit)
	}
	got := strings.Join(r.model.transcript, "\n")
	if !strings.Contains(got, "bash [sandboxed: net off, temp writes, env filtered]") {
		t.Fatalf("/tools list missing sandbox details for bash: %q", got)
	}
	if strings.Contains(got, "plain [sandboxed]") {
		t.Fatalf("/tools list wrongly marked plain tool: %q", got)
	}

	r.model.transcript = nil
	if handled, quit := r.runCommand("/tools show bash"); !handled || quit {
		t.Fatalf("/tools show handled=%v quit=%v", handled, quit)
	}
	got = strings.Join(r.model.transcript, "\n")
	for _, want := range []string{
		"sandboxed: true",
		"sandbox: network off; writes limited to temp; env filters credential-like variables",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("/tools show missing %q in %q", want, got)
		}
	}
}

func TestToolsSandboxBadges(t *testing.T) {
	tests := []struct {
		name string
		cfg  sandbox.Config
		want string
	}{
		{
			name: "network allowed",
			cfg:  sandbox.Config{AllowNetwork: true},
			want: "bash [sandboxed: net on, temp writes, env filtered]",
		},
		{
			name: "dns denied",
			cfg:  sandbox.Config{AllowNetwork: true, DenyDNS: true},
			want: "bash [sandboxed: net on, dns off, temp writes, env filtered]",
		},
		{
			name: "read only",
			cfg:  sandbox.Config{DenyWrite: true},
			want: "bash [sandboxed: net off, read-only, env filtered]",
		},
		{
			name: "custom writes",
			cfg:  sandbox.Config{WritablePaths: []string{"/tmp", "/workspace/out"}},
			want: "bash [sandboxed: net off, temp+custom writes, env filtered]",
		},
		{
			name: "env allowlist",
			cfg:  sandbox.Config{AllowEnv: []string{"PATH"}},
			want: "bash [sandboxed: net off, temp writes, env allowlist]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			factory := func(cfg sandbox.Config) (sandbox.Sandbox, error) {
				return probeFailSandbox{}, nil
			}
			registry := tools.NewToolRegistry(nil, tools.WithSandboxFactory(factory, tt.cfg))
			if _, err := registry.LoadToolAuto("bash"); err != nil {
				t.Fatalf("LoadToolAuto(bash) error = %v", err)
			}
			r := newManagedREPL(&Config{}, "ctx", 0, 0)
			r.state = &conversationState{toolRegistry: registry}
			if handled, quit := r.runCommand("/tools list"); !handled || quit {
				t.Fatalf("/tools list handled=%v quit=%v", handled, quit)
			}
			if got := strings.Join(r.model.transcript, "\n"); !strings.Contains(got, tt.want) {
				t.Fatalf("/tools list missing %q in %q", tt.want, got)
			}
		})
	}
}

func TestToolsSandboxOptOutAndFallbackBadges(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/bash
if [ "$1" = "--schema" ]; then
	echo '{"title":"unsandboxed_tool","description":"Opts out","type":"object","sandbox":false,"properties":{}}'
elif [ "$1" = "--execute" ]; then
	echo "ok"
fi
`
	scriptPath := filepath.Join(dir, "unsandboxed.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	factory := func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		return passthroughSandbox{}, nil
	}
	registry := tools.NewToolRegistry(nil, tools.WithSandboxFactory(factory, sandbox.Config{}))
	if _, err := registry.LoadShellTool(scriptPath); err != nil {
		t.Fatalf("LoadShellTool() error = %v", err)
	}
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.state = &conversationState{toolRegistry: registry}
	if handled, quit := r.runCommand("/tools list"); !handled || quit {
		t.Fatalf("/tools list handled=%v quit=%v", handled, quit)
	}
	got := strings.Join(r.model.transcript, "\n")
	if !strings.Contains(got, "unsandboxed__unsandboxed_tool [not sandboxed: opted out]") {
		t.Fatalf("/tools list missing opt-out badge: %q", got)
	}

	registry = tools.NewToolRegistry([]tools.Tool{tools.NewBashTool("").WithSandbox(probeFailSandbox{})})
	r = newManagedREPL(&Config{}, "ctx", 0, 0)
	r.state = &conversationState{toolRegistry: registry}
	if handled, quit := r.runCommand("/tools list"); !handled || quit {
		t.Fatalf("/tools list handled=%v quit=%v", handled, quit)
	}
	got = strings.Join(r.model.transcript, "\n")
	if !strings.Contains(got, "bash [sandboxed]") || strings.Contains(got, "bash [sandboxed:") {
		t.Fatalf("/tools list should fall back to simple sandbox badge without config: %q", got)
	}
}

func TestSandboxNoticeLine(t *testing.T) {
	if got := sandboxNoticeLine(&Config{NoSandbox: true}, nil); got != "sandbox: disabled (--nosandbox)" {
		t.Fatalf("disabled notice = %q", got)
	}
	if got := sandboxNoticeLine(&Config{}, nil); got != "sandbox: unavailable" {
		t.Fatalf("nil-state notice = %q", got)
	}

	registry := stubSandboxRegistry(t)
	state := &conversationState{toolRegistry: registry}
	if got := sandboxNoticeLine(&Config{}, state); got != "sandbox: active (1 tools sandboxed)" {
		t.Fatalf("active notice = %q", got)
	}

	// An unsandboxed-but-capable tool is called out by name.
	factory := func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		return probeFailSandbox{}, nil
	}
	registry = tools.NewToolRegistry([]tools.Tool{tools.NewBashTool("")},
		tools.WithSandboxFactory(factory, sandbox.Config{}))
	state = &conversationState{toolRegistry: registry}
	got := sandboxNoticeLine(&Config{}, state)
	if !strings.Contains(got, "0 tools sandboxed") || !strings.Contains(got, "not sandboxed: bash") {
		t.Fatalf("opt-out notice = %q", got)
	}
}

func TestCompleteSlashSubcommands(t *testing.T) {
	cases := []struct {
		in            string
		wantOK        bool
		wantCompleted string
		wantMatches   []string
	}{
		{"/g", true, "/get", []string{"/get"}},
		{"/get model", true, "/get model", []string{"/get model"}},
		{"/get max", true, "/get max", []string{"/get maxcontext", "/get maxtokens"}},
		{"/queue d", true, "/queue drop", []string{"/queue drop"}},
		{"/queue c", true, "/queue c", []string{"/queue clear", "/queue continue"}},
		{"/tools s", true, "/tools show", []string{"/tools show"}},
		{"/help me", false, "", nil},
	}
	for _, c := range cases {
		completed, matches, ok := completeSlash(c.in)
		if ok != c.wantOK {
			t.Errorf("completeSlash(%q) ok=%v, want %v", c.in, ok, c.wantOK)
			continue
		}
		if completed != c.wantCompleted {
			t.Errorf("completeSlash(%q) completed=%q, want %q", c.in, completed, c.wantCompleted)
		}
		if strings.Join(matches, ",") != strings.Join(c.wantMatches, ",") {
			t.Errorf("completeSlash(%q) matches=%v, want %v", c.in, matches, c.wantMatches)
		}
	}
}

func dispatchDefaultCommandForTest(t *testing.T, line string, ctx *replCommandContext) []string {
	t.Helper()
	var replies []string
	ctx.reply = func(line string) error {
		replies = append(replies, line)
		return nil
	}
	handled, quit, err := defaultReplCommands.dispatch(line, ctx)
	if err != nil {
		t.Fatalf("dispatch(%q) error = %v", line, err)
	}
	if !handled || quit {
		t.Fatalf("dispatch(%q) handled=%v quit=%v, want handled non-quit", line, handled, quit)
	}
	return replies
}

func TestClearCommandOnlyClearsDisplay(t *testing.T) {
	store := sessions.NewSyncMapSessionStore(nil)
	session, err := store.Get("clear-display")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if err := session.AddMessage(messages.ChatMessage{Role: messages.MessageRoleUser, Content: "keep me"}); err != nil {
		t.Fatalf("AddMessage() error = %v", err)
	}

	cleared := false
	ctx := &replCommandContext{
		state: &conversationState{session: session},
		clearTranscript: func() error {
			cleared = true
			return nil
		},
	}
	replies := dispatchDefaultCommandForTest(t, "/clear", ctx)
	if !cleared {
		t.Fatal("/clear did not invoke clearTranscript")
	}
	if got := len(session.GetHistory()); got != 1 {
		t.Fatalf("/clear changed durable history; got %d messages, want 1", got)
	}
	if got := strings.Join(replies, "\n"); got != "display cleared" {
		t.Fatalf("/clear replies = %q, want display cleared", got)
	}

	replies = dispatchDefaultCommandForTest(t, "/clear", &replCommandContext{})
	if got := strings.Join(replies, "\n"); got != "display clear unavailable" {
		t.Fatalf("unavailable /clear replies = %q", got)
	}
}

func TestResetCommandRequiresLiteralConfirmation(t *testing.T) {
	resetCalls := 0
	ctx := &replCommandContext{
		resetConversation: func() error {
			resetCalls++
			return nil
		},
	}
	for _, line := range []string{"/reset", "/reset yes", "/reset CONFIRM", "/reset confirm now"} {
		replies := dispatchDefaultCommandForTest(t, line, ctx)
		if resetCalls != 0 {
			t.Fatalf("%q invoked reset without literal confirmation", line)
		}
		if got := strings.Join(replies, "\n"); !strings.Contains(got, "/reset confirm") {
			t.Fatalf("%q reply = %q, want confirmation instruction", line, got)
		}
	}

	replies := dispatchDefaultCommandForTest(t, "/reset confirm", ctx)
	if resetCalls != 1 {
		t.Fatalf("confirmed reset calls = %d, want 1", resetCalls)
	}
	if got := strings.Join(replies, "\n"); got != "conversation reset" {
		t.Fatalf("confirmed reset reply = %q", got)
	}

	replies = dispatchDefaultCommandForTest(t, "/reset confirm", &replCommandContext{})
	if got := strings.Join(replies, "\n"); got != "conversation reset unavailable" {
		t.Fatalf("unavailable reset reply = %q", got)
	}
}

func TestQueueCommandLifecycle(t *testing.T) {
	queue := []string{"first", "second\nline"}
	continued := 0
	ctx := &replCommandContext{
		queueLines: func() []string {
			return append([]string(nil), queue...)
		},
		// /queue drop intentionally removes the newest/last item, not the next
		// item that would run from the front of the queue.
		dropQueued: func() (string, bool) {
			if len(queue) == 0 {
				return "", false
			}
			last := len(queue) - 1
			line := queue[last]
			queue = queue[:last]
			return line, true
		},
		clearQueue: func() int {
			count := len(queue)
			queue = nil
			return count
		},
		continueQueue: func() error {
			continued++
			return nil
		},
	}

	replies := dispatchDefaultCommandForTest(t, "/queue", ctx)
	got := strings.Join(replies, "\n")
	for _, want := range []string{"queue (2):", "1. first", "2. second ↵ line"} {
		if !strings.Contains(got, want) {
			t.Fatalf("/queue list missing %q in %q", want, got)
		}
	}

	replies = dispatchDefaultCommandForTest(t, "/queue drop", ctx)
	if len(queue) != 1 || queue[0] != "first" {
		t.Fatalf("/queue drop removed wrong item; queue = %v", queue)
	}
	if got := strings.Join(replies, "\n"); !strings.Contains(got, "dropped newest queued input: second ↵ line") {
		t.Fatalf("/queue drop reply = %q", got)
	}

	replies = dispatchDefaultCommandForTest(t, "/queue clear", ctx)
	if len(queue) != 0 {
		t.Fatalf("/queue clear left queue = %v", queue)
	}
	if got := strings.Join(replies, "\n"); got != "cleared 1 queued input(s)" {
		t.Fatalf("/queue clear reply = %q", got)
	}

	replies = dispatchDefaultCommandForTest(t, "/queue list", ctx)
	if got := strings.Join(replies, "\n"); got != "queue empty" {
		t.Fatalf("empty /queue list reply = %q", got)
	}
	replies = dispatchDefaultCommandForTest(t, "/queue drop", ctx)
	if got := strings.Join(replies, "\n"); got != "queue empty" {
		t.Fatalf("empty /queue drop reply = %q", got)
	}
	replies = dispatchDefaultCommandForTest(t, "/queue clear", ctx)
	if got := strings.Join(replies, "\n"); got != "queue already empty" {
		t.Fatalf("empty /queue clear reply = %q", got)
	}

	replies = dispatchDefaultCommandForTest(t, "/queue continue", ctx)
	if continued != 1 {
		t.Fatalf("/queue continue calls = %d, want 1", continued)
	}
	if got := strings.Join(replies, "\n"); got != "queue continued" {
		t.Fatalf("/queue continue reply = %q", got)
	}

	replies = dispatchDefaultCommandForTest(t, "/queue wat", ctx)
	if got := strings.Join(replies, "\n"); !strings.HasPrefix(got, "usage: /queue") {
		t.Fatalf("invalid /queue reply = %q", got)
	}
}

func TestQueueCommandsGracefullyReportUnavailableCallbacks(t *testing.T) {
	cases := map[string]string{
		"/queue":          "queue unavailable",
		"/queue drop":     "queue drop unavailable",
		"/queue clear":    "queue clear unavailable",
		"/queue continue": "queue continue unavailable",
	}
	for line, want := range cases {
		replies := dispatchDefaultCommandForTest(t, line, &replCommandContext{})
		if got := strings.Join(replies, "\n"); got != want {
			t.Errorf("%s reply = %q, want %q", line, got, want)
		}
	}
}

func TestRetryCommandUsesCallbackAndReportsUnavailable(t *testing.T) {
	retryCalls := 0
	ctx := &replCommandContext{
		retryTurn: func() error {
			retryCalls++
			return nil
		},
	}
	replies := dispatchDefaultCommandForTest(t, "/retry", ctx)
	if retryCalls != 1 {
		t.Fatalf("/retry calls = %d, want 1", retryCalls)
	}
	if got := strings.Join(replies, "\n"); got != "retrying last turn" {
		t.Fatalf("/retry reply = %q", got)
	}

	replies = dispatchDefaultCommandForTest(t, "/retry", &replCommandContext{})
	if got := strings.Join(replies, "\n"); got != "retry unavailable" {
		t.Fatalf("unavailable /retry reply = %q", got)
	}
	replies = dispatchDefaultCommandForTest(t, "/retry now", ctx)
	if got := strings.Join(replies, "\n"); got != "usage: /retry" {
		t.Fatalf("invalid /retry reply = %q", got)
	}
}

func TestLifecycleCommandsAppearInHelp(t *testing.T) {
	help := strings.Join(defaultReplCommands.helpLines(), "\n")
	for _, want := range []string{
		"/clear",
		"clear the display (keep conversation history)",
		"/queue",
		"/reset",
		"/retry",
	} {
		if !strings.Contains(help, want) {
			t.Fatalf("help missing %q in %q", want, help)
		}
	}
}

func TestFallbackREPLDispatchesRegistryCommands(t *testing.T) {
	store := sessions.NewSyncMapSessionStore(nil)
	session, err := store.Get("fallback")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	session.AddMessage(messages.ChatMessage{Role: messages.MessageRoleUser, Content: "hi"})
	state := &conversationState{session: session, toolRegistry: tools.NewToolRegistry(nil)}
	config := &Config{Settings: Settings{Model: "anthropic/claude-sonnet-4-6"}}
	var out bytes.Buffer
	reader := bufio.NewReader(strings.NewReader("/get model\n/context\n/reset confirm\n/exit\n"))
	err = runREPLLoopWithCommands(reader, &out, newWriterReplCommandContext(config, state, &out), func(prompt string) error {
		t.Fatalf("runTurn should not be called for command %q", prompt)
		return nil
	})
	if err != nil {
		t.Fatalf("runREPLLoopWithCommands() error = %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "model: anthropic/claude-sonnet-4-6") || !strings.Contains(got, "context: fallback") || !strings.Contains(got, "conversation reset") {
		t.Fatalf("fallback command output = %q", got)
	}
	if len(session.GetHistory()) != 0 {
		t.Fatalf("fallback /reset confirm did not clear durable history: %#v", session.GetHistory())
	}
}

func TestFallbackREPLReportsUnknownSlashCommand(t *testing.T) {
	var out bytes.Buffer
	reader := bufio.NewReader(strings.NewReader("/bogus\n"))
	err := runREPLLoopWithCommands(reader, &out, newWriterReplCommandContext(nil, nil, &out), func(prompt string) error {
		t.Fatalf("runTurn should not be called for unknown slash command %q", prompt)
		return nil
	})
	if err != nil {
		t.Fatalf("runREPLLoopWithCommands() error = %v", err)
	}
	if got := out.String(); !strings.Contains(got, "unknown command: /bogus") {
		t.Fatalf("unknown slash output = %q", got)
	}
}

func TestFallbackREPLRecoversFromCancelledTurn(t *testing.T) {
	// A per-turn cancellation (context.Canceled from the turn, but parent
	// context still alive) must show an error and keep the loop open, not
	// exit the REPL.
	var out bytes.Buffer
	reader := bufio.NewReader(strings.NewReader("prompt1\nprompt2\n/exit\n"))
	callCount := 0
	err := runREPLLoopWithCommands(reader, &out, newWriterReplCommandContext(nil, nil, &out), func(prompt string) error {
		callCount++
		if callCount == 1 {
			return fmt.Errorf("cancelled")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("runREPLLoopWithCommands() error = %v", err)
	}
	if callCount != 2 {
		t.Fatalf("expected 2 calls to runTurn, got %d", callCount)
	}
	got := out.String()
	if !strings.Contains(got, "Error: cancelled") {
		t.Fatalf("expected 'Error: cancelled' in output, got %q", got)
	}
}
