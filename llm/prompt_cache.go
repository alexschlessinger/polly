package llm

import (
	"encoding/json"

	"github.com/alexschlessinger/pollytool/messages"
)

const promptCacheKeyVersion = "polly-prompt-cache-v1"

type promptCacheShape struct {
	Version        string             `json:"version"`
	Model          string             `json:"model"`
	System         []string           `json:"system"`
	Tools          []promptCacheTool  `json:"tools"`
	ResponseSchema *promptCacheSchema `json:"response_schema,omitempty"`
	Temperature    *float32           `json:"temperature,omitempty"`
	MaxTokens      int                `json:"max_tokens,omitempty"`
	ThinkingEffort string             `json:"thinking_effort"`
}

type promptCacheTool struct {
	Name   string          `json:"name"`
	Strict bool            `json:"strict"`
	Raw    json.RawMessage `json:"schema"`
}

type promptCacheSchema struct {
	Strict bool            `json:"strict"`
	Raw    json.RawMessage `json:"schema"`
}

// derivePromptCacheKey hashes only the resolved, stable agent shape. Dynamic
// transcript content is deliberately excluded so equivalent agents can share
// provider prefix work across sessions.
func derivePromptCacheKey(req *CompletionRequest, resolvedMessages []messages.ChatMessage) (string, error) {
	cache := req.shapeCache
	if cache == nil {
		cache = newRequestShapeCache(resolvedMessages)
		cache.prepareTools(req.Tools)
	}
	return cache.promptCacheKey(req)
}
