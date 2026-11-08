package main

import (
	"crypto/sha256"
	"fmt"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

// Sparse prefix facts retain the identities and counts the whole-conversation
// catalog needs. Turn and prose barriers reconstruct ordering without fetching
// discarded text. These placeholders are never sent to the visible hydrator.
func (m *replModel) hydrateWindowDisplayCatalog(history []messages.ChatMessage, prefix *sessions.TranscriptDisplay) {
	if prefix == nil {
		m.hydrateDisplayCatalog(history)
		return
	}
	var projected []messages.ChatMessage
	turn, previous := int64(-1), int64(-1)
	deniedHash := fmt.Sprintf("%x", sha256.Sum256([]byte(llm.ToolDeniedContent)))
	for _, fact := range prefix.Prefix {
		barrier := fact.Barrier
		if !m.showCommentary {
			barrier = fact.AnswerBarrier
		}
		if fact.Turn != turn {
			projected = append(projected, messages.ChatMessage{Role: messages.MessageRoleUser})
			turn = fact.Turn
		} else if barrier > previous {
			projected = append(projected, messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "x"})
		}
		msg := fact.Message
		if msg.Role == messages.MessageRoleTool && fact.ContentHash == deniedHash {
			msg.Content = llm.ToolDeniedContent
		}
		projected = append(projected, msg)
		previous = fact.Sequence
	}
	projected = append(projected, history...)
	m.hydrateDisplayCatalog(projected)
}
