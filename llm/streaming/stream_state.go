package streaming

import (
	"maps"
	"sync"

	"github.com/alexschlessinger/pollytool/messages"
)

// StreamStateInterface defines the methods for interacting with streaming state.
// This interface is implemented by StreamState and allows adapters to work without circular imports.
type StreamStateInterface interface {
	// Setters
	AppendContent(content string)
	AppendReasoning(reasoning string)
	AddToolCall(toolCall messages.ChatMessageToolCall)
	SetTokenUsage(input, output int)
	SetPromptCacheUsage(read, write int)
	SetStopReason(reason messages.StopReason)
	SetMetadata(key string, value any)
	UpdateToolCallAtIndex(index int, updater func(*messages.ChatMessageToolCall))

	// Getters
	GetMetadata(key string) (any, bool)
	GetStopReason() messages.StopReason
	GetToolCalls() []messages.ChatMessageToolCall
	GetInputTokens() int
	GetOutputTokens() int
	GetCacheReadInputTokens() int
	GetCacheWriteInputTokens() int
	HasPromptCacheUsage() bool
}

// StreamState holds the common state during streaming for all providers.
// It provides thread-safe access to streaming state that accumulates across chunks.
type StreamState struct {
	// Common fields used by all providers
	ResponseContent       string                         // Accumulated text content
	ReasoningContent      string                         // Accumulated thinking/reasoning content
	ToolCalls             []messages.ChatMessageToolCall // Accumulated tool calls
	StopReason            messages.StopReason            // Reason for completion
	InputTokens           int                            // Token count for prompt
	OutputTokens          int                            // Token count for completion
	CacheReadInputTokens  int                            // Provider-reported cache hits
	CacheWriteInputTokens int                            // Provider-reported cache writes
	PromptCacheUsageSet   bool                           // Whether the provider reported cache details

	// Provider-specific metadata storage
	// Used for things like Anthropic thinking blocks, Gemini signatures, etc.
	Metadata map[string]any

	// Internal state management
	mu sync.Mutex // Protects concurrent access to fields
}

// NewStreamState creates a new StreamState with initialized fields
func NewStreamState() *StreamState {
	return &StreamState{
		ToolCalls: make([]messages.ChatMessageToolCall, 0),
		Metadata:  make(map[string]any),
	}
}

// AppendContent safely appends content to the response
func (s *StreamState) AppendContent(content string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ResponseContent += content
}

// AppendReasoning safely appends reasoning/thinking content
func (s *StreamState) AppendReasoning(reasoning string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ReasoningContent += reasoning
}

// AddToolCall safely adds a tool call to the state
func (s *StreamState) AddToolCall(toolCall messages.ChatMessageToolCall) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ToolCalls = append(s.ToolCalls, toolCall)
}

// SetTokenUsage safely sets token counts
func (s *StreamState) SetTokenUsage(input, output int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.InputTokens = input
	s.OutputTokens = output
}

// SetPromptCacheUsage safely records provider-reported prompt cache usage.
func (s *StreamState) SetPromptCacheUsage(read, write int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.CacheReadInputTokens = read
	s.CacheWriteInputTokens = write
	s.PromptCacheUsageSet = true
}

// SetStopReason safely sets the stop reason
func (s *StreamState) SetStopReason(reason messages.StopReason) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.StopReason = reason
}

// GetStopReason returns the stop reason recorded so far
func (s *StreamState) GetStopReason() messages.StopReason {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.StopReason
}

// SetMetadata safely sets a metadata value
func (s *StreamState) SetMetadata(key string, value any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Metadata == nil {
		s.Metadata = make(map[string]any)
	}
	s.Metadata[key] = value
}

// GetMetadata safely gets a metadata value
func (s *StreamState) GetMetadata(key string) (any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	val, ok := s.Metadata[key]
	return val, ok
}

// UpdateToolCallAtIndex safely updates a tool call at a specific index
// Used by OpenAI for index-based tool call accumulation
func (s *StreamState) UpdateToolCallAtIndex(index int, updater func(*messages.ChatMessageToolCall)) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Ensure the slice has enough capacity
	for len(s.ToolCalls) <= index {
		s.ToolCalls = append(s.ToolCalls, messages.ChatMessageToolCall{
			Arguments: "{}", // Initialize with empty JSON object
		})
	}

	// Apply the updater function
	updater(&s.ToolCalls[index])
}

// GetToolCalls safely returns a copy of the tool calls
func (s *StreamState) GetToolCalls() []messages.ChatMessageToolCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Return a copy to prevent external modification
	result := make([]messages.ChatMessageToolCall, len(s.ToolCalls))
	copy(result, s.ToolCalls)
	return result
}

// GetInputTokens safely returns the input token count
func (s *StreamState) GetInputTokens() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.InputTokens
}

// GetOutputTokens safely returns the output token count
func (s *StreamState) GetOutputTokens() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.OutputTokens
}

func (s *StreamState) GetCacheReadInputTokens() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.CacheReadInputTokens
}

func (s *StreamState) GetCacheWriteInputTokens() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.CacheWriteInputTokens
}

func (s *StreamState) HasPromptCacheUsage() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.PromptCacheUsageSet
}

// Clone creates a copy of the current state (for debugging/logging)
func (s *StreamState) Clone() *StreamState {
	s.mu.Lock()
	defer s.mu.Unlock()

	clone := StreamState{
		ResponseContent:       s.ResponseContent,
		ReasoningContent:      s.ReasoningContent,
		StopReason:            s.StopReason,
		InputTokens:           s.InputTokens,
		OutputTokens:          s.OutputTokens,
		CacheReadInputTokens:  s.CacheReadInputTokens,
		CacheWriteInputTokens: s.CacheWriteInputTokens,
		PromptCacheUsageSet:   s.PromptCacheUsageSet,
		ToolCalls:             make([]messages.ChatMessageToolCall, len(s.ToolCalls)),
		Metadata:              make(map[string]any),
	}

	// Deep copy tool calls
	copy(clone.ToolCalls, s.ToolCalls)

	// Copy metadata
	maps.Copy(clone.Metadata, s.Metadata)

	return &clone
}
