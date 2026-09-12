package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/urfave/cli/v3"
)

func TestAutomaticContextConfigAndPersistence(t *testing.T) {
	for _, tc := range []struct {
		args   []string
		auto   bool
		budget int
	}{
		{nil, true, 256000}, {[]string{"--maxcontext", "256000"}, false, 256000}, {[]string{"--maxcontext", "0"}, false, 0},
	} {
		cmd := &cli.Command{Flags: historyConfigFlags(), Action: func(_ context.Context, c *cli.Command) error {
			s := parseConfig(c).Launch
			if s.AutoMaxContext != tc.auto || s.MaxHistoryTokens != tc.budget {
				t.Fatalf("%v: %+v", tc.args, s)
			}
			spec, _ := settingSpecFor("maxcontext")
			md := &sessions.Metadata{}
			spec.toMeta(&s, md)
			restored := Settings{}
			spec.fromMeta(&restored, md)
			if restored.AutoMaxContext != s.AutoMaxContext || restored.MaxHistoryTokens != s.MaxHistoryTokens {
				t.Fatalf("restore: %+v", restored)
			}
			return nil
		}}
		if err := cmd.Run(context.Background(), append([]string{"polly"}, tc.args...)); err != nil {
			t.Fatal(err)
		}
	}
	spec, _ := settingSpecFor("maxcontext")
	s := Settings{AutoMaxContext: true}
	spec.fromMeta(&s, &sessions.Metadata{MaxHistoryTokens: 256000})
	if s.AutoMaxContext {
		t.Fatal("legacy numeric setting became automatic")
	}
	if err := spec.parse(&s, "auto"); err != nil || !s.AutoMaxContext {
		t.Fatalf("auto: %+v %v", s, err)
	}
	if err := spec.parse(&s, "0"); err != nil || s.AutoMaxContext || s.MaxHistoryTokens != 0 {
		t.Fatalf("unlimited: %+v %v", s, err)
	}
}

func TestAutomaticContextFollowsDetectedCapacity(t *testing.T) {
	client := &metadataCompletionLLM{window: 1000000}
	agent := llm.NewAgent(client, nil, llm.AgentConfig{})
	defer agent.Close()
	state := &conversationState{agent: agent, settings: Settings{Model: "openai/test", AutoMaxContext: true, MaxHistoryTokens: 256000, MaxTokens: 4096}}
	for _, tc := range []struct{ window, want int }{{1000000, 895904}, {128000, 111104}, {0, 256000}} {
		client.window = tc.window
		if got := resolveContextBudget(context.Background(), state); got != tc.want {
			t.Fatalf("window %d: %d, want %d", tc.window, got, tc.want)
		}
	}
	client.window = 2000
	state.settings.AutoMaxContext = false
	state.settings.MaxHistoryTokens = 64000
	if got := resolveContextBudget(context.Background(), state); got != 64000 {
		t.Fatalf("explicit: %d", got)
	}
	state.settings.MaxHistoryTokens = 0
	if got := resolveContextBudget(context.Background(), state); got != 0 {
		t.Fatalf("unlimited: %d", got)
	}
}

func TestModelFormDetectedContextAndApply(t *testing.T) {
	for _, tc := range []struct {
		auto   bool
		budget int
	}{{true, 256000}, {false, 64000}, {false, 0}} {
		r, _ := newFormREPL(t)
		store := testOpenMemoryStore(t, nil)
		r.state.session = testAcquireSession(t, store, "form")
		r.state.settings.AutoMaxContext = tc.auto
		r.state.settings.MaxHistoryTokens = tc.budget
		r.openModelPicker()
		f := r.model.modal.modelForm
		f.provider = "openrouter"
		window, hostWindow := 1000000, 512000
		f.infos = map[string]llm.ModelInfo{"org/model": {ID: "org/model", Routed: true, ModelCapabilities: llm.ModelCapabilities{ContextTokens: &window}, Endpoints: []llm.ModelEndpointInfo{{ID: "host", ModelCapabilities: llm.ModelCapabilities{ContextTokens: &hostWindow}}}}}
		f.model.setText("org/model:host")
		if got := f.modelContextWindow(); got != hostWindow {
			t.Fatalf("host context: %d", got)
		}
		for _, width := range []int{40, 76} {
			text := plainStyledText(f.text(20, width))
			want := tc.budget
			if tc.auto {
				want = hostWindow
			}
			if !strings.Contains(text, fmt.Sprintf("Context  %d", want)) {
				t.Fatalf("missing context: %q", text)
			}
			for _, row := range strings.Split(text, "\n") {
				if len([]rune(row)) > width-2 {
					t.Fatalf("overflow: %q", row)
				}
			}
		}
		r.applyModelForm(f)
		want := tc.budget
		if tc.auto {
			want = hostWindow
		}
		md, err := r.state.session.GetMetadata(context.Background())
		if err != nil || md.MaxHistoryTokens != want || md.AutoMaxContext != tc.auto {
			t.Fatalf("apply: %+v %v", md, err)
		}
	}
}

