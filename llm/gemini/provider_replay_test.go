package gemini

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestProviderReplayWireParity(t *testing.T) {
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
			want, _ := messagesToContent(history, &contract.ReplayCache{})
			cache := &contract.ReplayCache{}
			for range 2 {
				got, _ := messagesToContent(history, cache)
				assertWireEqual(t, got, want)
			}
		})
	}
}

func assertWireEqual(t *testing.T, got, want []*Content) {
	t.Helper()
	gotJSON, err := (&GenerateContentRequest{Contents: got}).MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, err := (&GenerateContentRequest{Contents: want}).MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var gotWire, wantWire GenerateContentRequest
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

func TestProviderReplayImageParity(t *testing.T) {
	for _, encoded := range []string{"", "YQ==", "Yh==", "Zh==", "Zm9=", "YQ==\r\n", "\r\n", "YQ", "YQ==YQ==", "YQ==A", "!invalid"} {
		history := []messages.ChatMessage{{Role: messages.MessageRoleUser, Parts: []messages.ContentPart{
			{Type: "text", Text: "image"},
			{Type: "image_base64", MimeType: "image/png", ImageData: encoded},
		}}}
		want, _ := messagesToContent(history, &contract.ReplayCache{})
		got, _ := messagesToContent(history, &contract.ReplayCache{})
		assertWireEqual(t, got, want)
		gotJSON, _ := (&GenerateContentRequest{Contents: got}).MarshalJSON()
		wantJSON, _ := (&GenerateContentRequest{Contents: want}).MarshalJSON()
		var gotValue, wantValue any
		json.Unmarshal(gotJSON, &gotValue)
		json.Unmarshal(wantJSON, &wantValue)
		if !reflect.DeepEqual(gotValue, wantValue) {
			t.Fatalf("base64 wire value changed for %q: got %s want %s", encoded, gotJSON, wantJSON)
		}
	}
}

func TestProviderReplayBase64ValidationParity(t *testing.T) {
	cache := &contract.ReplayCache{}
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
		if got := cache.ValidGeminiImage(source); got != (err == nil) {
			t.Fatalf("validation changed for %q: got %t, DecodeString err=%v", source, got, err)
		}
	}
}

func BenchmarkReplay(b *testing.B) {
	text := `{"output":"` + strings.Repeat("x", 64<<10) + `"}`
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "c", Name: "f", Arguments: text}}},
		{Role: messages.MessageRoleTool, ToolCallID: "c", ToolName: "f", Content: text},
	}
	for _, provider := range []string{"gemini", "gemini_image"} {
		for _, cached := range []bool{false, true} {
			name := provider + "/uncached"
			var cache *contract.ReplayCache
			if cached {
				name = provider + "/cached"
				cache = &contract.ReplayCache{}
			}
			b.Run(name, func(b *testing.B) {
				msgs := history
				if provider == "gemini_image" {
					msgs = []messages.ChatMessage{{Role: messages.MessageRoleUser, Parts: []messages.ContentPart{{Type: "image_base64", MimeType: "image/png", ImageData: base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 256<<10)))}}}}
				}
				encode := func() {
					contents, _ := messagesToContent(msgs, cache)
					if _, err := (&GenerateContentRequest{Contents: contents}).MarshalJSON(); err != nil {
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
