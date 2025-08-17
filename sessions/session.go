package sessions

import (
	"time"

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

// LocalSession implements an in-memory session
type LocalSession struct {
	history []messages.ChatMessage
	last    time.Time
	name    string
}

// MemorySessionStore implements an in-memory session store
type MemorySessionStore struct {
	sessions map[string]*LocalSession
	ttl      time.Duration
}

// NewSessionStore creates a new in-memory session store
func NewSessionStore(ttl time.Duration) SessionStore {
	store := &MemorySessionStore{
		sessions: make(map[string]*LocalSession),
		ttl:      ttl,
	}

	// Start expiry goroutine if TTL is set
	if ttl > 0 {
		go func() {
			ticker := time.NewTicker(ttl)
			defer ticker.Stop()
			for range ticker.C {
				store.Expire()
			}
		}()
	}

	return store
}

// Get retrieves or creates a session
func (s *MemorySessionStore) Get(id string) Session {
	if session, exists := s.sessions[id]; exists {
		session.last = time.Now()
		return session
	}

	session := &LocalSession{
		name:    id,
		last:    time.Now(),
		history: []messages.ChatMessage{},
	}
	s.sessions[id] = session
	return session
}

// Delete removes a session
func (s *MemorySessionStore) Delete(id string) {
	delete(s.sessions, id)
}

// Expire removes old sessions
func (s *MemorySessionStore) Expire() {
	now := time.Now()
	for id, session := range s.sessions {
		if now.Sub(session.last) > s.ttl {
			delete(s.sessions, id)
		}
	}
}

// GetHistory returns a copy of the session history
func (s *LocalSession) GetHistory() []messages.ChatMessage {
	history := make([]messages.ChatMessage, len(s.history))
	copy(history, s.history)
	return history
}

// AddMessage adds a message to the session history
func (s *LocalSession) AddMessage(msg messages.ChatMessage) {
	s.history = append(s.history, msg)
	s.last = time.Now()
}

// Clear clears the session history
func (s *LocalSession) Clear() {
	s.history = []messages.ChatMessage{}
	s.last = time.Now()
}
