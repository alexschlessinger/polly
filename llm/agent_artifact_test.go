package llm

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestAgentExternalizesRichToolImageAndAttachesItOnce(t *testing.T) {
	imageBytes := []byte("typed MCP-style image bytes")
	rich := &testRichTool{name: "render", output: tools.ToolOutput{
		Text:  "render complete",
		Media: []tools.ToolMedia{{Data: imageBytes, MIMEType: "image/png", Name: "render.png"}},
	}}
	pathTool := &tools.Func{Name: "inspect", Run: func(context.Context, tools.Args) (string, error) {
		return "generated path: /tmp/never-auto-upload.png", nil
	}}
	model := &recordingSequentialLLM{responses: []messages.ChatMessage{
		{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "render-call", Name: "render", Arguments: `{}`}}},
		{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "inspect-call", Name: "inspect", Arguments: `{}`}}},
		{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonEndTurn, Content: "done"},
	}}
	store := newTestArtifactStore()
	registry := tools.NewToolRegistry([]tools.Tool{rich, pathTool})
	agent := NewAgent(model, registry, AgentConfig{ArtifactStore: store})

	response, err := agent.Run(context.Background(), &CompletionRequest{Messages: messages.User("render it")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(model.requests) != 3 {
		t.Fatalf("model requests = %d, want 3", len(model.requests))
	}

	var durableTool messages.ChatMessage
	for _, msg := range response.AllMessages {
		if msg.Role == messages.MessageRoleTool && msg.ToolName == "render" {
			durableTool = msg
			break
		}
	}
	if len(durableTool.Parts) != 1 || durableTool.Parts[0].Artifact == nil || durableTool.Parts[0].Artifact.Kind != artifacts.KindImage {
		t.Fatalf("durable rich tool result = %#v", durableTool)
	}
	if token := durableTool.Parts[0].Artifact.ImageToken; !stableImageTokenPattern.MatchString(token) || !strings.Contains(durableTool.Content, token) {
		t.Fatalf("tool image has no visible stable token: ref=%#v content=%q", durableTool.Parts[0].Artifact, durableTool.Content)
	}
	encodedImage := base64.StdEncoding.EncodeToString(imageBytes)
	if strings.Contains(durableTool.Content, encodedImage) || durableTool.Parts[0].ImageData != "" {
		t.Fatalf("image bytes leaked into durable textual tool output: %#v", durableTool)
	}
	r, err := store.Open(context.Background(), durableTool.Parts[0].Artifact.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil || string(stored) != string(imageBytes) {
		t.Fatalf("stored image = %q, %v", stored, err)
	}

	if got := len(projectedImageParts(model.requests[1])); got != 1 {
		t.Fatalf("immediate following request contains %d images, want 1: %#v", got, model.requests[1])
	}
	if got := len(projectedImageParts(model.requests[2])); got != 0 {
		t.Fatalf("later request reattached image %d time(s): %#v", got, model.requests[2])
	}
	if messagesContainEncodedImageInText(model.requests[1], encodedImage) {
		t.Fatal("base64 image leaked into a textual provider message")
	}
	if got := len(projectedImageParts(model.requests[2])); got != 0 || !strings.Contains(projectedText(model.requests[2]), "/tmp/never-auto-upload.png") {
		t.Fatalf("textual filesystem path was not kept text-only: %#v", model.requests[2])
	}
	if _, ok, allowed := registry.GetIfAllowed("read_artifact"); !ok || !allowed {
		t.Fatalf("session read_artifact tool registration = exists:%v allowed:%v", ok, allowed)
	}
}

func TestAgentLargeToolResultKeepsFullArtifactAndSendsPreview(t *testing.T) {
	full := "HEAD\n" + strings.Repeat("large tool result line\n", 4_000) + "TAIL"
	tool := &tools.Func{Name: "large", Run: func(context.Context, tools.Args) (string, error) { return full, nil }}
	model := &recordingSequentialLLM{responses: []messages.ChatMessage{
		{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "large-call", Name: "large", Arguments: `{}`}}},
		{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonEndTurn, Content: "done"},
	}}
	store := newTestArtifactStore()
	agent := NewAgent(model, tools.NewToolRegistry([]tools.Tool{tool}), AgentConfig{ArtifactStore: store})
	var callbackResult string

	response, err := agent.Run(context.Background(), &CompletionRequest{Messages: messages.User("run")}, &AgentCallbacks{
		OnToolEnd: func(_ messages.ChatMessageToolCall, result string, _ time.Duration, _ error) {
			callbackResult = result
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if callbackResult != full {
		t.Fatalf("tool observer received %d bytes, want complete %d", len(callbackResult), len(full))
	}
	var durable messages.ChatMessage
	for _, msg := range response.AllMessages {
		if msg.Role == messages.MessageRoleTool {
			durable = msg
		}
	}
	if len(durable.Parts) != 1 || durable.Parts[0].Artifact == nil || durable.Content != artifactBirthPreview(*durable.Parts[0].Artifact, []byte(full)) {
		t.Fatalf("durable large tool result = %#v", durable)
	}
	r, err := store.Open(context.Background(), durable.Parts[0].Artifact.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil || string(stored) != full {
		t.Fatalf("stored tool result has %d bytes, want %d; err=%v", len(stored), len(full), err)
	}
	projectedTools := messagesWithRole(model.requests[1], messages.MessageRoleTool)
	if len(projectedTools) != 1 || !strings.Contains(projectedTools[0].Content, "head/tail preview") || estimatedStringTokens(projectedTools[0].Content) > toolPreviewTokenLimit {
		t.Fatalf("provider tool projection = %#v", projectedTools)
	}
	// The provider-visible form equals the durable form: projection never
	// rewrites a born preview, keeping the prompt prefix byte-stable.
	if projectedTools[0].Content != durable.Content {
		t.Fatalf("projected form diverged from durable form: %q", projectedTools[0].Content[:min(200, len(projectedTools[0].Content))])
	}
}

func TestAgentLargeRichToolResultBoundsBirthPreviewWithMediaDescriptors(t *testing.T) {
	full := "HEAD\n" + strings.Repeat("large rich tool result line\n", 4_000) + "TAIL"
	rich := &testRichTool{name: "large-rich", output: tools.ToolOutput{
		Text: full,
		Media: []tools.ToolMedia{
			{Data: []byte("first image bytes"), MIMEType: "image/png", Name: "first.png", Reference: "[image #9]"},
			{Data: []byte("binary report bytes"), MIMEType: "application/pdf", Name: "report.pdf"},
			{Data: []byte("second image bytes"), MIMEType: "image/webp", Name: "second.webp", Reference: "[image #10]"},
		},
	}}
	model := &recordingSequentialLLM{responses: []messages.ChatMessage{
		{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "large-rich-call", Name: "large-rich", Arguments: `{}`}}},
		{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonEndTurn, Content: "done"},
	}}
	store := newTestArtifactStore()
	agent := NewAgent(model, tools.NewToolRegistry([]tools.Tool{rich}), AgentConfig{ArtifactStore: store})

	response, err := agent.Run(context.Background(), &CompletionRequest{Messages: messages.User("run")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var durable messages.ChatMessage
	for _, msg := range response.AllMessages {
		if msg.Role == messages.MessageRoleTool && msg.ToolName == "large-rich" {
			durable = msg
			break
		}
	}
	textRef := textArtifactRef(durable)
	if textRef == nil {
		t.Fatalf("durable rich tool result refs = %#v", durable.Parts)
	}
	descriptors := artifactDescriptors(durable, textRef.ID)
	if len(descriptors) != len(rich.output.Media) {
		t.Fatalf("durable media descriptors = %q, want %d", descriptors, len(rich.output.Media))
	}
	for _, descriptor := range descriptors {
		if !strings.Contains(durable.Content, descriptor) {
			t.Fatalf("durable preview omitted media descriptor %q: %q", descriptor, durable.Content)
		}
	}
	if got := estimatedStringTokens(durable.Content); got > toolPreviewTokenLimit {
		t.Fatalf("durable preview tokens = %d, want <= %d", got, toolPreviewTokenLimit)
	}
	head, tail := previewWindows([]byte(full))
	if want := artifactPreviewWithDescriptors(*textRef, head, tail, durable); durable.Content != want {
		t.Fatalf("durable preview differs from bounded descriptor-aware form")
	}
	projectedTools := messagesWithRole(model.requests[1], messages.MessageRoleTool)
	if len(projectedTools) != 1 || projectedTools[0].Content != durable.Content {
		t.Fatalf("provider and durable tool forms diverged: projected=%#v durable=%#v", projectedTools, durable)
	}
}

func TestAgentPersistsCurrentTurnPressureSpills(t *testing.T) {
	first := strings.Repeat("first-inline-", 1_600)
	second := strings.Repeat("second-inline-", 1_500)
	firstTool := &tools.Func{Name: "first", Run: func(context.Context, tools.Args) (string, error) { return first, nil }}
	secondTool := &tools.Func{Name: "second", Run: func(context.Context, tools.Args) (string, error) { return second, nil }}
	model := &recordingSequentialLLM{responses: []messages.ChatMessage{
		{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "first", Name: "first", Arguments: `{}`}}},
		{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "second", Name: "second", Arguments: `{}`}}},
		{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonEndTurn, Content: "done"},
	}}
	store := newTestArtifactStore()
	agent := NewAgent(model, tools.NewToolRegistry([]tools.Tool{firstTool, secondTool}), AgentConfig{ArtifactStore: store})

	response, err := agent.Run(context.Background(), &CompletionRequest{
		Messages: messages.User("run both"), MaxContextTokens: 6_000,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var spilled messages.ChatMessage
	for _, msg := range response.AllMessages {
		if msg.Role == messages.MessageRoleTool && msg.ToolName == "first" {
			spilled = msg
		}
	}
	ref := textArtifactRef(spilled)
	if ref == nil || spilled.Content != artifactReceipt(*ref) {
		t.Fatalf("durable pressure spill = %#v", spilled)
	}
	r, err := store.Open(context.Background(), ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil || string(stored) != first {
		t.Fatalf("pressure-spilled bytes = %d, %v", len(stored), err)
	}
}

func TestAgentSurfacesArtifactStoreFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output tools.ToolOutput
	}{
		{name: "large text", output: tools.ToolOutput{Text: strings.Repeat("failed text", 4_000)}},
		{name: "image", output: tools.ToolOutput{Text: "image", Media: []tools.ToolMedia{{Data: []byte("image bytes"), MIMEType: "image/png", Name: "failed.png"}}}},
		{name: "binary", output: tools.ToolOutput{Text: "binary", Media: []tools.ToolMedia{{Data: []byte("binary bytes"), MIMEType: "application/octet-stream", Name: "failed.bin"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rich := &testRichTool{name: "failing-store", output: tc.output}
			model := &recordingSequentialLLM{responses: []messages.ChatMessage{
				{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "failure-call", Name: "failing-store", Arguments: `{}`}}},
				{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonEndTurn, Content: "must not run"},
			}}
			agent := NewAgent(model, tools.NewToolRegistry([]tools.Tool{rich}), AgentConfig{ArtifactStore: failingArtifactStore{}})
			var callbackErr error

			response, err := agent.Run(context.Background(), &CompletionRequest{Messages: messages.User("run")}, &AgentCallbacks{
				OnToolEnd: func(_ messages.ChatMessageToolCall, _ string, _ time.Duration, err error) {
					callbackErr = err
				},
			})
			if !errors.Is(err, errFailingArtifactStore) {
				t.Fatalf("Run() error = %v, want artifact storage failure", err)
			}
			if !errors.Is(callbackErr, errFailingArtifactStore) {
				t.Fatalf("OnToolEnd() error = %v, want artifact storage failure", callbackErr)
			}
			if len(model.requests) != 1 {
				t.Fatalf("model requests = %d, want failure before follow-up call", len(model.requests))
			}
			if response == nil || len(messagesWithRole(response.AllMessages, messages.MessageRoleTool)) != 0 {
				t.Fatalf("failed artifact result became durable: %#v", response)
			}
		})
	}
}

func TestAgentPreservesRichMediaReturnedWithToolError(t *testing.T) {
	imageBytes := []byte("error evidence image")
	rich := &testRichTool{name: "failing-rich", err: errors.New("tool failed"), output: tools.ToolOutput{
		Text: "failure detail", Media: []tools.ToolMedia{{Data: imageBytes, MIMEType: "image/png", Name: "failure.png"}},
	}}
	model := &recordingSequentialLLM{responses: []messages.ChatMessage{
		{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "call", Name: "failing-rich", Arguments: `{}`}}},
		{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonEndTurn, Content: "handled"},
	}}
	store := newTestArtifactStore()
	agent := NewAgent(model, tools.NewToolRegistry([]tools.Tool{rich}), AgentConfig{ArtifactStore: store})

	response, err := agent.Run(context.Background(), &CompletionRequest{Messages: messages.User("run")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var result messages.ChatMessage
	for _, msg := range response.AllMessages {
		if msg.Role == messages.MessageRoleTool {
			result = msg
		}
	}
	succeeded, known := result.ToolSucceeded()
	if !known || succeeded || len(result.Parts) != 1 || result.Parts[0].Artifact == nil || result.Parts[0].Artifact.Kind != artifacts.KindImage {
		t.Fatalf("rich error audit result = %#v", result)
	}
	r, err := store.Open(context.Background(), result.Parts[0].Artifact.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil || string(stored) != string(imageBytes) {
		t.Fatalf("stored error media = %q, %v", stored, err)
	}
}

func TestAgentUsesRichToolOutputWithoutArtifactStore(t *testing.T) {
	imageBytes := []byte("no-store image")
	rich := &testRichTool{name: "rich-no-store", output: tools.ToolOutput{
		Text: "typed text", Media: []tools.ToolMedia{{Data: imageBytes, MIMEType: "image/png", Name: "image.png"}},
	}}
	model := &recordingSequentialLLM{responses: []messages.ChatMessage{
		{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "call", Name: "rich-no-store", Arguments: `{}`}}},
		{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonEndTurn, Content: "done"},
	}}
	agent := NewAgent(model, tools.NewToolRegistry([]tools.Tool{rich}), AgentConfig{})

	response, err := agent.Run(context.Background(), &CompletionRequest{Messages: messages.User("run")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var result messages.ChatMessage
	for _, msg := range response.AllMessages {
		if msg.Role == messages.MessageRoleTool {
			result = msg
		}
	}
	if result.Content != "typed text" || len(result.Parts) != 1 || result.Parts[0].Type != "image_base64" {
		t.Fatalf("rich result was narrowed to textual JSON without a store: %#v", result)
	}
	if strings.Contains(result.Content, base64.StdEncoding.EncodeToString(imageBytes)) {
		t.Fatalf("rich image leaked into tool text: %q", result.Content)
	}
}

func TestAgentKeepsBinaryFallbackForAuditButSendsOnlyDescriptor(t *testing.T) {
	binaryBytes := []byte("private binary payload")
	rich := &testRichTool{name: "binary-no-store", output: tools.ToolOutput{
		Media: []tools.ToolMedia{{Data: binaryBytes, MIMEType: "application/pdf", Name: "result.pdf"}},
	}}
	model := &recordingSequentialLLM{responses: []messages.ChatMessage{
		{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "call", Name: "binary-no-store", Arguments: `{}`}}},
		{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonEndTurn, Content: "done"},
	}}
	agent := NewAgent(model, tools.NewToolRegistry([]tools.Tool{rich}), AgentConfig{})

	response, err := agent.Run(context.Background(), &CompletionRequest{Messages: messages.User("run")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var durable messages.ChatMessage
	for _, msg := range response.AllMessages {
		if msg.Role == messages.MessageRoleTool {
			durable = msg
		}
	}
	encoded := base64.StdEncoding.EncodeToString(binaryBytes)
	if len(durable.Parts) != 1 || durable.Parts[0].Type != "file" || durable.Parts[0].Text != encoded || !strings.Contains(durable.Content, "payload retained outside model text") {
		t.Fatalf("durable binary fallback = %#v", durable)
	}
	if strings.Contains(projectedText(model.requests[1]), encoded) {
		t.Fatalf("binary base64 leaked into projected text: %#v", model.requests[1])
	}
	for _, msg := range model.requests[1] {
		for _, part := range msg.Parts {
			if part.Type == "file" {
				t.Fatalf("private binary part reached provider projection: %#v", msg)
			}
		}
	}
}

func TestAgentIndexesTransientLegacyToolArtifactForReadTool(t *testing.T) {
	legacyOutput := "FIRST LINE\n" + strings.Repeat("legacy body\n", 5_000)
	model := &legacyArtifactReaderLLM{}
	store := newTestArtifactStore()
	agent := NewAgent(model, nil, AgentConfig{ArtifactStore: store})
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "old request"},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "legacy", Name: "legacy_tool", Arguments: `{}`}}},
		{Role: messages.MessageRoleTool, ToolCallID: "legacy", ToolName: "legacy_tool", Content: legacyOutput},
		{Role: messages.MessageRoleAssistant, Content: "old answer"},
		{Role: messages.MessageRoleUser, Content: "inspect that stored output"},
	}

	response, err := agent.Run(context.Background(), &CompletionRequest{Messages: history}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var readResult messages.ChatMessage
	for _, msg := range response.AllMessages {
		if msg.Role == messages.MessageRoleTool && msg.ToolName == "read_artifact" {
			readResult = msg
		}
	}
	succeeded, known := readResult.ToolSucceeded()
	if !known || !succeeded || !strings.HasPrefix(readResult.Content, "1: FIRST LINE") {
		t.Fatalf("transient artifact read result = %#v", readResult)
	}
}

