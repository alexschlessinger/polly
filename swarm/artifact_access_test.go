package swarm

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestMemberReadArtifactUsesItsPublishedAccess(t *testing.T) {
	ctx := context.Background()
	var ref artifacts.Ref
	model := modelFunc(func(ctx context.Context, req *llm.CompletionRequest) messages.ChatMessage {
		for _, msg := range req.Messages {
			if msg.ToolName == "read_artifact" {
				if !strings.Contains(msg.GetContent(), "2: published evidence") {
					t.Errorf("member artifact result = %q", msg.GetContent())
				}
				return answer("read shared evidence")
			}
		}
		return messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "read", Name: "read_artifact", Arguments: tools.Result(map[string]any{"id": ref.ID, "query": "evidence"})}}}
	})
	r := runtimeTest(t, model, 1, 4)
	var err error
	ref, err = r.config.Parent.ArtifactStore().Put(ctx, artifacts.Blob{Kind: artifacts.KindText, Data: []byte("header\npublished evidence\n")})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.parent.UpdateCoordination(ctx, func(s *sessions.CoordinationState) error {
		s.Pins = []string{ref.ID}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	config := r.config.Agent
	config.OpenArtifact = func(context.Context, string) (artifacts.Ref, io.ReadCloser, error) {
		t.Error("member inherited parent artifact callback")
		return artifacts.Ref{}, nil, errors.New("parent callback")
	}
	r.UpdateDefaults(r.config.Request, config, nil)
	result, err := r.Agent(ctx, "", AgentRequest{Label: "Read evidence", Task: "read shared evidence", ReadOnly: true})
	if err != nil || result.Value != "read shared evidence" {
		t.Fatalf("member result = %+v, %v", result, err)
	}
}
