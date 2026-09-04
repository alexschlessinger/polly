package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/schema"
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
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.state = &conversationState{settings: Settings{
		Model:            "openai/gpt-5.4",
		Temperature:      0.7,
		MaxTokens:        1234,
		MaxHistoryTokens: 5678,
		ThinkingEffort:   "medium",
		SystemPrompt:     "be useful",
		ToolTimeout:      3 * time.Second,
		SkillDirs:        []string{"/tmp/skills"},
	}}

	if handled, quit := r.runCommand("/get model"); !handled || quit {
		t.Fatalf("/get model handled=%v quit=%v", handled, quit)
	}
	if got := strings.Join(transcriptTexts(r.model), "\n"); !strings.Contains(got, "model: openai/gpt-5.4") {
		t.Fatalf("/get model output = %q", got)
	}

	clearTranscriptForTest(r.model)
	if handled, quit := r.runCommand("/get all"); !handled || quit {
		t.Fatalf("/get all handled=%v quit=%v", handled, quit)
	}
	got := strings.Join(transcriptTexts(r.model), "\n")
	for _, want := range []string{"settings:", "temp: 0.70", "maxtokens: 1234", "tooltimeout: 3s", "skilldir: /tmp/skills"} {
		if !strings.Contains(got, want) {
			t.Fatalf("/get all missing %q in %q", want, got)
		}
	}

	clearTranscriptForTest(r.model)
	r.runCommand("/get unknown")
	if got := strings.Join(transcriptTexts(r.model), "\n"); !strings.Contains(got, "unknown key: unknown") {
		t.Fatalf("/get unknown output = %q", got)
	}
}

func TestToolsCommandListNamespaceAndShow(t *testing.T) {
	registry := tools.NewToolRegistry([]tools.Tool{
		&tools.Func{Name: "git__status", Desc: "Show git status", Params: schema.Params{"path": schema.S("Repository path")}, Required: []string{"path"}},
		&tools.Func{Name: "git__diff", Desc: "Show git diff"},
		tools.NewUnsafeBashTool(""),
	})
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.state = &conversationState{toolRegistry: registry}

	if handled, quit := r.runCommand("/tools list git"); !handled || quit {
		t.Fatalf("/tools list handled=%v quit=%v", handled, quit)
	}
	got := strings.Join(transcriptTexts(r.model), "\n")
	if !strings.Contains(got, "git__status") || !strings.Contains(got, "git__diff") || strings.Contains(got, "bash") {
		t.Fatalf("/tools list git output = %q", got)
	}

	clearTranscriptForTest(r.model)
	if handled, quit := r.runCommand("/tools show git__status"); !handled || quit {
		t.Fatalf("/tools show handled=%v quit=%v", handled, quit)
	}
	got = strings.Join(transcriptTexts(r.model), "\n")
	for _, want := range []string{"name: git__status", "type: native", "source: builtin", "description: Show git status", "required: path"} {
		if !strings.Contains(got, want) {
			t.Fatalf("/tools show missing %q in %q", want, got)
		}
	}

	clearTranscriptForTest(r.model)
	r.runCommand("/tools show missing")
	if got := strings.Join(transcriptTexts(r.model), "\n"); !strings.Contains(got, "tool not found: missing") {
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
	registry := tools.NewToolRegistry(nil,
		tools.WithSandboxFactory(factory, sandbox.Config{}),
		tools.WithUnsafeNoSandbox())
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
	if got := strings.Join(transcriptTexts(r.model), "\n"); !strings.Contains(got, "sandbox: disabled (--nosandbox)") {
		t.Fatalf("/get sandbox output = %q", got)
	}

	registry := stubSandboxRegistry(t)
	registry.Register(&tools.Func{Name: "plain", Desc: "no sandbox support"})
	r = newManagedREPL(&Config{}, "ctx", 0, 0)
	r.state = &conversationState{toolRegistry: registry}
	r.runCommand("/get sandbox")
	got := strings.Join(transcriptTexts(r.model), "\n")
	if !strings.Contains(got, "active") || !strings.Contains(got, "1 sandboxed, 0 not") {
		t.Fatalf("/get sandbox output = %q", got)
	}

	clearTranscriptForTest(r.model)
	r.runCommand("/get all")
	if got := strings.Join(transcriptTexts(r.model), "\n"); !strings.Contains(got, "sandbox: active") {
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
	got := plainStyledText(strings.Join(transcriptTexts(r.model), "\n"))
	if !strings.Contains(got, "bash [sandboxed: net off, temp writes, env filtered]") {
		t.Fatalf("/tools list missing sandbox details for bash: %q", got)
	}
	if strings.Contains(got, "plain [sandboxed]") {
		t.Fatalf("/tools list wrongly marked plain tool: %q", got)
	}

	clearTranscriptForTest(r.model)
	if handled, quit := r.runCommand("/tools show bash"); !handled || quit {
		t.Fatalf("/tools show handled=%v quit=%v", handled, quit)
	}
	got = strings.Join(transcriptTexts(r.model), "\n")
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
		{
			name: "env passthrough",
			cfg:  sandbox.Config{PassEnv: []string{"SSH_AUTH_SOCK"}},
			want: "bash [sandboxed: net off, temp writes, env filtered+pass]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.name == "custom writes" {
				home, err := os.UserHomeDir()
				if err != nil {
					t.Fatal(err)
				}
				tt.cfg.WritablePaths = []string{os.TempDir(), home}
			}
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
			if got := plainStyledText(strings.Join(transcriptTexts(r.model), "\n")); !strings.Contains(got, tt.want) {
				t.Fatalf("/tools list missing %q in %q", tt.want, got)
			}
		})
	}
}

