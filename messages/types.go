package messages

// StopReason indicates why the model stopped generating
type StopReason string

const (
	// StopReasonEndTurn indicates normal completion
	StopReasonEndTurn StopReason = "end_turn"
	// StopReasonToolUse indicates the model wants to use tools
	StopReasonToolUse StopReason = "tool_use"
	// StopReasonMaxTokens indicates the response was truncated due to token limit
	StopReasonMaxTokens StopReason = "max_tokens"
	// StopReasonContentFilter indicates the response was blocked by safety/policy
	StopReasonContentFilter StopReason = "content_filter"
	// StopReasonError indicates malformed output or other error
	StopReasonError StopReason = "error"
)

// ContentPart represents a part of a message content (text, image, etc.)
type ContentPart struct {
	Type      string // "text", "image_url", "image_base64", "file"
	Text      string // For text content
	ImageURL  string // For image URLs
	ImageData string // For base64 encoded images
	MimeType  string // MIME type for images/files
	FileName  string // Original filename if applicable
}

// ChatMessage represents a provider-agnostic chat message
type ChatMessage struct {
	Role       string
	Content    string        // Simple string content (backward compatible)
	Parts      []ContentPart // Multimodal content parts
	ToolCalls  []ChatMessageToolCall
	ToolCallID string         // For tool response messages
	Reasoning  string         // Reasoning/thinking content from <think> blocks
	Metadata   map[string]any // Additional metadata for the message
	StopReason StopReason     // Why the model stopped generating (only set on final message)
}

// GetContent returns the content as a string, handling both simple and multimodal messages
func (m *ChatMessage) GetContent() string {
	if m.Content != "" {
		return m.Content
	}
	// For multimodal, return text parts concatenated
	var result string
	for _, part := range m.Parts {
		if part.Type == "text" && part.Text != "" {
			result += part.Text
		}
	}
	return result
}

// HasImages returns true if the message contains image content
func (m *ChatMessage) HasImages() bool {
	for _, part := range m.Parts {
		if part.Type == "image_url" || part.Type == "image_base64" {
			return true
		}
	}
	return false
}

// ChatMessageToolCall represents a tool call within a message
type ChatMessageToolCall struct {
	ID        string
	Name      string
	Arguments string // JSON string of arguments
}

// Standard role constants
const (
	MessageRoleSystem    = "system"
	MessageRoleUser      = "user"
	MessageRoleAssistant = "assistant"
	MessageRoleTool      = "tool"
)

// Metadata keys for token usage
const (
	MetadataKeyInputTokens  = "input_tokens"
	MetadataKeyOutputTokens = "output_tokens"
)

// GetInputTokens returns the input token count from metadata, or 0 if not set
func (m *ChatMessage) GetInputTokens() int {
	if m.Metadata == nil {
		return 0
	}
	if v, ok := m.Metadata[MetadataKeyInputTokens].(int); ok {
		return v
	}
	if v, ok := m.Metadata[MetadataKeyInputTokens].(int64); ok {
		return int(v)
	}
	return 0
}

// GetOutputTokens returns the output token count from metadata, or 0 if not set
func (m *ChatMessage) GetOutputTokens() int {
	if m.Metadata == nil {
		return 0
	}
	if v, ok := m.Metadata[MetadataKeyOutputTokens].(int); ok {
		return v
	}
	if v, ok := m.Metadata[MetadataKeyOutputTokens].(int64); ok {
		return int(v)
	}
	return 0
}

// SetTokenUsage sets the input and output token counts in metadata
func (m *ChatMessage) SetTokenUsage(input, output int) {
	if m.Metadata == nil {
		m.Metadata = make(map[string]any)
	}
	m.Metadata[MetadataKeyInputTokens] = input
	m.Metadata[MetadataKeyOutputTokens] = output
}
