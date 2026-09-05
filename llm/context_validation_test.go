package llm

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestValidateRequestDoesNotWriteArtifactsOrChangeAgentState(t *testing.T) {
	ctx := context.Background()
	store := newTestArtifactStore()
	model := &recordingSequentialLLM{}
	agent := NewAgent(model, nil, AgentConfig{ArtifactStore: store})
	previous := messages.User("previous snapshot")
	agent.setTranscript(previous)
	previousRef := artifacts.RefForBlob(artifacts.Blob{Kind: artifacts.KindText, Data: []byte("previous artifact")})
	agent.indexArtifact(previousRef)
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "old request"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "old", Name: "lookup", Arguments: `{}`}}},
		{Role: messages.MessageRoleTool, ToolName: "lookup", ToolCallID: "old", Content: strings.Repeat("x", 8_000)},
		{Role: messages.MessageRoleAssistant, Content: "done"},
		{Role: messages.MessageRoleUser, Content: strings.Repeat("q", 2_000)},
	}
	before := cloneMessages(history)
	req := &CompletionRequest{Messages: history, MaxContextTokens: 2_500}
	if err := agent.ValidateRequest(ctx, req); err != nil {
		t.Fatal(err)
	}
	if len(model.requests) != 0 || len(store.blobs) != 0 {
		t.Fatalf("validation called provider or wrote artifacts: requests=%d blobs=%d", len(model.requests), len(store.blobs))
	}
	if !reflect.DeepEqual(agent.transcriptSnapshot(), previous) || !reflect.DeepEqual(agent.listArtifacts(), []artifacts.Ref{previousRef}) {
		t.Fatal("validation changed the agent's transcript or authorization index")
	}
	if !reflect.DeepEqual(history, before) {
		t.Fatal("validation changed caller-owned history")
	}
	response, err := agent.Run(ctx, req, nil)
	if err != nil || response.Projection.CompactedToolResults == 0 || len(store.blobs) == 0 {
		t.Fatalf("Run did not apply the validated reduction: response=%+v blobs=%d err=%v", response, len(store.blobs), err)
	}
}

func TestValidateRequestRejectsMissingSelectedImage(t *testing.T) {
	agent := NewAgent(&recordingSequentialLLM{}, nil, AgentConfig{ArtifactStore: newTestArtifactStore()})
	ref := artifacts.RefForBlob(artifacts.Blob{Kind: artifacts.KindImage, MIMEType: "image/png", Data: []byte("missing")})
	err := agent.ValidateRequest(context.Background(), &CompletionRequest{Messages: []messages.ChatMessage{{
		Role: messages.MessageRoleUser, Parts: []messages.ContentPart{imageArtifactPart(ref)},
	}}})
	if err == nil || !strings.Contains(err.Error(), "read image artifact") {
		t.Fatalf("missing selected image error = %v", err)
	}
}

func TestValidateRequestRespectsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	agent := NewAgent(&recordingSequentialLLM{}, nil, AgentConfig{})
	if err := agent.ValidateRequest(ctx, &CompletionRequest{Messages: messages.User("hi")}); !errors.Is(err, context.Canceled) {
		t.Fatalf("validation error = %v, want context.Canceled", err)
	}
}
