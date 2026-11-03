package subagent

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestChildRestoreSelectionUsesEffectiveAvailability(t *testing.T) {
	store, err := sessions.OpenStore(sessions.StoreConfig{Mode: sessions.ModeMemory})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	session, err := store.Acquire(context.Background(), "child-restore", sessions.AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	for _, tc := range []struct {
		name   string
		config llm.AgentConfig
		want   bool
	}{
		{name: "no store"},
		{name: "enabled", config: llm.AgentConfig{ArtifactStore: session.ArtifactStore()}, want: true},
		{name: "selected without store", config: llm.AgentConfig{Builtins: []string{llm.BuiltinRestoreArtifact}}},
		{name: "explicit builtin", config: llm.AgentConfig{ArtifactStore: session.ArtifactStore(), Builtins: []string{llm.BuiltinRestoreArtifact}}, want: true},
		{name: "omitted builtin", config: llm.AgentConfig{ArtifactStore: session.ArtifactStore(), Builtins: []string{llm.BuiltinReadTranscript}}},
	} {
		for _, binding := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{true: "/native", false: "/derived"}[binding], func(t *testing.T) {
				parent := tools.NewToolRegistry(nil, tools.WithNativeTools(), tools.WithUnsafeNoSandbox())
				defer parent.Close()
				model := &sequentialLLM{responses: []messages.ChatMessage{reply("done")}}
				run := AgentRunner(model, parent, llm.CompletionRequest{}, tc.config)
				if binding {
					run = RunnerWithTools(model, tools.NativeOpenTools(parent), tools.ToolScope{Root: t.TempDir()}, llm.CompletionRequest{}, tc.config)
				}
				_, err := run(context.Background(), Request{Label: "restore", Task: "restore an artifact", Tools: []string{llm.BuiltinRestoreArtifact}})
				if tc.want {
					if err != nil || model.calls != 1 || !slices.Contains(names(model.last.Tools), llm.BuiltinRestoreArtifact) {
						t.Fatalf("enabled restore: calls=%d, err=%v", model.calls, err)
					}
				} else if !errors.Is(err, ErrNoMatchingTools) || model.calls != 0 {
					t.Fatalf("unavailable restore accepted: calls=%d, err=%v", model.calls, err)
				}
			})
		}
	}
}

func TestChildRestoreSelectionCannotBorrowParentAuthority(t *testing.T) {
	// Even a visible tool with this reserved name must not satisfy a child
	// whose effective configuration cannot install its own private restore tool.
	parent := tools.NewToolRegistry([]tools.Tool{&tools.Func{Name: llm.BuiltinRestoreArtifact}})
	defer parent.Close()
	model := &sequentialLLM{}
	run := AgentRunner(model, parent, llm.CompletionRequest{}, llm.AgentConfig{})
	if _, err := run(context.Background(), Request{Label: "restore", Task: "restore an artifact", Tools: []string{llm.BuiltinRestoreArtifact}}); !errors.Is(err, ErrNoMatchingTools) || model.calls != 0 {
		t.Fatalf("borrowed restore authority: calls=%d, err=%v", model.calls, err)
	}
}
