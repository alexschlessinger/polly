package openai

import (
	"encoding/json"
	"testing"
)

// Prompt-cache accounting arrives in three shapes and a read can reach us at a
// different level from the write that accompanies it: OpenAI nests both, a
// gateway fronting an Anthropic model nests the read and reports the write
// under Anthropic's own top-level key, and DeepSeek reports only a hit count
// whose misses are ordinary input rather than a cache write.
func TestChatUsagePromptCacheShapes(t *testing.T) {
	for _, tc := range []struct {
		name        string
		body        string
		read, write int
		reported    bool
	}{
		{name: "absent", body: `{"prompt_tokens":10}`},
		{name: "openai nested read", body: `{"prompt_tokens_details":{"cached_tokens":64}}`, read: 64, reported: true},
		{name: "openai nested read and write", body: `{"prompt_tokens_details":{"cached_tokens":64,"cache_write_tokens":8}}`, read: 64, write: 8, reported: true},
		{name: "gateway anthropic write beside nested read", body: `{"prompt_tokens_details":{"cached_tokens":6656},"cache_creation_input_tokens":1024}`, read: 6656, write: 1024, reported: true},
		{name: "gateway anthropic write alone", body: `{"cache_creation_input_tokens":1024}`, write: 1024, reported: true},
		{name: "deepseek hits only", body: `{"prompt_cache_hit_tokens":32,"prompt_cache_miss_tokens":9}`, read: 32, reported: true},
		{name: "deepseek misses are not writes", body: `{"prompt_cache_miss_tokens":9}`, reported: true},
		{name: "empty details fall through to deepseek", body: `{"prompt_tokens_details":{},"prompt_cache_hit_tokens":32}`, read: 32, reported: true},
		{name: "implicit cache reports no write", body: `{"prompt_tokens_details":{"cached_tokens":6656}}`, read: 6656, reported: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var usage ChatUsage
			if err := json.Unmarshal([]byte(tc.body), &usage); err != nil {
				t.Fatal(err)
			}
			read, write, reported := usage.PromptCacheUsage()
			if read != tc.read || write != tc.write || reported != tc.reported {
				t.Fatalf("PromptCacheUsage() = %d, %d, %t; want %d, %d, %t", read, write, reported, tc.read, tc.write, tc.reported)
			}
		})
	}
	if read, write, reported := (*ChatUsage)(nil).PromptCacheUsage(); read != 0 || write != 0 || reported {
		t.Fatalf("nil usage = %d, %d, %t", read, write, reported)
	}
}
