package contract

import (
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
