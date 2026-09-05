package llm

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/alexschlessinger/pollytool/llm/gemini"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestProviderReplayGeminiWireParity(t *testing.T) {
	for _, source := range []string{
		`{"value":1,"nested":{"x":[true,null,"<>&"]}}`, `{"value":1,"value":2}`,
		`{}`, " { \n } ", `null`, `[]`, `[1,"x"]`, `42`, `"text"`, `true`,
		`{"large":9007199254740993}`, `{"overflow":1e400}`, `1e400`, "", "plain text", `{"broken":`,
	} {
		t.Run(source, func(t *testing.T) {
			history := []messages.ChatMessage{
				{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "c", Name: "f", Arguments: source}}},
				{Role: messages.MessageRoleTool, ToolCallID: "c", ToolName: "f", Content: source},
			}
			want, _, _ := MessagesToGeminiContent(history)
			cache := &providerReplayCache{}
			for range 2 {
				got, _, _ := messagesToGeminiContent(history, cache)
				assertGeminiWireEqual(t, got, want)
			}
		})
	}
}

func assertGeminiWireEqual(t *testing.T, got, want []*gemini.Content) {
	t.Helper()
	gotJSON, err := (&gemini.GenerateContentRequest{Contents: got}).MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, err := (&gemini.GenerateContentRequest{Contents: want}).MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var gotWire, wantWire gemini.GenerateContentRequest
	if err := json.Unmarshal(gotJSON, &gotWire); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(wantJSON, &wantWire); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotWire, wantWire) {
		t.Fatalf("wire changed:\ngot  %s\nwant %s", gotJSON, wantJSON)
	}
}

func TestProviderReplayGeminiImageParity(t *testing.T) {
	for _, encoded := range []string{"", "YQ==", "Yh==", "Zh==", "Zm9=", "YQ==\r\n", "\r\n", "YQ", "YQ==YQ==", "YQ==A", "!invalid"} {
		history := []messages.ChatMessage{{Role: messages.MessageRoleUser, Parts: []messages.ContentPart{
			{Type: "text", Text: "image"},
			{Type: "image_base64", MimeType: "image/png", ImageData: encoded},
		}}}
		want, _, _ := MessagesToGeminiContent(history)
		got, _, _ := messagesToGeminiContent(history, &providerReplayCache{})
		assertGeminiWireEqual(t, got, want)
		gotJSON, _ := (&gemini.GenerateContentRequest{Contents: got}).MarshalJSON()
		wantJSON, _ := (&gemini.GenerateContentRequest{Contents: want}).MarshalJSON()
		var gotValue, wantValue any
		json.Unmarshal(gotJSON, &gotValue)
		json.Unmarshal(wantJSON, &wantValue)
		if !reflect.DeepEqual(gotValue, wantValue) {
			t.Fatalf("base64 wire value changed for %q: got %s want %s", encoded, gotJSON, wantJSON)
		}
	}
}

func TestProviderReplayBase64ValidationParity(t *testing.T) {
	cache := &providerReplayCache{}
	const alphabet = "AB=\r\n!"
	for code := 0; code < 7776; code++ {
		n := code
		var input [5]byte
		for i := range input {
			input[i] = alphabet[n%len(alphabet)]
			n /= len(alphabet)
		}
		source := string(input[:])
		_, err := base64.StdEncoding.DecodeString(source)
		if got := cache.validGeminiImage(source); got != (err == nil) {
			t.Fatalf("validation changed for %q: got %t, DecodeString err=%v", source, got, err)
		}
	}
}

func TestProviderReplayChangedArgumentsAndFallback(t *testing.T) {
	cache := &providerReplayCache{}
	history := []messages.ChatMessage{{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "same", Name: "f"}}}}
	for _, source := range []string{`{"version":1}`, `{"version":2}`, "broken", "", " null ", `{"version":1}`} {
		history[0].ToolCalls[0].Arguments = source
		got, _ := messagesToAnthropicParams(history, cache)
		want, _ := MessagesToAnthropicParams(history)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("cached conversion for %q changed: got %+v, want %+v", source, got, want)
		}
	}
}

