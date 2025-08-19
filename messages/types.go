package messages

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
