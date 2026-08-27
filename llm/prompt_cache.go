package llm

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

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
	Name   string         `json:"name"`
	Strict bool           `json:"strict"`
	Raw    map[string]any `json:"schema"`
}

type promptCacheSchema struct {
	Strict bool           `json:"strict"`
	Raw    map[string]any `json:"schema"`
}

// derivePromptCacheKey hashes only the resolved, stable agent shape. Dynamic
// transcript content is deliberately excluded so equivalent agents can share
// provider prefix work across sessions.
func derivePromptCacheKey(req *CompletionRequest, resolvedMessages []messages.ChatMessage) (string, error) {
	shape := promptCacheShape{
		Version:        promptCacheKeyVersion,
		Model:          req.Model,
		Temperature:    req.Temperature,
		MaxTokens:      req.MaxTokens,
		ThinkingEffort: req.ThinkingEffort.String(),
	}
	for _, msg := range resolvedMessages {
		if msg.Role == messages.MessageRoleSystem {
			shape.System = append(shape.System, msg.GetContent())
		}
	}
	for _, tool := range req.Tools {
		schema := tool.GetSchema()
		if schema == nil {
			shape.Tools = append(shape.Tools, promptCacheTool{})
			continue
		}
		shape.Tools = append(shape.Tools, promptCacheTool{
			Name: schema.Title(), Strict: schema.Strict, Raw: schema.Raw,
		})
	}
	sort.SliceStable(shape.Tools, func(i, j int) bool {
		if shape.Tools[i].Name != shape.Tools[j].Name {
			return shape.Tools[i].Name < shape.Tools[j].Name
		}
		left, _ := json.Marshal(shape.Tools[i])
		right, _ := json.Marshal(shape.Tools[j])
		return string(left) < string(right)
	})
	if req.ResponseSchema != nil {
		shape.ResponseSchema = &promptCacheSchema{
			Strict: req.ResponseSchema.Strict,
			Raw:    req.ResponseSchema.Raw,
		}
	}

	encoded, err := json.Marshal(shape)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}
