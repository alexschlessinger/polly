package sessions

import (
	"github.com/alexschlessinger/pollytool/messages"
)

// Session interface defines the contract for session implementations
type Session interface {
	GetHistory() []messages.ChatMessage
	AddMessage(messages.ChatMessage)
	Clear()
}

// SessionStore manages multiple sessions
type SessionStore interface {
	Get(string) Session
	Delete(string)
	Expire()
}