func TestAgentStopsBeforeProviderWhenActiveExchangeExceedsBudget(t *testing.T) {
	model := &recordingSequentialLLM{}
	agent := NewAgent(model, nil, AgentConfig{})
	response, err := agent.Run(context.Background(), &CompletionRequest{
		MaxContextTokens: 100,
		Messages:         messages.User(strings.Repeat("too large ", 1_000)),
	}, nil)
	var limitErr *ContextLimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("Run error = %v, want ContextLimitError", err)
	}
	if len(model.requests) != 0 {
		t.Fatalf("provider was called %d time(s) after local context overflow", len(model.requests))
	}
	if response == nil || response.Projection.EstimatedTokens != limitErr.EstimatedTokens {
		t.Fatalf("projection stats = %#v, error=%#v", response, limitErr)
	}
}

func TestAgentArtifactAuthorizationResetsWithEachTranscript(t *testing.T) {
	store := newTestArtifactStore()
	ref := putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindText, Data: []byte("old private result")})
	agent := NewAgent(&recordingSequentialLLM{}, nil, AgentConfig{ArtifactStore: store})
	withArtifact := []messages.ChatMessage{{
		Role: messages.MessageRoleTool, Content: artifactReceipt(ref),
		Parts: []messages.ContentPart{{Type: "artifact", Artifact: &ref}},
	}}
	if _, err := agent.Run(context.Background(), &CompletionRequest{Messages: withArtifact}, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := agent.lookupArtifact(ref.ID); !ok {
		t.Fatal("current transcript artifact was not authorized")
	}
	if _, err := agent.Run(context.Background(), &CompletionRequest{Messages: messages.User("after reset")}, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := agent.lookupArtifact(ref.ID); ok {
		t.Fatal("artifact from replaced transcript remained authorized")
	}
}

func TestAgentDoesNotAuthorizeInternalOnlyArtifacts(t *testing.T) {
	store := newTestArtifactStore()
	ref := putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindText, MIMEType: "text/plain", Name: "internal.txt", Data: []byte("provider-hidden internal data")})
	model := &recordingSequentialLLM{responses: []messages.ChatMessage{
		{
			Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse,
			ToolCalls: []messages.ChatMessageToolCall{{ID: "list", Name: "list_artifacts", Arguments: `{}`}},
		},
		{
			Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse,
			ToolCalls: []messages.ChatMessageToolCall{{
				ID: "read", Name: "read_artifact", Arguments: fmt.Sprintf(`{"id":%q,"limit":1}`, ref.ID),
			}},
		},
		{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonEndTurn, Content: "done"},
	}}
	agent := NewAgent(model, nil, AgentConfig{ArtifactStore: store})
	history := []messages.ChatMessage{
		{
			Role: messages.MessageRoleInternal, Content: artifactReceipt(ref),
			Parts: []messages.ContentPart{{Type: "artifact", Artifact: &ref}},
		},
		{Role: messages.MessageRoleUser, Content: "continue"},
	}

	response, err := agent.Run(context.Background(), &CompletionRequest{Messages: history}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var listResult, readResult messages.ChatMessage
	for _, msg := range response.AllMessages {
		if msg.Role != messages.MessageRoleTool {
			continue
		}
		switch msg.ToolName {
		case "list_artifacts":
			listResult = msg
		case "read_artifact":
			readResult = msg
		}
	}
	if strings.Contains(listResult.Content, ref.ID) || listResult.Content != "No artifacts are referenced by this conversation." {
		t.Fatalf("internal-only artifact leaked through catalog: %#v", listResult)
	}
	if succeeded, known := readResult.ToolSucceeded(); !known || succeeded || !strings.Contains(readResult.Content, "is not referenced by this conversation") {
		t.Fatalf("internal-only artifact remained readable: %#v", readResult)
	}
}

