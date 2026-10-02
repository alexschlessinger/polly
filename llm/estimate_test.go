package llm

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm/anthropic"
)

// jsonLength stands in for encoding replayed reasoning: it must come within
// a few bytes per item of the encoded length.
func TestJSONLengthMatchesTheEncoding(t *testing.T) {
	thinking := strings.Repeat("t", 4_000)
	for name, value := range map[string]any{
		"thinking blocks": []map[string]any{{"type": "thinking", "thinking": thinking, "signature": "sig"}, {"type": "redacted_thinking", "data": "abc"}},
		"reasoning items": []any{map[string]any{"type": "reasoning", "encrypted_content": thinking, "summary": []any{}}},
		"signatures":      map[string]string{"call_1": thinking},
		"raw details":     map[string]any{"endpoint": "https://openrouter.ai/api/v1", "reasoning_details": json.RawMessage(`[{"type":"reasoning.encrypted","data":"` + thinking + `"}]`)},
	} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := jsonLength(value), len(raw); got < want-16 || got > want+16 {
			t.Errorf("%s: jsonLength = %d, encoded %d", name, got, want)
		}
	}
}

func BenchmarkReasoningReplayTokens(b *testing.B) {
	msg := reply("answer", 0)
	msg.Metadata = map[string]any{anthropic.ThinkingBlocksKey: []map[string]any{{"type": "thinking", "thinking": strings.Repeat("t", 4_000), "signature": strings.Repeat("s", 400)}}}
	for b.Loop() {
		reasoningReplayTokens(msg)
	}
}
