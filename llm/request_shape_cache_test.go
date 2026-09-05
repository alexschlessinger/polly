package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/tools"
)

func TestRequestShapeCacheTracksMutableSchemas(t *testing.T) {
	enum := []string{"small", "large"}
	definition := schema.Tool("choose", "choose a size", schema.Params{
		"size": map[string]any{"type": "string", "enum": enum},
	})
	tool := testTool{name: "choose", schema: definition}
	req := &CompletionRequest{
		Model: "model", Temperature: Float32Ptr(0.2), Tools: []tools.Tool{tool},
		Messages:       messages.User("hello"),
		ResponseSchema: &Schema{Raw: map[string]any{"type": "object"}},
	}
	req.shapeCache = newRequestShapeCache(req.Messages)
	key := func() string {
		t.Helper()
		req.shapeCache.prepareTools(req.Tools)
		got, err := derivePromptCacheKey(req, req.Messages)
		if err != nil {
			t.Fatal(err)
		}
		uncached := *req
		uncached.shapeCache = nil
		want, err := derivePromptCacheKey(&uncached, req.Messages)
		if err != nil || got != want {
			t.Fatalf("cached key %q != current uncached key %q: %v", got, want, err)
		}
		if got, want := estimateRequestToolSchemaTokens(req), estimateToolSchemaTokens(req.Tools); got != want {
			t.Fatalf("cached tokens = %d, current tokens = %d", got, want)
		}
		return got
	}
	before := key()
	entry := req.shapeCache.tools[0]
	if again := key(); again != before || req.shapeCache.tools[0] != entry {
		t.Fatal("unchanged schema was not reused")
	}
	for _, change := range []struct {
		name  string
		apply func()
	}{
		{"nested slice", func() { enum[0] = strings.Repeat("changed", 100) }},
		{"nested map", func() {
			definition.Raw["properties"].(map[string]any)["size"].(map[string]any)["description"] = "new description"
		}},
		{"strict", func() { definition.Strict = !definition.Strict }},
		{"response schema", func() { req.ResponseSchema.Raw["description"] = "new response description" }},
		{"temperature pointer", func() { *req.Temperature = 0.8 }},
		{"model", func() { req.Model = "other-model" }},
		{"tool addition", func() {
			req.Tools = append(req.Tools, testTool{name: "new", schema: schema.Tool("new", "new tool", nil)})
		}},
		{"tool removal", func() { req.Tools = req.Tools[:1] }},
	} {
		t.Run(change.name, func(t *testing.T) {
			change.apply()
			after := key()
			if after == before {
				t.Fatal("schema/request change reused stale prompt key")
			}
			before = after
		})
	}
}

func TestPromptCacheKeyPreservesCanonicalShape(t *testing.T) {
	req := &CompletionRequest{Model: "model", Tools: []tools.Tool{
		testTool{name: "first", schema: &schema.ToolSchema{Raw: map[string]any{"title": "same", "description": "z"}}},
		testTool{name: "second", schema: &schema.ToolSchema{Raw: map[string]any{"title": "same", "description": "a"}}},
	}}
	history := []messages.ChatMessage{{Role: messages.MessageRoleSystem, Content: "system"}}
	// This is the pre-cache canonical JSON, including equal-title ordering.
	canonical := `{"version":"polly-prompt-cache-v1","model":"model","system":["system"],"tools":[{"name":"same","strict":false,"schema":{"description":"a","title":"same"}},{"name":"same","strict":false,"schema":{"description":"z","title":"same"}}],"thinking_effort":"off"}`
	digest := sha256.Sum256([]byte(canonical))
	want := hex.EncodeToString(digest[:])
	got, err := derivePromptCacheKey(req, history)
	if err != nil || got != want {
		t.Fatalf("prompt key = %q, want %q: %v", got, want, err)
	}
}

type changingSchemaJSON struct{ text *string }

func (v changingSchemaJSON) MarshalJSON() ([]byte, error) { return json.Marshal(*v.text) }

func TestRequestShapeCacheDoesNotFreezeCustomMarshaler(t *testing.T) {
	text := "first"
	req := &CompletionRequest{Tools: []tools.Tool{testTool{name: "custom", schema: &schema.ToolSchema{Raw: map[string]any{
		"title": "custom", "description": changingSchemaJSON{&text},
	}}}}}
	req.shapeCache = newRequestShapeCache(nil)
	req.shapeCache.prepareTools(req.Tools)
	first, err := derivePromptCacheKey(req, nil)
	if err != nil {
		t.Fatal(err)
	}
	text = "changed"
	req.shapeCache.prepareTools(req.Tools)
	second, err := derivePromptCacheKey(req, nil)
	if err != nil || first == second {
		t.Fatalf("custom schema marshaler change was cached: %q %q %v", first, second, err)
	}
}

