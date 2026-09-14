package anthropic

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestReplayChangedArgumentsAndFallback(t *testing.T) {
	cache := &contract.ReplayCache{}
	history := []messages.ChatMessage{{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "same", Name: "f"}}}}
	for _, source := range []string{`{"version":1}`, `{"version":2}`, "broken", "", " null ", `{"version":1}`} {
		history[0].ToolCalls[0].Arguments = source
		got, _ := messagesToParams(history, cache)
		want, _ := messagesToParams(history, nil)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("cached conversion for %q changed: got %+v, want %+v", source, got, want)
		}
	}
}

func BenchmarkReplay(b *testing.B) {
	text := `{"output":"` + strings.Repeat("x", 64<<10) + `"}`
	history := []messages.ChatMessage{
		{Role: messages.MessageRoleAssistant, ToolCalls: []messages.ChatMessageToolCall{{ID: "c", Name: "f", Arguments: text}}},
		{Role: messages.MessageRoleTool, ToolCallID: "c", ToolName: "f", Content: text},
	}
	for _, cached := range []bool{false, true} {
		name := "uncached"
		var cache *contract.ReplayCache
		if cached {
			name = "cached"
			cache = &contract.ReplayCache{}
		}
		b.Run(name, func(b *testing.B) {
			encode := func() {
				contents, _ := messagesToParams(history, cache)
				if _, err := json.Marshal(contents); err != nil {
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
