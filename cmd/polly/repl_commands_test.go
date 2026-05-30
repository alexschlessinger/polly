package main

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
)

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
	reader := bufio.NewReader(strings.NewReader("/get model\n/context\n/exit\n"))
	err = runREPLLoopWithCommands(reader, &out, newWriterReplCommandContext(config, state, &out), func(prompt string) error {
		t.Fatalf("runTurn should not be called for command %q", prompt)
		return nil
	})
	if err != nil {
		t.Fatalf("runREPLLoopWithCommands() error = %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "model: anthropic/claude-sonnet-4-6") || !strings.Contains(got, "context: fallback") {
		t.Fatalf("fallback command output = %q", got)
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

