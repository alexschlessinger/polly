package swarm

import (
	"context"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestMemberBuiltinsFollowTheDefaultAgentConfig(t *testing.T) {
	var offered []string
	model := modelFunc(func(_ context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		offered = offered[:0]
		for _, tool := range req.Tools {
			offered = append(offered, tool.GetName())
		}
		return answer("done")
	})
	r := runtimeTest(t, model, 1, 8)
	r.UpdateDefaults(r.config.Request, llm.AgentConfig{Builtins: []string{llm.BuiltinListArtifacts, llm.BuiltinReadArtifact}, MaxIterations: 2}, nil)

	if _, err := r.Agent(context.Background(), "", AgentRequest{Label: "Test agent", Task: "inspect", ReadOnly: true}); err != nil {
		t.Fatalf("member run: %v", err)
	}
	has := func(name string) bool {
		for _, n := range offered {
			if n == name {
				return true
			}
		}
		return false
	}
	if has(llm.BuiltinReadTranscript) || !has(llm.BuiltinReadArtifact) || !has(llm.BuiltinListArtifacts) {
		t.Fatalf("member tools = %v, want the artifact readers without read_transcript", offered)
	}

	// A selection may only name a built-in the member will have.
	_, err := r.Agent(context.Background(), "", AgentRequest{Label: "Test agent", Task: "inspect", ReadOnly: true, Tools: []string{llm.BuiltinReadTranscript}})
	if err == nil || !strings.Contains(err.Error(), `required tool "read_transcript" cannot honor execution context`) {
		t.Fatalf("omitted built-in selection = %v", err)
	}
	if _, err := r.Agent(context.Background(), "", AgentRequest{Label: "Test agent", Task: "inspect", ReadOnly: true, Tools: []string{llm.BuiltinReadArtifact}}); err != nil {
		t.Fatalf("installed built-in selection refused: %v", err)
	}
}