func TestAgentRefreshesPromptKeyAfterToolChanges(t *testing.T) {
	model := &promptCacheRecordingLLM{responses: []messages.ChatMessage{{
		Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse,
		ToolCalls: []messages.ChatMessageToolCall{{ID: "change", Name: "load", Arguments: `{}`}},
	}}}
	registry := tools.NewToolRegistry(nil)
	registry.Register(&tools.Func{Name: "load", Run: func(context.Context, tools.Args) (string, error) {
		registry.Register(&tools.Func{Name: "loaded", Desc: "new tool"})
		return "loaded", nil
	}})
	agent := NewAgent(model, registry, AgentConfig{MaxIterations: 2})
	defer agent.Close()
	if _, err := agent.Run(context.Background(), &CompletionRequest{Messages: messages.User("load a tool")}, nil); err != nil {
		t.Fatal(err)
	}
	if len(model.requests) != 2 || model.requests[0].PromptCacheKey == model.requests[1].PromptCacheKey {
		t.Fatal("tool loading reused the earlier prompt cache key")
	}
}

func TestAgentOwnsHistoryBeforeRequestCallbacks(t *testing.T) {
	model := &promptCacheRecordingLLM{}
	agent := NewAgent(model, nil, AgentConfig{MaxIterations: 1})
	req := &CompletionRequest{Messages: []messages.ChatMessage{
		{Role: messages.MessageRoleSystem, Parts: []messages.ContentPart{{Type: "text", Text: "original system"}}},
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "old", Name: "run", Arguments: `{"value":"original"}`}}},
		{Role: messages.MessageRoleTool, ToolCallID: "old", ToolName: "run", Content: "old result"},
		{Role: messages.MessageRoleUser, Content: "question"},
	}}
	_, err := agent.Run(context.Background(), req, &AgentCallbacks{BeforeFirstRequest: func(ProjectionStats) error {
		req.Messages[0].Parts[0].Text = "mutated caller system"
		req.Messages[1].ToolCalls[0].Arguments = `{"value":"mutated"}`
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(model.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(model.requests))
	}
	got := model.requests[0].Messages
	if got[0].GetContent() != "original system" || got[1].ToolCalls[0].Arguments != `{"value":"original"}` {
		t.Fatalf("caller mutation leaked into the owned request history: %#v", got)
	}
}

// Keep the pre-cache algorithm as the benchmark baseline. Its map-backed shape
// serializes schemas directly and does not pay for the new snapshot machinery.
func benchmarkUncachedPromptKey(req *CompletionRequest) (string, error) {
	type toolShape struct {
		Name   string         `json:"name"`
		Strict bool           `json:"strict"`
		Raw    map[string]any `json:"schema"`
	}
	shape := struct {
		Version        string      `json:"version"`
		Model          string      `json:"model"`
		System         []string    `json:"system"`
		Tools          []toolShape `json:"tools"`
		ThinkingEffort string      `json:"thinking_effort"`
	}{Version: promptCacheKeyVersion, Model: req.Model, ThinkingEffort: req.ThinkingEffort.String()}
	for _, msg := range req.Messages {
		if msg.Role == messages.MessageRoleSystem {
			shape.System = append(shape.System, msg.GetContent())
		}
	}
	for _, tool := range req.Tools {
		s := tool.GetSchema()
		shape.Tools = append(shape.Tools, toolShape{s.Title(), s.Strict, s.Raw})
	}
	sort.SliceStable(shape.Tools, func(i, j int) bool { return shape.Tools[i].Name < shape.Tools[j].Name })
	encoded, err := json.Marshal(shape)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func BenchmarkRequestShapeCache(b *testing.B) {
	for _, cached := range []bool{false, true} {
		b.Run(fmt.Sprintf("cached=%t", cached), func(b *testing.B) {
			req := &CompletionRequest{Messages: []messages.ChatMessage{{Role: messages.MessageRoleSystem, Content: strings.Repeat("instructions ", 10000)}}}
			for i := 0; i < 50; i++ {
				name := fmt.Sprintf("tool_%d", i)
				req.Tools = append(req.Tools, testTool{name: name, schema: schema.Tool(name, strings.Repeat("description ", 100), schema.Params{"value": schema.S("value")})})
			}
			if cached {
				req.shapeCache = newRequestShapeCache(req.Messages)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var err error
				if cached {
					req.shapeCache.prepareTools(req.Tools)
					_, err = derivePromptCacheKey(req, req.Messages)
				} else {
					estimateToolSchemaTokens(req.Tools)
					_, err = benchmarkUncachedPromptKey(req)
				}
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
