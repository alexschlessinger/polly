package messages

import "strings"

// AssistantPhase distinguishes interim updates from the answer. Empty means
// the provider did not supply a phase; unknown values remain ordinary text.
type AssistantPhase string

const (
	PhaseCommentary  AssistantPhase = "commentary"
	PhaseFinalAnswer AssistantPhase = "final_answer"
)

// AssistantText is one assistant output message within a model response.
// Streaming chunks carry a text delta; a completed ChatMessage keeps the
// ordered, complete blocks. ID is local to that response.
type AssistantText struct {
	ID    string         `json:"id,omitempty"`
	Phase AssistantPhase `json:"phase,omitempty"`
	Text  string         `json:"text"`
}

// AnswerText selects answer/unspecified blocks, preserving message boundaries.
// Commentary remains in TextBlocks even when there is no final answer.
func AnswerText(blocks []AssistantText) string {
	var texts []string
	for _, block := range blocks {
		if block.Phase != PhaseCommentary && block.Text != "" {
			texts = append(texts, block.Text)
		}
	}
	return strings.Join(texts, "\n\n")
}

// HasTextBlocks reports whether the structured text still describes Content.
// Caller edits and history transformations may replace Content; stale blocks
// must never override that replacement during replay or display.
func (m ChatMessage) HasTextBlocks() bool {
	if m.Role != MessageRoleAssistant || len(m.TextBlocks) == 0 || m.Content != AnswerText(m.TextBlocks) {
		return false
	}
	for _, part := range m.Parts {
		switch part.Type {
		case "text", "image_url", "image_base64", "image_artifact":
			return false
		}
	}
	return true
}

// ModelText includes interim updates when translating history to a provider
// without message phases. GetContent remains the completed answer projection.
func (m ChatMessage) ModelText() string {
	if !m.HasTextBlocks() {
		return m.GetContent()
	}
	var texts []string
	for _, block := range m.TextBlocks {
		if block.Text != "" {
			texts = append(texts, block.Text)
		}
	}
	return strings.Join(texts, "\n\n")
}

// FlattenAssistantText explicitly converts phase-aware history to plain text.
// Use before rewriting its text/parts for a different provider capability.
func (m *ChatMessage) FlattenAssistantText() {
	if m.HasTextBlocks() {
		m.Content = m.ModelText()
	}
	m.TextBlocks = nil
}
