package llm

import (
	"encoding/json"

	"github.com/alexschlessinger/pollytool/llm/openai"

	"github.com/alexschlessinger/pollytool/messages"
)

const promptCacheKeyVersion = "polly-prompt-cache-v1"

type promptCacheShape struct {
	Version            string                `json:"version"`
	Model              string                `json:"model"`
	System             []string              `json:"system"`
	Tools              []promptCacheTool     `json:"tools"`
	ResponseSchema     *promptCacheSchema    `json:"response_schema,omitempty"`
	Temperature        *float32              `json:"temperature,omitempty"`
	MaxTokens          int                   `json:"max_tokens,omitempty"`
	ThinkingEffort     string                `json:"thinking_effort"`
	OpenRouterThinking *openai.ChatReasoning `json:"openrouter_reasoning,omitempty"`
	OpenRouterReplay   string                `json:"openrouter_replay,omitempty"`
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
// transcript content is excluded for prefix caching. OpenRouter also binds its
// request identity to the selected reasoning replay; duplicate display text and
// response attribution do not contribute.
func derivePromptCacheKey(req *CompletionRequest, resolvedMessages []messages.ChatMessage) (string, error) {
	cache := req.shapeCache
	if cache == nil {
		cache = newRequestShapeCache(resolvedMessages)
		cache.prepareTools(req.Tools)
	}
	return cache.promptCacheKey(req)
}