func TestProviderReplayCacheBoundAndConcurrentReads(t *testing.T) {
	cache := &providerReplayCache{}
	large := strings.Repeat("x", providerReplayCacheLimit/4)
	for i := range 6 {
		cache.anthropicInput(large + string(rune('a'+i)))
		if cache.bytes > providerReplayCacheLimit {
			t.Fatalf("cache retained %d bytes", cache.bytes)
		}
	}
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			for range 20 {
				if got := string(cache.anthropicInput(`{"x":1}`)); got != `{"x":1}` {
					t.Errorf("cached input = %s", got)
				}
			}
		})
	}
	workers.Wait()
}

func TestOpenAIStructuralSchemaCopyIsolation(t *testing.T) {
	input := map[string]any{
		"type":       "object",
		"properties": map[string]any{"child": map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string"}}}},
		"required":   []string{"child"},
		"anyOf":      []map[string]any{{"type": "object", "properties": map[string]any{"n": map[string]string{"type": "number"}}}},
		"default":    json.RawMessage(`{"a":1}`),
	}
	before, _ := json.Marshal(input)
	copy := deepCopyMap(input)
	normalizeStrictJSONSchema(copy)
	after, _ := json.Marshal(input)
	if string(before) != string(after) {
		t.Fatalf("normalization mutated input:\nbefore %s\nafter %s", before, after)
	}
	if copy["additionalProperties"] != false || copy["anyOf"].([]any)[0].(map[string]any)["additionalProperties"] != false {
		t.Fatalf("typed schema containers were not normalized: %+v", copy)
	}
}

func TestOpenAIStructuralSchemaCopyCycles(t *testing.T) {
	cycleMap := map[string]any{}
	cycleMap["self"] = cycleMap
	cycleSlice := make([]any, 1)
	cycleSlice[0] = cycleSlice
	for _, cycle := range []any{cycleMap, cycleSlice} {
		original := map[string]any{"type": "object", "x-annotation": cycle}
		copied := deepCopyMap(original)
		copied["type"] = "string"
		if original["type"] != "object" {
			t.Fatal("fallback aliases the original top-level map")
		}
		if _, err := json.Marshal(copied); err == nil {
			t.Fatal("fallback dropped the cyclic annotation")
		}
	}
}

func BenchmarkOpenAISchemaCopy(b *testing.B) {
	properties := make(map[string]any, 50)
	for i := range 50 {
		properties[string(rune('A'+i))] = map[string]any{"type": "string", "description": strings.Repeat("a", 128)}
	}
	input := map[string]any{"type": "object", "properties": properties, "required": []string{"A"}}
	for _, structural := range []bool{false, true} {
		name := "json"
		if structural {
			name = "structural"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if structural {
					_ = deepCopyMap(input)
				} else {
					raw, _ := json.Marshal(input)
					var copy map[string]any
					if err := json.Unmarshal(raw, &copy); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}

func BenchmarkProviderReplay(b *testing.B) {
	text := `{"output":"` + strings.Repeat("x", 64<<10) + `"}`
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "c", Name: "f", Arguments: text}}},
		{Role: messages.MessageRoleTool, ToolCallID: "c", ToolName: "f", Content: text},
	}
	for _, provider := range []string{"gemini", "anthropic", "gemini_image"} {
		for _, cached := range []bool{false, true} {
			name := provider + "/uncached"
			var cache *providerReplayCache
			if cached {
				name = provider + "/cached"
				cache = &providerReplayCache{}
			}
			b.Run(name, func(b *testing.B) {
				msgs := history
				if provider == "gemini_image" {
					msgs = []messages.ChatMessage{{Role: messages.MessageRoleUser, Parts: []messages.ContentPart{{Type: "image_base64", MimeType: "image/png", ImageData: base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 256<<10)))}}}}
				}
				encode := func() {
					var contents any
					if provider == "anthropic" {
						contents, _ = messagesToAnthropicParams(msgs, cache)
					} else {
						contents, _, _ = messagesToGeminiContent(msgs, cache)
					}
					var err error
					if provider == "anthropic" {
						_, err = json.Marshal(contents)
					} else {
						_, err = (&gemini.GenerateContentRequest{Contents: contents.([]*gemini.Content)}).MarshalJSON()
					}
					if err != nil {
						b.Fatal(err)
					}
				}
				encode()
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					encode()
				}
			})
		}
	}
}
