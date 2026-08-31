package adapters

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/alexschlessinger/pollytool/llm/gemini"
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
)

// GeminiAdapter handles Gemini-specific streaming patterns.
// Gemini receives complete tool calls per chunk and manages thought signatures.
type GeminiAdapter struct {
	signatures map[string]string // Tool call ID -> base64 encoded signature
	idPrefix   string            // random per-stream namespace for synthetic tool call IDs
}

// NewGeminiAdapter creates a new Gemini streaming adapter
func NewGeminiAdapter() *GeminiAdapter {
	return &GeminiAdapter{
		signatures: make(map[string]string),
		idPrefix:   randomIDPrefix(),
	}
}

// ProcessChunk handles Gemini streaming chunks
func (a *GeminiAdapter) ProcessChunk(chunk any, state streaming.StreamStateInterface) error {
	resp, ok := chunk.(*gemini.GenerateContentResponse)
	if !ok {
		return nil
	}

	// Capture token usage (available on each chunk, use latest values)
	if resp.UsageMetadata != nil {
		state.SetTokenUsage(
			int(resp.UsageMetadata.PromptTokenCount),
			int(resp.UsageMetadata.CandidatesTokenCount),
		)
		if resp.UsageMetadata.CachedContentTokenCount != nil {
			state.SetPromptCacheUsage(int(*resp.UsageMetadata.CachedContentTokenCount), 0)
		}
	}

	// Process each candidate's parts
	if len(resp.Candidates) > 0 {
		candidate := resp.Candidates[0]

		// Capture finish reason when set
		if candidate.FinishReason != "" {
			state.SetStopReason(mapGeminiFinishReason(candidate.FinishReason))
		}

		if candidate.Content != nil {
			for _, part := range candidate.Content.Parts {
				// Handle text content (emission handled by main loop)
				if part.Text != "" {
					// Content will be emitted by the main streaming loop
				}

				// Handle function calls
				if part.FunctionCall != nil {
					a.handleFunctionCall(part, state)
				}
			}
		}
	}

	// If there are tool calls, override stop reason to ToolUse (Gemini has no
	// tool-call finish reason - it uses "STOP"). Only a healthy finish is
	// overridden: a terminal reason such as SAFETY, MAX_TOKENS, or
	// MALFORMED_FUNCTION_CALL must survive, or calls accumulated before it
	// would present as an ordinary tool turn.
	if toolCalls := state.GetToolCalls(); len(toolCalls) > 0 {
		switch state.GetStopReason() {
		case "", messages.StopReasonEndTurn:
			state.SetStopReason(messages.StopReasonToolUse)
		}
	}

	return nil
}

// handleFunctionCall processes Gemini function calls
func (a *GeminiAdapter) handleFunctionCall(part *gemini.Part, state streaming.StreamStateInterface) {
	if part.FunctionCall == nil {
		return
	}

	// Marshal arguments to JSON
	argsJSON, err := json.Marshal(part.FunctionCall.Args)
	if err != nil {
		argsJSON = []byte("{}")
	}

	// Prefer the native call ID when the API provides one (it must be echoed
	// back on the matching FunctionResponse); synthesize one otherwise.
	toolCalls := state.GetToolCalls()
	toolCallID := part.FunctionCall.ID
	if toolCallID == "" {
		toolCallID = fmt.Sprintf("gemini-%s-%d", a.idPrefix, len(toolCalls))
	}

	// Add the tool call
	state.AddToolCall(messages.ChatMessageToolCall{
		ID:        toolCallID,
		Name:      part.FunctionCall.Name,
		Arguments: string(argsJSON),
	})

	// Store thought signature if present
	if len(part.ThoughtSignature) > 0 {
		a.signatures[toolCallID] = base64.StdEncoding.EncodeToString(part.ThoughtSignature)
	}
}

// EnrichFinalMessage adds Gemini-specific metadata to the final message
func (a *GeminiAdapter) EnrichFinalMessage(msg *messages.ChatMessage, state streaming.StreamStateInterface) {
	// Add thought signatures to metadata
	if len(a.signatures) > 0 {
		if msg.Metadata == nil {
			msg.Metadata = make(map[string]any)
		}
		msg.Metadata["gemini_thought_signatures"] = a.signatures
	}
}

// HandleToolCall provides Gemini-specific tool call handling
func (a *GeminiAdapter) HandleToolCall(toolData any, state streaming.StreamStateInterface) error {
	// Tool calls are handled in ProcessChunk for Gemini
	return nil
}

// mapGeminiFinishReason converts Gemini's finish reason to our normalized type
func mapGeminiFinishReason(fr gemini.FinishReason) messages.StopReason {
	switch fr {
	case gemini.FinishReasonStop:
		return messages.StopReasonEndTurn
	case gemini.FinishReasonMaxTokens:
		return messages.StopReasonMaxTokens
	case gemini.FinishReasonSafety, gemini.FinishReasonRecitation,
		gemini.FinishReasonBlocklist, gemini.FinishReasonProhibitedContent,
		gemini.FinishReasonSPII, gemini.FinishReasonImageSafety,
		gemini.FinishReasonImageProhibitedContent:
		return messages.StopReasonContentFilter
	case gemini.FinishReasonMalformedFunctionCall:
		return messages.StopReasonError
	default:
		return messages.StopReasonEndTurn
	}
}