type testRichTool struct {
	tools.NativeTool
	name   string
	output tools.ToolOutput
	err    error
}

func (t *testRichTool) GetName() string { return t.name }

func (t *testRichTool) GetSchema() *schema.ToolSchema {
	return schema.Tool(t.name, "test rich output", nil)
}

func (t *testRichTool) Execute(context.Context, map[string]any) (string, error) {
	return t.output.Text, t.err
}

func (t *testRichTool) ExecuteOutput(context.Context, map[string]any) (tools.ToolOutput, error) {
	return t.output, t.err
}

type recordingSequentialLLM struct {
	mu        sync.Mutex
	responses []messages.ChatMessage
	requests  [][]messages.ChatMessage
}

type legacyArtifactReaderLLM struct{ calls int }

var testArtifactIDPattern = regexp.MustCompile(`sha256:[0-9a-f]{64}`)

func (l *legacyArtifactReaderLLM) ChatCompletionStream(_ context.Context, req *CompletionRequest, processor EventStreamProcessor) <-chan *messages.StreamEvent {
	var response messages.ChatMessage
	if l.calls == 0 {
		var id string
		for _, msg := range req.Messages {
			if match := testArtifactIDPattern.FindString(msg.Content); match != "" {
				id = match
				break
			}
		}
		response = messages.ChatMessage{
			Role:       messages.MessageRoleAssistant,
			StopReason: messages.StopReasonToolUse,
			ToolCalls: []messages.ChatMessageToolCall{{
				ID: "read", Name: "read_artifact", Arguments: fmt.Sprintf(`{"id":%q,"limit":1}`, id),
			}},
		}
	} else {
		response = messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonEndTurn, Content: "done"}
	}
	l.calls++
	input := make(chan messages.ChatMessage, 1)
	input <- response
	close(input)
	return processor.ProcessMessagesToEvents(input)
}

