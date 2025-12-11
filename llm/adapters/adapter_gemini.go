package adapters

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
	"google.golang.org/genai"
)

// GeminiAdapter handles Gemini-specific streaming patterns.
// Gemini receives complete tool calls per chunk and manages thought signatures.
type GeminiAdapter struct {
	signatures map[string]string // Tool call ID -> base64 encoded signature
}

// NewGeminiAdapter creates a new Gemini streaming adapter
func NewGeminiAdapter() *GeminiAdapter {
	return &GeminiAdapter{
		signatures: make(map[string]string),
	}
}

// ProcessChunk handles Gemini streaming chunks
func (a *GeminiAdapter) ProcessChunk(chunk any, state streaming.StreamStateInterface) error {
	resp, ok := chunk.(*genai.GenerateContentResponse)
	if !ok {
		return nil
	}

	// Capture token usage (available on each chunk, use latest values)
	if resp.UsageMetadata != nil {
		state.SetTokenUsage(
			int(resp.UsageMetadata.PromptTokenCount),
			int(resp.UsageMetadata.CandidatesTokenCount),
		)
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

	// If there are tool calls, override stop reason to ToolUse
	// (Gemini doesn't have a specific finish reason for tool calls - it uses "STOP")
	toolCalls := state.GetToolCalls()
	if len(toolCalls) > 0 {
		state.SetStopReason(messages.StopReasonToolUse)
	}

	return nil
}

// handleFunctionCall processes Gemini function calls
func (a *GeminiAdapter) handleFunctionCall(part *genai.Part, state streaming.StreamStateInterface) {
	if part.FunctionCall == nil {
		return
	}

	// Marshal arguments to JSON
	argsJSON, err := json.Marshal(part.FunctionCall.Args)
	if err != nil {
		argsJSON = []byte("{}")
	}

	// Generate a synthetic tool call ID (Gemini doesn't provide one)
	toolCalls := state.GetToolCalls()
	toolCallID := fmt.Sprintf("gemini-%d", len(toolCalls))

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
func mapGeminiFinishReason(fr genai.FinishReason) messages.StopReason {
	switch fr {
	case genai.FinishReasonStop:
		return messages.StopReasonEndTurn
	case genai.FinishReasonMaxTokens:
		return messages.StopReasonMaxTokens
	case genai.FinishReasonSafety, genai.FinishReasonRecitation,
		genai.FinishReasonBlocklist, genai.FinishReasonProhibitedContent,
		genai.FinishReasonSPII, genai.FinishReasonImageSafety,
		genai.FinishReasonImageProhibitedContent:
		return messages.StopReasonContentFilter
	case genai.FinishReasonMalformedFunctionCall:
		return messages.StopReasonError
	default:
		return messages.StopReasonEndTurn
	}
}
