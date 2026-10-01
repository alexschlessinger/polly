package contract

import (
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestReplayCacheBoundAndConcurrentReads(t *testing.T) {
	cache := &ReplayCache{}
	large := strings.Repeat("x", replayCacheLimit/4)
	for i := range 6 {
		cache.AnthropicInput(large + string(rune('a'+i)))
		if cache.bytes > replayCacheLimit {
			t.Fatalf("cache retained %d bytes", cache.bytes)
		}
	}
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			for range 20 {
				if got := string(cache.AnthropicInput(`{"x":1}`)); got != `{"x":1}` {
					t.Errorf("cached input = %s", got)
				}
			}
		})
	}
	workers.Wait()
}

// Arguments that are not a JSON object replay as one holding the raw text, so
// no provider drops the call or reports arguments the model did not send.
func TestInvalidToolArgumentsReplayAsAnObject(t *testing.T) {
	var cache ReplayCache
	for _, raw := range []string{`{"q": 1`, `[1, 2]`, `not json`, `"text"`} {
		want := `{"invalid_arguments":` + strconv.Quote(raw) + `}`
		if got := string(cache.AnthropicInput(raw)); got != want {
			t.Errorf("anthropic input for %q = %s, want %s", raw, got, want)
		}
		if got := string(cache.GeminiArguments(raw)); got != want {
			t.Errorf("gemini arguments for %q = %s, want %s", raw, got, want)
		}
		if args := ToolArguments(raw); args[InvalidArgumentsKey] != raw {
			t.Errorf("tool arguments for %q = %v", raw, args)
		}
	}
	if got := string(cache.AnthropicInput(`{"q":1}`)); got != `{"q":1}` {
		t.Errorf("valid input changed: %s", got)
	}
	// A call without arguments, which adapters record as "" or "null",
	// replays with none.
	for _, raw := range []string{"", "null", " null "} {
		if args := ToolArguments(raw); len(args) != 0 {
			t.Errorf("arguments %q = %v", raw, args)
		}
		if got := cache.GeminiArguments(raw); got != nil {
			t.Errorf("gemini arguments for %q = %s, want none", raw, got)
		}
		if got := string(cache.AnthropicInput(raw)); got != `{}` {
			t.Errorf("anthropic input for %q = %s, want {}", raw, got)
		}
	}
}
