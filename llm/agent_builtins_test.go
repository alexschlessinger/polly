package llm

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestAgentConfigBuiltinsSelectPrivateTools(t *testing.T) {
	store := newTestArtifactStore()
	for _, tc := range []struct {
		name   string
		config AgentConfig
		want   []string
	}{
		{name: "nil installs everything the store allows", config: AgentConfig{ArtifactStore: store}, want: []string{BuiltinListArtifacts, BuiltinReadArtifact, BuiltinReadTranscript}},
		{name: "nil without a store installs the transcript reader", config: AgentConfig{}, want: []string{BuiltinReadTranscript}},
		{name: "empty installs none", config: AgentConfig{ArtifactStore: store, Builtins: []string{}}, want: nil},
		{name: "artifact readers only", config: AgentConfig{ArtifactStore: store, Builtins: []string{BuiltinReadArtifact, BuiltinListArtifacts}}, want: []string{BuiltinListArtifacts, BuiltinReadArtifact}},
		{name: "artifact readers need a store", config: AgentConfig{Builtins: []string{BuiltinReadArtifact, BuiltinListArtifacts}}, want: nil},
		{name: "unknown names are ignored", config: AgentConfig{ArtifactStore: store, Builtins: []string{"view_image", BuiltinReadTranscript}}, want: []string{BuiltinReadTranscript}},
		{name: "disabled tools install none", config: AgentConfig{ArtifactStore: store, DisableTools: true}, want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := tools.NewToolRegistry([]tools.Tool{&tools.Func{Name: "configured", Run: func(context.Context, tools.Args) (string, error) { return "ok", nil }}})
			defer registry.Close()
			agent := NewAgent(nil, registry, tc.config)
			defer agent.Close()
			var got []string
			for _, tool := range agent.ToolRegistry().All() {
				if slices.Contains(BuiltinToolNames(), tool.GetName()) {
					got = append(got, tool.GetName())
				}
			}
			slices.Sort(got)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("installed built-ins = %v, want %v", got, tc.want)
			}
			if effective := tc.config.BuiltinTools(); !slices.Equal(effective, tc.want) {
				t.Fatalf("BuiltinTools() = %v, want %v", effective, tc.want)
			}
			if _, ok := agent.ToolRegistry().Get("configured"); !ok {
				t.Fatal("the caller's tool went missing")
			}
			for _, name := range BuiltinToolNames() {
				if _, ok := registry.Get(name); ok {
					t.Fatalf("built-in %s leaked into the caller's registry", name)
				}
			}
		})
	}
}

func TestAgentOmittedBuiltinsAreNeverAdvertisedOrRecommended(t *testing.T) {
	store := newTestArtifactStore()
	registry := tools.NewToolRegistry(nil, tools.WithNativeTools())
	defer registry.Close()
	client := ownershipLLM(func(_ context.Context, req *CompletionRequest) messages.ChatMessage {
		for _, tool := range req.Tools {
			if tool.GetName() == BuiltinReadTranscript {
				t.Error("read_transcript was advertised although it was omitted")
			}
		}
		listed := false
		for _, tool := range req.Tools {
			if tool.GetName() == BuiltinListArtifacts {
				listed = true
			}
		}
		if !listed {
			t.Error("list_artifacts was not advertised although it was selected")
		}
		return messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "done", StopReason: messages.StopReasonEndTurn}
	})
	agent := NewAgent(client, registry, AgentConfig{ArtifactStore: store, Builtins: []string{BuiltinListArtifacts, BuiltinReadArtifact}})
	defer agent.Close()
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "old " + repeatWords("history", 2000)},
		{Role: messages.MessageRoleAssistant, Content: "old answer"},
		{Role: messages.MessageRoleUser, Content: "new question"},
	}
	resp, err := agent.Run(context.Background(), &CompletionRequest{MaxContextTokens: 2000, Messages: history}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Projection.OmittedExchanges != 1 {
		t.Fatalf("omitted exchanges = %d, want 1", resp.Projection.OmittedExchanges)
	}
	// The omission marker recommends only what the model can call.
	projected, _, err := projectCompletionRequest(context.Background(), &CompletionRequest{MaxContextTokens: 2000, Messages: history}, store, projectionToolsFor(agent.ToolRegistry().All()), nil)
	if err != nil {
		t.Fatal(err)
	}
	marker := projectedText(projected)
	if !strings.Contains(marker, "call list_artifacts to enumerate them") || strings.Contains(marker, "call read_transcript") {
		t.Fatalf("marker = %q", marker)
	}
}

func repeatWords(word string, n int) string {
	out := make([]byte, 0, (len(word)+1)*n)
	for range n {
		out = append(out, word...)
		out = append(out, ' ')
	}
	return string(out)
}