func TestSandboxSummariesReportUnixSocketGrants(t *testing.T) {
	info := tools.SandboxInfo{
		Capable: true,
		Active:  true,
		Config:  &sandbox.Config{AllowUnixSockets: []string{"/tmp/agent.sock"}},
	}
	if got := sandboxCompactSummary(info); !strings.Contains(got, "1 unix socket(s)") {
		t.Fatalf("sandboxCompactSummary = %q, want a unix-socket token", got)
	}
	if got := sandboxShowDetail(info); !strings.Contains(got, "Unix sockets: 1 granted") {
		t.Fatalf("sandboxShowDetail = %q, want a unix-socket line", got)
	}
	info.Config = &sandbox.Config{PassEnv: []string{"SSH_AUTH_SOCK"}}
	if got := sandboxShowDetail(info); !strings.Contains(got, "passing: SSH_AUTH_SOCK") {
		t.Fatalf("sandboxShowDetail = %q, want the passed names listed", got)
	}
}

func TestSandboxNoticeReportsMissingSSHAgent(t *testing.T) {
	skipIfWindows(t)
	state := &conversationState{toolRegistry: stubSandboxRegistry(t)}
	t.Setenv("SSH_AUTH_SOCK", "/nonexistent/agent.sock")
	got := sandboxNoticeLine(&Config{SandboxPreset: "workspace+net+git+ssh"}, state)
	if !strings.Contains(got, "ssh: agent unavailable") {
		t.Fatalf("sandboxNoticeLine = %q, want an agent-unavailable hint", got)
	}
	// Without the ssh component the hint must not appear.
	got = sandboxNoticeLine(&Config{SandboxPreset: "workspace+net+git"}, state)
	if strings.Contains(got, "agent unavailable") {
		t.Fatalf("sandboxNoticeLine = %q, hint must be scoped to the ssh component", got)
	}

	// A symlinked agent path is live for the sandbox (the grant resolves
	// symlinks), so it must read as live here too.
	dir, err := os.MkdirTemp("", "pagent")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "agent.sock")
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("bind test agent socket: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	alias := filepath.Join(dir, "alias.sock")
	if err := os.Symlink(sock, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSH_AUTH_SOCK", alias)
	got = sandboxNoticeLine(&Config{SandboxPreset: "workspace+net+git+ssh"}, state)
	if strings.Contains(got, "agent unavailable") {
		t.Fatalf("sandboxNoticeLine = %q, symlinked live agent socket must not read as unavailable", got)
	}
}

func TestPreparedDefaultSandboxTempPathsAreNotCustom(t *testing.T) {
	root := t.TempDir()
	realTemp := filepath.Join(root, "real-temp")
	if err := os.Mkdir(realTemp, 0700); err != nil {
		t.Fatalf("create real temp: %v", err)
	}
	tempAlias := filepath.Join(root, "temp-alias")
	if err := os.Symlink(realTemp, tempAlias); err != nil {
		t.Skipf("create temp symlink: %v", err)
	}
	t.Setenv("TMPDIR", tempAlias)

	prepared, err := sandbox.PrepareConfig(sandbox.DefaultConfig())
	if err != nil {
		t.Fatalf("PrepareConfig(DefaultConfig()) error = %v", err)
	}
	if len(prepared.WritablePaths) == 0 {
		t.Fatal("prepared default config has no temp writable path")
	}

	tests := []struct {
		name        string
		allowNet    bool
		wantCompact string
		wantDetail  string
	}{
		{
			name:        "base",
			wantCompact: "net off, temp writes, env filtered",
			wantDetail:  "network off; writes limited to temp; env filters credential-like variables",
		},
		{
			name:        "net",
			allowNet:    true,
			wantCompact: "net on, temp writes, env filtered",
			wantDetail:  "network on; writes limited to temp; env filters credential-like variables",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := prepared
			cfg.AllowNetwork = tt.allowNet
			info := tools.SandboxInfo{Capable: true, Active: true, Config: &cfg}
			if got := sandboxCompactSummary(info); got != tt.wantCompact {
				t.Fatalf("sandboxCompactSummary() = %q, want %q", got, tt.wantCompact)
			}
			if got := sandboxShowDetail(info); got != tt.wantDetail {
				t.Fatalf("sandboxShowDetail() = %q, want %q", got, tt.wantDetail)
			}
		})
	}
}

