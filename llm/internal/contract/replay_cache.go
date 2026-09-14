package contract

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"sync"
)

// ReplayCache memoizes provider-side conversions of immutable message
// sources for one run. Keys include the complete immutable source string so
// loaded histories and caller edits cannot accidentally reuse stale conversions.
// The cap bounds retained source strings and derived JSON independently of run
// length; a full cache is discarded before admitting the next entry.
const replayCacheLimit = 16 << 20

type replayKey struct {
	kind   byte
	source string
}

type replayValue struct {
	raw   json.RawMessage
	valid bool
}

// ReplayCache caches converted tool arguments, results, and image validity
// keyed by their source text. The zero value is ready to use; a nil cache
// converts without memoizing.
type ReplayCache struct {
	mu    sync.Mutex
	items map[replayKey]replayValue
	bytes int
}

func (c *ReplayCache) value(kind byte, source string, build func(string) replayValue) replayValue {
	if c == nil {
		return build(source)
	}
	key := replayKey{kind: kind, source: source}
	c.mu.Lock()
	defer c.mu.Unlock()
	if value, ok := c.items[key]; ok {
		return value
	}
	value := build(source)
	size := len(source) + len(value.raw) + 64
	if size > replayCacheLimit {
		return value
	}
	if c.items == nil || c.bytes+size > replayCacheLimit {
		c.items = make(map[replayKey]replayValue)
		c.bytes = 0
	}
	c.items[key] = value
	c.bytes += size
	return value
}

func (c *ReplayCache) AnthropicInput(source string) json.RawMessage {
	return c.value('a', source, func(source string) replayValue {
		raw := json.RawMessage(strings.TrimSpace(source))
		if len(raw) == 0 || !json.Valid(raw) {
			raw = json.RawMessage("{}")
		}
		return replayValue{raw: raw, valid: true}
	}).raw
}

func (c *ReplayCache) GeminiArguments(source string) (json.RawMessage, bool) {
	value := c.value('g', source, func(source string) replayValue {
		var args map[string]any
		if json.Unmarshal([]byte(source), &args) != nil {
			return replayValue{}
		}
		if len(args) == 0 {
			return replayValue{valid: true}
		}
		// Canonicalize once to retain the existing numeric and duplicate-key
		// behavior, then replay raw JSON without constructing maps each turn.
		raw, _ := json.Marshal(args)
		return replayValue{raw: raw, valid: true}
	})
	return value.raw, value.valid
}

func (c *ReplayCache) GeminiResult(source string) json.RawMessage {
	return c.value('r', source, func(source string) replayValue {
		var output any
		if json.Unmarshal([]byte(source), &output) != nil {
			output = source
		}
		response, ok := output.(map[string]any)
		if !ok {
			response = map[string]any{"result": output}
		}
		if len(response) == 0 {
			return replayValue{valid: true}
		}
		raw, _ := json.Marshal(response)
		return replayValue{raw: raw, valid: true}
	}).raw
}

func (c *ReplayCache) ValidGeminiImage(source string) bool {
	return c.value('i', source, func(source string) replayValue {
		// Validate without allocating a full decoded image. The wire payload
		// retains the original base64 instead of encoding those bytes again.
		_, err := io.Copy(io.Discard, base64.NewDecoder(base64.StdEncoding, strings.NewReader(source)))
		return replayValue{valid: err == nil}
	}).valid
}
