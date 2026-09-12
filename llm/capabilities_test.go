package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestCapabilityProjectionPreservesHistoryAndIsIdempotent(t *testing.T) {
	ref := artifacts.RefForBlob(artifacts.Blob{Kind: artifacts.KindImage, MIMEType: "image/png", Data: []byte("unread bytes")})
	ref.ImageToken = "[image #2]"
	req := &CompletionRequest{Model: "test/text", Temperature: Float32Ptr(.7), ThinkingEffort: EffortLevel(LevelHigh), Tools: []tools.Tool{&tools.Func{Name: "lookup"}}, Messages: []messages.ChatMessage{
		{Role: messages.MessageRoleUser, Content: "view [image #1]", Parts: []messages.ContentPart{{Type: "image_base64", ImageData: "private-image-bytes", MimeType: "image/png", Reference: "[image #1]"}}},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "call", Name: "lookup", Arguments: `{"q":"find"}`}}},
		{Role: messages.MessageRoleTool, ToolCallID: "call", ToolName: "lookup", Content: "found", Parts: []messages.ContentPart{{Type: "image_url", ImageURL: "https://private.example/image.png"}, {Type: "image_artifact", Artifact: &ref}}},
		{Role: messages.MessageRoleUser, Content: "compare [image #1] and [image #2]"},
	}}
	before := cloneMessages(req.Messages)
	caps := ModelCapabilities{InputModalities: []string{"text"}, Tools: truth(false), Reasoning: truth(false), Parameters: map[string]bool{"temperature": false}}
	out, notes, err := PrepareCapabilities(req, caps, false)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out.Messages)
	for _, bad := range []string{"private-image-bytes", "https://private.example/image.png", "image_artifact", "tool_calls", "tool_call_id"} {
		if strings.Contains(string(raw), bad) {
			t.Fatalf("leaked %s: %s", bad, raw)
		}
	}
	if !strings.Contains(string(raw), "call (lookup)") || !strings.Contains(string(raw), "cannot view images") {
		t.Fatal("lost associated text")
	}
	if !reflect.DeepEqual(req.Messages, before) || req.Temperature == nil || !req.ThinkingEffort.IsEnabled() || len(req.Tools) != 1 {
		t.Fatal("mutated caller")
	}
	if len(out.Tools) != 0 || out.Temperature != nil || out.ThinkingEffort.IsEnabled() || len(notes) != 4 || notes[0].Count != 3 {
		t.Fatalf("adaptation: %+v", notes)
	}
	again, notes, err := PrepareCapabilities(out, caps, false)
	if err != nil || len(notes) != 0 || !reflect.DeepEqual(again.Messages, out.Messages) {
		t.Fatal("not idempotent")
	}
	projected, stats, err := projectCompletionRequest(context.Background(), again, nil, projectionTools{})
	if err != nil || stats.HydratedImages != 0 || len(projected) == 0 {
		t.Fatalf("text reference projection: %+v %v", stats, err)
	}
}

func TestCapabilityContractsAndUnknowns(t *testing.T) {
	req := &CompletionRequest{Model: "custom/m", Temperature: Float32Ptr(.5), ThinkingEffort: EffortLevel(LevelHigh), ResponseSchema: &Schema{}}
	if _, _, err := PrepareCapabilities(req, ModelCapabilities{StructuredOutput: truth(false)}, false); err == nil {
		t.Fatal("schema downgraded")
	}
	if _, _, err := PrepareCapabilities(req, ModelCapabilities{Tools: truth(false)}, true); err == nil {
		t.Fatal("required tools downgraded")
	}
	out, notes, err := PrepareCapabilities(req, ModelCapabilities{}, false)
	if err != nil || len(notes) != 0 || out.Temperature == nil || !out.ThinkingEffort.IsEnabled() {
		t.Fatal("unknown changed request")
	}
	// Partial reasoning declarations must not become complete exclusions.
	out, _, _ = PrepareCapabilities(req, ModelCapabilities{ReasoningEfforts: []string{"low"}}, false)
	if !out.ThinkingEffort.IsEnabled() {
		t.Fatal("partial effort declaration enforced")
	}
	out, _, _ = PrepareCapabilities(req, ModelCapabilities{ReasoningEfforts: []string{"low"}, ReasoningEffortsComplete: true}, false)
	if out.ThinkingEffort.IsEnabled() {
		t.Fatal("complete effort declaration ignored")
	}
}

type metadataRecordingLLM struct {
	recordingSequentialLLM
	info ModelInfo
}

func (m *metadataRecordingLLM) GetModelInfo(context.Context, ModelTarget) (*ModelInfo, error) {
	return &m.info, nil
}