func TestToolsSandboxOptOutAndFallbackBadges(t *testing.T) {
	skipIfWindows(t)
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
	registry := tools.NewToolRegistry(nil,
		tools.WithSandboxFactory(factory, sandbox.Config{}),
		tools.WithUnsafeNoSandbox())
	if _, err := registry.LoadShellTool(scriptPath); err != nil {
		t.Fatalf("LoadShellTool() error = %v", err)
	}
	r := newManagedREPL(&Config{}, "ctx", 0, 0)
	r.state = &conversationState{toolRegistry: registry}
	if handled, quit := r.runCommand("/tools list"); !handled || quit {
		t.Fatalf("/tools list handled=%v quit=%v", handled, quit)
	}
	got := plainStyledText(strings.Join(transcriptTexts(r.model), "\n"))
	if !strings.Contains(got, "unsandboxed__unsandboxed_tool [not sandboxed: opted out]") {
		t.Fatalf("/tools list missing opt-out badge: %q", got)
	}

	registry = tools.NewToolRegistry([]tools.Tool{tools.NewUnsafeBashTool("").WithSandbox(probeFailSandbox{})})
	r = newManagedREPL(&Config{}, "ctx", 0, 0)
	r.state = &conversationState{toolRegistry: registry}
	if handled, quit := r.runCommand("/tools list"); !handled || quit {
		t.Fatalf("/tools list handled=%v quit=%v", handled, quit)
	}
	got = plainStyledText(strings.Join(transcriptTexts(r.model), "\n"))
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
	if got := sandboxNoticeLine(&Config{}, state); got != "sandbox: active (base; 1 tools sandboxed)" {
		t.Fatalf("active notice = %q", got)
	}
	if got := sandboxNoticeLine(&Config{SandboxPreset: "workspace+net+git"}, state); got != "sandbox: active (workspace+net+git; 1 tools sandboxed)" {
		t.Fatalf("preset notice = %q", got)
	}

	// An unsandboxed-but-capable tool is called out by name.
	factory := func(cfg sandbox.Config) (sandbox.Sandbox, error) {
		return probeFailSandbox{}, nil
	}
	registry = tools.NewToolRegistry([]tools.Tool{tools.NewUnsafeBashTool("")},
		tools.WithSandboxFactory(factory, sandbox.Config{}))
	state = &conversationState{toolRegistry: registry}
	got := sandboxNoticeLine(&Config{}, state)
	if !strings.Contains(got, "0 tools sandboxed") || !strings.Contains(got, "not sandboxed: bash") {
		t.Fatalf("opt-out notice = %q", got)
	}
}

