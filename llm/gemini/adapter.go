package gemini

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
)

// ThoughtSignaturesKey is the message metadata key under which the
// adapter stores each tool call's thought signature (base64), keyed by call
// ID, so the client can replay them on later requests.
const ThoughtSignaturesKey = "gemini_thought_signatures"

// Adapter handles Gemini-specific streaming patterns.
// Gemini receives complete tool calls per chunk and manages thought signatures.
type Adapter struct {
	signatures map[string]string // Tool call ID -> base64 encoded signature
	idPrefix   string            // random per-stream namespace for synthetic tool call IDs
}

// NewAdapter creates a new Gemini streaming adapter
func NewAdapter() *Adapter {
	return &Adapter{
		signatures: make(map[string]string),
		idPrefix:   streaming.RandomIDPrefix(),
	}
}

// ProcessChunk handles Gemini streaming chunks
func (a *Adapter) ProcessChunk(chunk any, state streaming.StreamStateInterface) error {
	resp, ok := chunk.(*GenerateContentResponse)
	if !ok {
		return nil
	}

	// Capture token usage (available on each chunk, use latest values).
	// Thinking tokens are billed output that the API reports beside the
	// candidate tokens, so both count.
	if resp.UsageMetadata != nil {
		state.SetTokenUsage(
			int(resp.UsageMetadata.PromptTokenCount),
			int(resp.UsageMetadata.CandidatesTokenCount+resp.UsageMetadata.ThoughtsTokenCount),
		)
		if resp.UsageMetadata.CachedContentTokenCount != nil {
			state.SetPromptCacheUsage(int(*resp.UsageMetadata.CachedContentTokenCount), 0)
		}
	}

	// A blocked prompt yields no candidates at all; the block reason is the
	// whole story, and a blank success would hide it.
	if resp.PromptFeedback != nil && resp.PromptFeedback.BlockReason != "" && resp.PromptFeedback.BlockReason != BlockReasonUnspecified {
		state.SetStopReason(messages.StopReasonContentFilter)
		return fmt.Errorf("gemini blocked the prompt: %s", resp.PromptFeedback.BlockReason)
	}

	// Process each candidate's parts
	if len(resp.Candidates) > 0 {
		candidate := resp.Candidates[0]

		// Capture finish reason when set
		if candidate.FinishReason != "" {
			state.SetStopReason(mapFinishReason(candidate.FinishReason))
		}

		if candidate.Content != nil {
			for _, part := range candidate.Content.Parts {
				// Text is emitted by the main streaming loop; only
				// function calls need adapter handling.
				if part.FunctionCall != nil {
					a.handleFunctionCall(part, state)
				}
			}
		}
	}

	// Gemini has no tool-call finish reason (it reports STOP); the streaming
	// core promotes a reply with calls to a tool turn at completion.
	return nil
}

// handleFunctionCall processes Gemini function calls
func (a *Adapter) handleFunctionCall(part *Part, state streaming.StreamStateInterface) {
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
		toolCallID = streaming.SyntheticCallID("gemini", a.idPrefix, len(toolCalls))
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
func (a *Adapter) EnrichFinalMessage(msg *messages.ChatMessage, state streaming.StreamStateInterface) {
	// Add thought signatures to metadata
	if len(a.signatures) > 0 {
		if msg.Metadata == nil {
			msg.Metadata = make(map[string]any)
		}
		msg.Metadata[ThoughtSignaturesKey] = a.signatures
	}
}

// mapFinishReason converts Gemini's finish reason to our normalized
// type. Only STOP is a healthy end of turn: every other reason, including
// one this build does not know, marks a reply that was cut short or refused,
// and must not be persisted as a normal completion.
func mapFinishReason(fr FinishReason) messages.StopReason {
	switch fr {
	case FinishReasonStop:
		return messages.StopReasonEndTurn
	case FinishReasonMaxTokens:
		return messages.StopReasonMaxTokens
	case FinishReasonSafety, FinishReasonRecitation,
		FinishReasonBlocklist, FinishReasonProhibitedContent,
		FinishReasonSPII, FinishReasonImageSafety,
		FinishReasonImageProhibitedContent, FinishReasonImageRecitation:
		return messages.StopReasonContentFilter
	case FinishReasonMalformedFunctionCall, FinishReasonUnexpectedToolCall,
		FinishReasonTooManyToolCalls, FinishReasonLanguage,
		FinishReasonOther, FinishReasonNoImage, FinishReasonImageOther,
		FinishReasonUnspecified:
		return messages.StopReasonError
	default:
		return messages.StopReasonError
	}
}
