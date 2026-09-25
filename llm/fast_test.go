package llm

import (
	"testing"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestFastModeReachesOnlyModelsWithATier(t *testing.T) {
	for _, model := range []string{"codex/gpt-5.5", "openai/gpt-5.4"} {
		if err := FastModeFor(model, ModelCapabilities{}); err != nil {
			t.Fatalf("%s: %v", model, err)
		}
	}
	ruledOut := ModelCapabilities{Parameters: map[string]bool{contract.ParameterServiceTier: false}}
	for model, caps := range map[string]ModelCapabilities{
		"anthropic/claude-sonnet-4-6": {},
		"codex/gpt-reserve":           ruledOut,
		"bare-model":                  {},
	} {
		err := FastModeFor(model, caps)
		want := "fast mode is not available for " + model
		if model == "anthropic/claude-sonnet-4-6" {
			want = "fast mode is not available for anthropic/ models"
		}
		if err == nil || err.Error() != want {
			t.Fatalf("%s: %v, want %q", model, err, want)
		}
	}

	req := &CompletionRequest{Model: "codex/gpt-5.5", Fast: true, Messages: messages.User("hi")}
	out, notes, err := PrepareCapabilities(req, ModelCapabilities{}, false)
	if err != nil || !out.Fast || len(notes) != 0 {
		t.Fatalf("unknown tier: fast=%v notes=%v err=%v", out.Fast, notes, err)
	}
	out, notes, err = PrepareCapabilities(req, ruledOut, false)
	if err != nil || out.Fast || len(notes) != 1 || notes[0].Feature != "fast" || notes[0].Message != "Fast mode omitted: not available for codex/gpt-5.5" {
		t.Fatalf("ruled out: fast=%v notes=%+v err=%v", out.Fast, notes, err)
	}
	req.Model = "anthropic/claude-sonnet-4-6"
	out, notes, err = PrepareCapabilities(req, ModelCapabilities{}, false)
	if err != nil || out.Fast || len(notes) != 1 || notes[0].Message != "Fast mode omitted: not available for anthropic/ models" {
		t.Fatalf("no tier: fast=%v notes=%+v err=%v", out.Fast, notes, err)
	}
	if !req.Fast {
		t.Fatal("preparation changed the caller's request")
	}
}