func TestWriteFallbackSandboxNotice(t *testing.T) {
	var out bytes.Buffer
	writeFallbackSandboxNotice(&out, &Config{NoSandbox: true}, nil)
	if got := out.String(); got != "sandbox: disabled (--nosandbox)\n" {
		t.Fatalf("fallback sandbox notice = %q", got)
	}

	out.Reset()
	writeFallbackSandboxNotice(&out, &Config{NoSandbox: true, Quiet: true}, nil)
	if out.Len() != 0 {
		t.Fatalf("quiet fallback wrote sandbox notice: %q", out.String())
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
		{"/tools s", true, "/tools show", []string{"/tools show"}},
		{"/thi", false, "", nil},
		{"/set th", true, "/set thinking", []string{"/set thinking"}},
		{"/set max", true, "/set max", []string{"/set maxcontext", "/set maxtokens"}},
		// Second arguments complete positionally.
		{"/set thinking m", true, "/set thinking m", []string{"/set thinking max", "/set thinking medium", "/set thinking minimal"}},
		{"/help /cl", true, "/help /cl", []string{"/help /clear", "/help /close"}},
		{"/help /cle", true, "/help /clear", []string{"/help /clear"}},
		// Keywords don't leak past their position.
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

func TestUnknownCommandNotice(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// Near misses by edit distance.
		{"/hlep", "unknown command: /hlep — did you mean /help?"},
		{"/quti", "unknown command: /quti — did you mean /quit?"},
		// Unique prefixes.
		{"/con", "unknown command: /con — did you mean /context?"},
		{"/stat", "unknown command: /stat — did you mean /stats?"},
		// Only the command token is named, not the arguments.
		{"/hlep me now", "unknown command: /hlep — did you mean /help?"},
		// Ambiguous prefix or nothing close: fall back to /help.
		{"/q", "unknown command: /q — did you mean /quit?"},
		{"/bogus", "unknown command: /bogus (try /help)"},
	}
	for _, c := range cases {
		if got := defaultReplCommands.unknownCommandNotice(c.in); got != c.want {
			t.Errorf("unknownCommandNotice(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDispatchIsCaseInsensitive(t *testing.T) {
	ctx := &replCommandContext{}
	var replies []string
	ctx.reply = func(line string) error {
		replies = append(replies, line)
		return nil
	}
	handled, quit, err := defaultReplCommands.dispatch("/HELP", ctx)
	if err != nil || !handled || quit {
		t.Fatalf("dispatch(/HELP) handled=%v quit=%v err=%v", handled, quit, err)
	}
	if len(replies) == 0 || replies[0] != "commands:" {
		t.Fatalf("dispatch(/HELP) replies = %v", replies)
	}
}

func TestHintFor(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// Not a slash command in progress.
		{"", ""},
		{"hello", ""},
		{"ask/", ""},
		{"/he\nlp", ""},
		{"/zzz", ""},
		// Typing the name: many matches list bare names, few include summaries.
		{"/t", "/tab — list open tabs, or switch to one   /tabs — list open tabs, or switch to one   /tools — inspect loaded tools"},
		{"/to", "/tools — inspect loaded tools"},
		{"/q", "/quit — leave the REPL"},
		// Typing arguments: keyword matches from the command's completer.
		{"/get max", "maxcontext  maxtokens"},
		// Value completion for keys with enumerable values.
		{"/set thinking ", "dynamic  high  low  max  medium  minimal  off  xhigh"},
		{"/set thinking hi", "high"},
		// A fully typed keyword or a command without a completer falls back to
		// the usage reminder.
		{"/reset ", "usage: /reset confirm"},
		{"/help me", "usage: /help [command]"},
	}
	for _, c := range cases {
		if got := defaultReplCommands.hintFor(nil, c.in); got != c.want {
			t.Errorf("hintFor(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	bare := defaultReplCommands.hintFor(nil, "/")
	if !strings.Contains(bare, "/help") || !strings.Contains(bare, "/tools") {
		t.Fatalf("hintFor(/) should list all commands, got %q", bare)
	}
	if strings.Contains(bare, "—") {
		t.Fatalf("hintFor(/) should omit summaries when many commands match, got %q", bare)
	}
}

func TestCompleteToolNamesAndNamespaces(t *testing.T) {
	registry := tools.NewToolRegistry([]tools.Tool{
		&tools.Func{Name: "git__status", Desc: "Show git status"},
		&tools.Func{Name: "git__diff", Desc: "Show git diff"},
		tools.NewUnsafeBashTool(""),
	})
	ctx := &replCommandContext{state: &conversationState{toolRegistry: registry}}

	completed, matches, ok := defaultReplCommands.complete("/tools show git__", ctx)
	if !ok || completed != "/tools show git__" {
		t.Fatalf("complete(/tools show git__) = %q %v ok=%v", completed, matches, ok)
	}
	if strings.Join(matches, ",") != "/tools show git__diff,/tools show git__status" {
		t.Fatalf("tool name matches = %v", matches)
	}

	completed, _, ok = defaultReplCommands.complete("/tools show git__s", ctx)
	if !ok || completed != "/tools show git__status" {
		t.Fatalf("complete(/tools show git__s) = %q ok=%v", completed, ok)
	}

	completed, _, ok = defaultReplCommands.complete("/tools list g", ctx)
	if !ok || completed != "/tools list git" {
		t.Fatalf("complete(/tools list g) = %q ok=%v", completed, ok)
	}

	// Without a registry there is nothing to offer.
	if _, _, ok := defaultReplCommands.complete("/tools show git__", &replCommandContext{}); ok {
		t.Fatal("tool name completion without a registry should not match")
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

func TestSetCommand(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "set-test")
	settings := &Settings{Model: "anthropic/claude-sonnet-4-6"}
	applied := 0
	ctx := &replCommandContext{
		settings:        settings,
		state:           &conversationState{session: session},
		settingsApplied: func() { applied++ },
	}

	replies := dispatchDefaultCommandForTest(t, "/set temp 1.5", ctx)
	if settings.Temperature != 1.5 {
		t.Fatalf("temp = %v, want 1.5", settings.Temperature)
	}
	if got := strings.Join(replies, "\n"); got != "temp: 1.50" {
		t.Fatalf("/set temp replies = %q", got)
	}

	dispatchDefaultCommandForTest(t, "/set model openai/gpt-5.4", ctx)
	if settings.Model != "openai/gpt-5.4" {
		t.Fatalf("model = %q, want openai/gpt-5.4", settings.Model)
	}
	dispatchDefaultCommandForTest(t, "/set thinking high", ctx)
	if settings.ThinkingEffort != "high" {
		t.Fatalf("thinking = %q, want high", settings.ThinkingEffort)
	}
	dispatchDefaultCommandForTest(t, "/set tooltimeout 45s", ctx)
	if settings.ToolTimeout != 45*time.Second {
		t.Fatalf("tooltimeout = %v, want 45s", settings.ToolTimeout)
	}
	if applied != 4 {
		t.Fatalf("settingsApplied ran %d times, want 4", applied)
	}

	// Settings persist to session metadata so they survive relaunch.
	md, err := session.GetMetadata(context.Background())
	if err != nil {
		t.Fatalf("GetMetadata() error = %v", err)
	}
	if md == nil || md.Model != "openai/gpt-5.4" || md.Temperature != 1.5 || md.ToolTimeout != 45*time.Second {
		t.Fatalf("metadata not persisted: %+v", md)
	}

	// Invalid input explains the constraint and leaves the settings untouched.
	for _, c := range []struct{ line, wantSub string }{
		{"/set model gpt", "provider prefix"},
		{"/set temp eleven", "must be a number"},
		{"/set temp 9", "between 0.0 and 2.0"},
		{"/set maxtokens zero", "non-negative integer"},
		{"/set maxtokens -1", "non-negative integer"},
		{"/set tooltimeout -1s", "non-negative duration"},
		{"/set thinking sideways", "thinking"},
		{"/set bogus 1", "unknown or read-only key"},
		{"/set system terse", "unknown or read-only key"},
		{"/set", "usage: /set"},
	} {
		replies := dispatchDefaultCommandForTest(t, c.line, ctx)
		if got := strings.Join(replies, "\n"); !strings.Contains(got, c.wantSub) {
			t.Errorf("%s replies = %q, want mention of %q", c.line, got, c.wantSub)
		}
	}
	if settings.Model != "openai/gpt-5.4" || settings.Temperature != 1.5 || settings.ThinkingEffort != "high" {
		t.Fatalf("rejected /set mutated the settings: %+v", *settings)
	}

	// Without a session there are no settings to mutate.
	replies = dispatchDefaultCommandForTest(t, "/set temp 1.0", &replCommandContext{})
	if got := strings.Join(replies, "\n"); got != "settings unavailable" {
		t.Fatalf("configless /set replies = %q", got)
	}
}

func TestClearCommandOnlyClearsDisplay(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "clear-display")
	if err := session.AddMessage(context.Background(), messages.ChatMessage{Role: messages.MessageRoleUser, Content: "keep me"}); err != nil {
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
	if got := len(testSessionHistory(t, session)); got != 1 {
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

func TestLifecycleCommandsAppearInHelp(t *testing.T) {
	help := strings.Join(defaultReplCommands.helpLines(), "\n")
	for _, want := range []string{
		"/clear",
		"clear the display (keep conversation history)",
		"/reset",
	} {
		if !strings.Contains(help, want) {
			t.Fatalf("help missing %q in %q", want, help)
		}
	}
	for _, removed := range []string{"/queue", "/retry"} {
		if strings.Contains(help, removed) {
			t.Fatalf("help still exposes removed %s command: %q", removed, help)
		}
	}
}

func TestFallbackREPLDispatchesRegistryCommands(t *testing.T) {
	store := testOpenMemoryStore(t, nil)
	session := testAcquireSession(t, store, "fallback")
	testAddMessage(t, session, messages.ChatMessage{Role: messages.MessageRoleUser, Content: "hi"})
	state := &conversationState{session: session, toolRegistry: tools.NewToolRegistry(nil), settings: Settings{Model: "anthropic/claude-sonnet-4-6"}}
	config := &Config{}
	var out bytes.Buffer
	reader := bufio.NewReader(strings.NewReader("/get model\n/context\n/reset confirm\n/exit\n"))
	err := runREPLLoopWithCommands(context.Background(), reader, &out, newWriterReplCommandContext(config, state, &out), func(prompt string) error {
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
	if history := testSessionHistory(t, session); len(history) != 0 {
		t.Fatalf("fallback /reset confirm did not clear durable history: %#v", history)
	}
}

func TestFallbackREPLReportsUnknownSlashCommand(t *testing.T) {
	var out bytes.Buffer
	reader := bufio.NewReader(strings.NewReader("/bogus\n"))
	err := runREPLLoopWithCommands(context.Background(), reader, &out, newWriterReplCommandContext(nil, nil, &out), func(prompt string) error {
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
	err := runREPLLoopWithCommands(context.Background(), reader, &out, newWriterReplCommandContext(nil, nil, &out), func(prompt string) error {
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