func TestTextOnlyAgentNeverHydratesImagesAndHonorsContextBudget(t *testing.T) {
	store := newTestArtifactStore()
	ref := putTestArtifact(t, store, artifacts.Blob{Kind: artifacts.KindImage, MIMEType: "image/png", Data: []byte("stored original")})
	ref.ImageToken = "[image #1]"
	model := &metadataRecordingLLM{info: ModelInfo{ModelCapabilities: ModelCapabilities{InputModalities: []string{"text"}, Tools: truth(false)}}}
	agent := NewAgent(model, nil, AgentConfig{ArtifactStore: store})
	defer agent.Close()
	history := []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "view [image #1]", Parts: []messages.ContentPart{imageArtifactPart(ref)}}}
	original := cloneMessages(history)
	var notes []RequestAdaptation
	result, err := agent.Run(context.Background(), &CompletionRequest{Model: "custom/m", Messages: history}, &AgentCallbacks{OnAdaptation: func(n RequestAdaptation) { notes = append(notes, n) }})
	if err != nil || len(model.requests) != 1 || result.Projection.HydratedImages != 0 {
		t.Fatalf("run: %+v %v", result, err)
	}
	if !reflect.DeepEqual(history, original) || len(store.blobs) != 1 {
		t.Fatal("original lost")
	}
	raw, _ := json.Marshal(model.requests)
	if strings.Contains(string(raw), "image_artifact") || strings.Contains(string(raw), "stored original") {
		t.Fatalf("media sent: %s", raw)
	}
	if len(notes) == 0 || notes[0].Feature != "images" {
		t.Fatalf("no notice: %+v", notes)
	}
	// Switching back to a vision-capable model restores media from the same history.
	model.info.InputModalities = nil
	vision, err := agent.Run(context.Background(), &CompletionRequest{Model: "custom/vision", Messages: history}, nil)
	if err != nil || vision.Projection.HydratedImages != 1 {
		t.Fatalf("vision switch: %+v %v", vision, err)
	}
	n := 2000
	model.info.ContextTokens = &n
	model.info.InputModalities = []string{"text"}
	_, err = agent.Run(context.Background(), &CompletionRequest{Messages: messages.User(strings.Repeat("x", 20000)), MaxContextTokens: 100000}, nil)
	if err != nil {
		t.Fatalf("explicit context budget was overridden by metadata: %v", err)
	}
}

func TestMultiPassWireHostRoutingAndTextOnlyAdaptation(t *testing.T) {
	for _, provider := range []string{"huggingface", "openrouter"} {
		t.Run(provider, func(t *testing.T) {
			var body map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/chat/completions" {
					http.Error(w, "unexpected metadata lookup", 500)
					return
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
			}))
			defer server.Close()
			model := provider + "/org/m"
			host := "host/turbo"
			if provider == "huggingface" {
				model += ":cerebras"
				host = ""
			}
			stream := false
			caps := ModelCapabilities{InputModalities: []string{"text"}, Tools: truth(false)}
			req := &CompletionRequest{Model: model, ModelHost: host, BaseURL: server.URL, APIKey: "fixture", Stream: &stream, Capabilities: &caps, Messages: []messages.ChatMessage{{Role: messages.MessageRoleUser, Parts: []messages.ContentPart{{Type: "image_url", ImageURL: "https://secret/image"}}}}}
			agent := NewAgent(NewMultiPass(nil), nil, AgentConfig{})
			defer agent.Close()
			if _, err := agent.Run(context.Background(), req, nil); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(body)
			if strings.Contains(string(raw), "https://secret/image") || strings.Contains(string(raw), "image_url") {
				t.Fatalf("media leaked: %s", raw)
			}
			if provider == "huggingface" {
				if body["model"] != "org/m:cerebras" || body["provider"] != nil {
					t.Fatalf("HF route: %s", raw)
				}
			} else {
				route := body["provider"].(map[string]any)
				if route["allow_fallbacks"] != false || !reflect.DeepEqual(route["only"], []any{"host/turbo"}) || body["model"] != "org/m" {
					t.Fatalf("OR route: %s", raw)
				}
			}
		})
	}
}

func TestTextOnlyToolContinuationRetainsOriginalMedia(t *testing.T) {
	store := newTestArtifactStore()
	rich := &testRichTool{name: "render", output: tools.ToolOutput{Text: "rendered", Media: []tools.ToolMedia{{Data: []byte("image bytes"), MIMEType: "image/png", Name: "render.png"}}}}
	registry := tools.NewToolRegistry([]tools.Tool{rich})
	defer registry.Close()
	model := &metadataRecordingLLM{info: ModelInfo{ModelCapabilities: ModelCapabilities{InputModalities: []string{"text"}, Tools: truth(true)}}, recordingSequentialLLM: recordingSequentialLLM{responses: []messages.ChatMessage{{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, ToolCalls: []messages.ChatMessageToolCall{{ID: "render", Name: "render", Arguments: `{}`}}}}}}
	agent := NewAgent(model, registry, AgentConfig{ArtifactStore: store})
	defer agent.Close()
	result, err := agent.Run(context.Background(), &CompletionRequest{Model: "custom/text", Messages: messages.User("render it")}, nil)
	if err != nil || len(model.requests) != 2 {
		t.Fatalf("continuation: %v", err)
	}
	for _, batch := range model.requests {
		for _, msg := range batch {
			for _, part := range msg.Parts {
				if isImagePart(part) || part.ImageURL != "" || part.ImageData != "" {
					t.Fatal("tool media reached text-only provider")
				}
			}
		}
	}
	found := false
	for _, msg := range result.AllMessages {
		for _, part := range msg.Parts {
			if isImagePart(part) {
				found = true
			}
		}
	}
	if !found || len(store.blobs) == 0 {
		t.Fatal("durable tool image was lost")
	}
}