func TestModelFormContextDraftPrecedenceAndPersistence(t *testing.T) {
	for _, tc := range []struct {
		input string
		auto  bool
		want  int
	}{
		{"2000000", false, 2000000}, {"0", false, 0}, {"auto", true, 1000000},
	} {
		r, _ := newFormREPL(t)
		store := testOpenMemoryStore(t, nil)
		r.state.session = testAcquireSession(t, store, "form")
		r.state.settings.AutoMaxContext = true
		r.state.settings.MaxHistoryTokens = defaultContextBudget
		r.openModelPicker()
		f := r.model.modal.modelForm
		f.syncContextLimit()
		if got := f.contextLimit.text(); got != "256000" {
			t.Fatalf("fallback: %s", got)
		}
		window := 1000000
		f.infos = map[string]llm.ModelInfo{"current": {ID: "current", ModelCapabilities: llm.ModelCapabilities{ContextTokens: &window}}}
		f.syncContextLimit()
		if got := f.contextLimit.text(); got != "1000000" {
			t.Fatalf("detection: %s", got)
		}
		formKey(r, "<Down>")
		formKey(r, "<Down>")
		formKey(r, "<C-u>")
		for _, ch := range tc.input {
			formKey(r, string(ch))
		}
		// A metadata refresh cannot overwrite the in-progress context draft.
		window = 128000
		f.text(20, 76)
		if got := f.contextLimit.text(); got != tc.input {
			t.Fatalf("draft replaced: %s", got)
		}
		window = 1000000
		if !r.state.settings.AutoMaxContext || r.state.settings.MaxHistoryTokens != defaultContextBudget {
			t.Fatal("draft changed live settings")
		}
		formKey(r, "<Enter>")
		formKey(r, "<Enter>")
		md, err := r.state.session.GetMetadata(context.Background())
		if err != nil || md.AutoMaxContext != tc.auto || md.MaxHistoryTokens != tc.want {
			t.Fatalf("saved: %+v %v", md, err)
		}
		r.openModelPicker()
		f = r.model.modal.modelForm
		f.infos = map[string]llm.ModelInfo{"current": {ID: "current", ModelCapabilities: llm.ModelCapabilities{ContextTokens: &window}}}
		f.syncContextLimit()
		if got := f.contextLimit.text(); got != fmt.Sprint(tc.want) {
			t.Fatalf("reopened: %s", got)
		}
	}
}

func TestModelFormContextInvalidAndCancelLeaveSettings(t *testing.T) {
	r, mp := newFormREPL(t)
	f := r.model.modal.modelForm
	before := r.state.settings.clone()
	formKey(r, "<Down>")
	formKey(r, "<Down>")
	formKey(r, "<C-u>")
	for _, ch := range "-1" {
		formKey(r, string(ch))
	}
	f.key.setText("draft-key")
	f.keyChanged = true
	formKey(r, "<Enter>")
	formKey(r, "<Enter>")
	if r.model.modal == nil || f.err == "" || r.state.settings.MaxHistoryTokens != before.MaxHistoryTokens || mp.APIKeySource("openai") != "environment" {
		t.Fatal("invalid context applied form")
	}
	formKey(r, "<Escape>")
	if r.state.settings.MaxHistoryTokens != before.MaxHistoryTokens || r.state.settings.AutoMaxContext != before.AutoMaxContext {
		t.Fatal("cancel applied context")
	}
}