func (r *recordingSequentialLLM) ChatCompletionStream(_ context.Context, req *CompletionRequest, processor EventStreamProcessor) <-chan *messages.StreamEvent {
	r.mu.Lock()
	index := len(r.requests)
	r.requests = append(r.requests, cloneMessages(req.Messages))
	response := messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonEndTurn, Content: "done"}
	if index < len(r.responses) {
		response = r.responses[index]
	}
	r.mu.Unlock()
	input := make(chan messages.ChatMessage, 1)
	input <- response
	close(input)
	return processor.ProcessMessagesToEvents(input)
}

type failingArtifactStore struct{}

var errFailingArtifactStore = errors.New("store unavailable")

func (failingArtifactStore) Put(context.Context, artifacts.Blob) (artifacts.Ref, error) {
	return artifacts.Ref{}, errFailingArtifactStore
}

func (failingArtifactStore) Open(context.Context, string) (io.ReadCloser, error) {
	return nil, errFailingArtifactStore
}

func (failingArtifactStore) RemoveAll(context.Context) error { return nil }

func messagesContainEncodedImageInText(history []messages.ChatMessage, encoded string) bool {
	for _, msg := range history {
		if strings.Contains(msg.Content, encoded) || strings.Contains(msg.Reasoning, encoded) {
			return true
		}
		for _, part := range msg.Parts {
			if part.Type == "text" && strings.Contains(part.Text, encoded) {
				return true
			}
		}
	}
	return false
}

