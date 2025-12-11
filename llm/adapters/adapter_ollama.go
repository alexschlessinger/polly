package adapters

import (
	"encoding/json"
	"fmt"

	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
	ollamaapi "github.com/ollama/ollama/api"
)

// OllamaAdapter handles Ollama-specific streaming patterns.
// Ollama sends complete tool calls on each update (reset pattern).
type OllamaAdapter struct {
	isDone bool // Track if we've received the final chunk
}

// NewOllamaAdapter creates a new Ollama streaming adapter
func NewOllamaAdapter() *OllamaAdapter {
	return &OllamaAdapter{}
}

// ProcessChunk handles Ollama streaming chunks
func (a *OllamaAdapter) ProcessChunk(chunk any, state streaming.StreamStateInterface) error {
	resp, ok := chunk.(*ollamaapi.ChatResponse)
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

	// Handle tool calls - Ollama sends complete state on each chunk
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

// handleToolCalls processes Ollama's complete tool call updates
func (a *OllamaAdapter) handleToolCalls(toolCalls []ollamaapi.ToolCall, state streaming.StreamStateInterface) {
	// Ollama sends the complete set of tool calls on each update
	// Reset and replace with the new set
	state.ResetToolCalls()

	for i, tc := range toolCalls {
		// Marshal arguments to JSON
		tcArgStr, err := json.Marshal(tc.Function.Arguments)
		if err != nil {
			tcArgStr = []byte("{}")
		}

		// Add the tool call with a generated ID
		state.AddToolCall(messages.ChatMessageToolCall{
			ID:        fmt.Sprintf("call_%d", i),
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
