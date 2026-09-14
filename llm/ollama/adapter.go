package ollama

import (
	"encoding/json"

	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
)

// Adapter handles Ollama-specific streaming patterns. Ollama streams
// each parsed tool call in its own chunk, so calls accumulate across chunks.
type Adapter struct {
	idPrefix string // random per-stream namespace for synthetic tool call IDs
}

// NewAdapter creates a new Ollama streaming adapter
func NewAdapter() *Adapter {
	return &Adapter{idPrefix: streaming.RandomIDPrefix()}
}

// ProcessChunk handles Ollama streaming chunks
func (a *Adapter) ProcessChunk(chunk any, state streaming.StreamStateInterface) error {
	resp, ok := chunk.(*ChatResponse)
	if !ok {
		return nil
	}

	// Thinking and content are emitted by the main streaming loop.
	// Handle tool calls - each chunk carries only the calls parsed since the last
	if len(resp.Message.ToolCalls) > 0 {
		a.handleToolCalls(resp.Message.ToolCalls, state)
	}

	// The final response carries the token counts and the done reason: a
	// reply num_predict cut off must read as truncated, not as a normal end
	// of turn. The streaming core promotes an ordinary finish with tool
	// calls to a tool turn at completion.
	if resp.Done {
		state.SetTokenUsage(resp.PromptEvalCount, resp.EvalCount)
		if resp.DoneReason == DoneReasonLength {
			state.SetStopReason(messages.StopReasonMaxTokens)
		} else {
			state.SetStopReason(messages.StopReasonEndTurn)
		}
	}

	return nil
}

// handleToolCalls appends the tool calls one chunk carries. Ollama's
// streaming parser emits each call once, in the chunk that completed it, so
// an earlier chunk's calls must survive; synthetic IDs number calls across
// the whole stream, not the chunk.
func (a *Adapter) handleToolCalls(toolCalls []ToolCall, state streaming.StreamStateInterface) {
	base := state.ToolCallCount()
	for i, tc := range toolCalls {
		// Marshal arguments to JSON
		tcArgStr, err := json.Marshal(tc.Function.Arguments)
		if err != nil {
			tcArgStr = []byte("{}")
		}

		// Prefer the native call ID when provided; synthesize one otherwise
		id := tc.ID
		if id == "" {
			id = streaming.SyntheticCallID("ollama", a.idPrefix, base+i)
		}
		state.AddToolCall(messages.ChatMessageToolCall{
			ID:        id,
			Name:      tc.Function.Name,
			Arguments: string(tcArgStr),
		})
	}
}

// EnrichFinalMessage adds Ollama-specific metadata to the final message
func (a *Adapter) EnrichFinalMessage(msg *messages.ChatMessage, state streaming.StreamStateInterface) {
	// Ollama doesn't require special metadata enrichment
	// Token usage is already set by StreamingCore
}