// catalogReaderLLM plays the recovery loop for content hidden by exchange
// omission: list the catalog, then read the artifact it names.
type catalogReaderLLM struct{ calls int }

func (l *catalogReaderLLM) ChatCompletionStream(_ context.Context, req *CompletionRequest, processor EventStreamProcessor) <-chan *messages.StreamEvent {
	var response messages.ChatMessage
	switch l.calls {
	case 0:
		response = messages.ChatMessage{
			Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse,
			ToolCalls: []messages.ChatMessageToolCall{{ID: "list", Name: "list_artifacts", Arguments: `{}`}},
		}
	case 1:
		var id string
		for _, msg := range req.Messages {
			if msg.Role == messages.MessageRoleTool && msg.ToolName == "list_artifacts" {
				id = testArtifactIDPattern.FindString(msg.Content)
			}
		}
		response = messages.ChatMessage{
			Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse,
			ToolCalls: []messages.ChatMessageToolCall{{
				ID: "read", Name: "read_artifact", Arguments: fmt.Sprintf(`{"id":%q,"limit":1}`, id),
			}},
		}
	default:
		response = messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonEndTurn, Content: "done"}
	}
	l.calls++
	input := make(chan messages.ChatMessage, 1)
	input <- response
	close(input)
	return processor.ProcessMessagesToEvents(input)
}

