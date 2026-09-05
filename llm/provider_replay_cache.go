package llm

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"sync"
)

// A run owns this cache. Keys include the complete immutable source string so
// loaded histories and caller edits cannot accidentally reuse stale conversions.
// The cap bounds retained source strings and derived JSON independently of run
// length; a full cache is discarded before admitting the next entry.
const providerReplayCacheLimit = 16 << 20

type providerReplayKey struct {
	kind   byte
	source string
}

type providerReplayValue struct {
	raw   json.RawMessage
	valid bool
}

type providerReplayCache struct {
	mu    sync.Mutex
	items map[providerReplayKey]providerReplayValue
	bytes int
}

func (c *providerReplayCache) value(kind byte, source string, build func(string) providerReplayValue) providerReplayValue {
	if c == nil {
		return build(source)
	}
	key := providerReplayKey{kind: kind, source: source}
	c.mu.Lock()
	defer c.mu.Unlock()
	if value, ok := c.items[key]; ok {
		return value
	}
	value := build(source)
	size := len(source) + len(value.raw) + 64
	if size > providerReplayCacheLimit {
		return value
	}
	if c.items == nil || c.bytes+size > providerReplayCacheLimit {
		c.items = make(map[providerReplayKey]providerReplayValue)
		c.bytes = 0
	}
	c.items[key] = value
	c.bytes += size
	return value
}

func requestProviderReplayCache(req *CompletionRequest) *providerReplayCache {
	if req.providerReplayCache != nil {
		return req.providerReplayCache
	}
	// Direct client callers still share conversions within this request,
	// without mutating their request or establishing a global cache.
	return &providerReplayCache{}
}

func (c *providerReplayCache) anthropicInput(source string) json.RawMessage {
	return c.value('a', source, func(source string) providerReplayValue {
		raw := json.RawMessage(strings.TrimSpace(source))
		if len(raw) == 0 || !json.Valid(raw) {
			raw = json.RawMessage("{}")
		}
		return providerReplayValue{raw: raw, valid: true}
	}).raw
}

func (c *providerReplayCache) geminiArguments(source string) (json.RawMessage, bool) {
	value := c.value('g', source, func(source string) providerReplayValue {
		var args map[string]any
		if json.Unmarshal([]byte(source), &args) != nil {
			return providerReplayValue{}
		}
		if len(args) == 0 {
			return providerReplayValue{valid: true}
		}
		// Canonicalize once to retain the existing numeric and duplicate-key
		// behavior, then replay raw JSON without constructing maps each turn.
		raw, _ := json.Marshal(args)
		return providerReplayValue{raw: raw, valid: true}
	})
	return value.raw, value.valid
}

func (c *providerReplayCache) geminiResult(source string) json.RawMessage {
	return c.value('r', source, func(source string) providerReplayValue {
		var output any
		if json.Unmarshal([]byte(source), &output) != nil {
			output = source
		}
		response, ok := output.(map[string]any)
		if !ok {
			response = map[string]any{"result": output}
		}
		if len(response) == 0 {
			return providerReplayValue{valid: true}
		}
		raw, _ := json.Marshal(response)
		return providerReplayValue{raw: raw, valid: true}
	}).raw
}

func (c *providerReplayCache) validGeminiImage(source string) bool {
	return c.value('i', source, func(source string) providerReplayValue {
		// Validate without allocating a full decoded image. The wire payload
		// retains the original base64 instead of encoding those bytes again.
		_, err := io.Copy(io.Discard, base64.NewDecoder(base64.StdEncoding, strings.NewReader(source)))
		return providerReplayValue{valid: err == nil}
	}).valid
}
