package messages

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOpenRouterMetadataDecodeKeepsOtherNamespacesUnchanged(t *testing.T) {
	for _, origin := range []bool{true, false} {
		fields := ""
		if origin {
			fields = `"endpoint":"https://openrouter.ai/api/v1","requested_model":"m",`
		}
		raw := `{"role":"assistant","metadata":{"input_tokens":123,"unrelated":{"value":45},"openrouter":{` + fields + `"reasoning_details":[{"opaque":9007199254740993}]}}}`
		var msg ChatMessage
		if err := json.Unmarshal([]byte(raw), &msg); err != nil {
			t.Fatal(err)
		}
		if _, ok := msg.Metadata["input_tokens"].(float64); !ok {
			t.Fatal("ordinary metadata changed type")
		}
		meta := msg.Metadata["openrouter"].(map[string]any)
		value := meta["reasoning_details"].([]any)[0].(map[string]any)["opaque"]
		if origin {
			if value != json.Number("9007199254740993") {
				t.Fatalf("opaque number: %v", value)
			}
			out, _ := json.Marshal(msg)
			if !strings.Contains(string(out), "9007199254740993") {
				t.Fatal("serialized precision lost")
			}
			if err := json.Unmarshal([]byte(`{"content":"updated"}`), &msg); err != nil {
				t.Fatalf("partial decode changed: %v", err)
			}
			if msg.Metadata["openrouter"] == nil || msg.Content != "updated" {
				t.Fatal("partial decode lost existing fields")
			}
		} else if _, ok := value.(float64); !ok {
			t.Fatal("unattributed historical object changed")
		}
	}
}