func TestAgentListsAndReadsArtifactFromOmittedExchange(t *testing.T) {
	store := newTestArtifactStore()
	data := "NEEDLE LINE\n" + strings.Repeat("filler body\n", 4_000)
	ref := putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindText, MIMEType: "text/plain", Name: "lookup.txt", Data: []byte(data)})
	agent := NewAgent(&catalogReaderLLM{}, nil, AgentConfig{ArtifactStore: store})
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "old request " + strings.Repeat("x", 4_000)},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "old", Name: "lookup", Arguments: `{}`}}},
		{Role: messages.MessageRoleTool, ToolCallID: "old", ToolName: "lookup", Content: artifactReceipt(ref), Parts: []messages.ContentPart{{Type: "artifact", Artifact: &ref}}},
		{Role: messages.MessageRoleAssistant, Content: "old answer"},
		{Role: messages.MessageRoleUser, Content: "what did that lookup return?"},
	}

	response, err := agent.Run(context.Background(), &CompletionRequest{Messages: history, MaxContextTokens: 600}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.Projection.OmittedExchanges == 0 {
		t.Fatalf("old exchange was not omitted: %+v", response.Projection)
	}
	var listResult, readResult messages.ChatMessage
	for _, msg := range response.AllMessages {
		if msg.Role != messages.MessageRoleTool {
			continue
		}
		switch msg.ToolName {
		case "list_artifacts":
			listResult = msg
		case "read_artifact":
			readResult = msg
		}
	}
	if !strings.Contains(listResult.Content, ref.ID) || !strings.Contains(listResult.Content, "in the order first referenced") {
		t.Fatalf("catalog result = %#v", listResult)
	}
	succeeded, known := readResult.ToolSucceeded()
	if !known || !succeeded || !strings.HasPrefix(readResult.Content, "1: NEEDLE LINE") {
		t.Fatalf("recovered read result = %#v", readResult)
	}
}
