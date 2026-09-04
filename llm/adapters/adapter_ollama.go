package adapters

import (
	"encoding/json"
	"fmt"

	"github.com/alexschlessinger/pollytool/llm/ollama"
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
)

// OllamaAdapter handles Ollama-specific streaming patterns. Ollama streams
// each parsed tool call in its own chunk, so calls accumulate across chunks.
type OllamaAdapter struct {
	isDone   bool   // Track if we've received the final chunk
	idPrefix string // random per-stream namespace for synthetic tool call IDs
}

// NewOllamaAdapter creates a new Ollama streaming adapter
func NewOllamaAdapter() *OllamaAdapter {
	return &OllamaAdapter{idPrefix: randomIDPrefix()}
}

// ProcessChunk handles Ollama streaming chunks
func (a *OllamaAdapter) ProcessChunk(chunk any, state streaming.StreamStateInterface) error {
	resp, ok := chunk.(*ollama.ChatResponse)
	if !ok {
		return nil
	}

	// Capture token counts from final response
	if resp.Done {
		a.isDone = true
		state.SetTokenUsage(resp.PromptEvalCount, resp.EvalCount)
	}

	// Handle thinking content (skip final chunk which contains full content)
	if resp.Message.Thinking != "" && !resp.Done {
		// Thinking will be emitted by the main streaming loop
	}

	// Handle regular content (skip final chunk which contains full content)
	if resp.Message.Content != "" && !resp.Done {
		// Content will be emitted by the main streaming loop
	}

	// Handle tool calls - each chunk carries only the calls parsed since the last
	if len(resp.Message.ToolCalls) > 0 {
		a.handleToolCalls(resp.Message.ToolCalls, state)
	}

	// Infer stop reason (Ollama doesn't expose detailed stop reasons)
	if resp.Done {
		toolCalls := state.GetToolCalls()
		if len(toolCalls) > 0 {
			state.SetStopReason(messages.StopReasonToolUse)
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
func (a *OllamaAdapter) handleToolCalls(toolCalls []ollama.ToolCall, state streaming.StreamStateInterface) {
	base := len(state.GetToolCalls())
	for i, tc := range toolCalls {
		// Marshal arguments to JSON
		tcArgStr, err := json.Marshal(tc.Function.Arguments)
		if err != nil {
			tcArgStr = []byte("{}")
		}

		// Prefer the native call ID when provided; synthesize one otherwise
		id := tc.ID
		if id == "" {
			id = fmt.Sprintf("call_%s_%d", a.idPrefix, base+i)
		}
		state.AddToolCall(messages.ChatMessageToolCall{
			ID:        id,
			Name:      tc.Function.Name,
			Arguments: string(tcArgStr),
		})
	}
}

// EnrichFinalMessage adds Ollama-specific metadata to the final message
func (a *OllamaAdapter) EnrichFinalMessage(msg *messages.ChatMessage, state streaming.StreamStateInterface) {
	// Ollama doesn't require special metadata enrichment
	// Token usage is already set by StreamingCore
}

// HandleToolCall provides Ollama-specific tool call handling
func (a *OllamaAdapter) HandleToolCall(toolData any, state streaming.StreamStateInterface) error {
	// Tool calls are handled in ProcessChunk for Ollama
	return nil
}
