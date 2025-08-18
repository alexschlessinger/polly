package sessions

import (
	"slices"
	"sync"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
)

// LocalSession implements an in-memory session
type LocalSession struct {
	history []messages.ChatMessage
	last    time.Time
	name    string
	mu      sync.RWMutex
	config  *SessionConfig
}

// SyncMapSessionStore implements a thread-safe in-memory session store
type SyncMapSessionStore struct {
	sync.Map
	config *SessionConfig
}

// NewSyncMapSessionStore creates a new thread-safe in-memory session store
func NewSyncMapSessionStore(config *SessionConfig) SessionStore {
	if config == nil {
		config = DefaultConfig()
	}

	store := &SyncMapSessionStore{
		config: config,
	}

	// Start expiry goroutine if TTL is set
	if config.TTL > 0 {
		go func() {
			ticker := time.NewTicker(config.TTL)
			defer ticker.Stop()
			for range ticker.C {
				store.Expire()
			}
		}()
	}

	return store
}

// Get retrieves or creates a session
func (s *SyncMapSessionStore) Get(id string) Session {
	if value, ok := s.Load(id); ok {
		session := value.(*LocalSession)
		session.mu.Lock()
		session.last = time.Now()
		session.mu.Unlock()
		return session
	}

	session := &LocalSession{
		name:   id,
		last:   time.Now(),
		config: s.config,
	}
	session.Clear()
	s.Store(id, session)
	return session
}

// Delete removes a session
func (s *SyncMapSessionStore) Delete(id string) {
	s.Map.Delete(id)
}

// Range iterates over all sessions
func (s *SyncMapSessionStore) Range(f func(key, value any) bool) {
	s.Map.Range(f)
}

// Expire removes old sessions
func (s *SyncMapSessionStore) Expire() {
	s.Range(func(key, value any) bool {
		session := value.(*LocalSession)
		if time.Since(session.last) > s.config.TTL {
			s.Delete(key.(string))
		}
		return true
	})
}

// GetHistory returns a copy of the session history
func (s *LocalSession) GetHistory() []messages.ChatMessage {
	s.mu.RLock()
	defer s.mu.RUnlock()

	history := make([]messages.ChatMessage, len(s.history))
	copy(history, s.history)
	return history
}

// AddMessage adds a message to the session history
func (s *LocalSession) AddMessage(msg messages.ChatMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.history = append(s.history, msg)
	s.last = time.Now()
	s.trimHistory()
}

// trimHistory limits the session history to MaxHistory messages
func (s *LocalSession) trimHistory() {
	if s.config == nil || s.config.MaxHistory == 0 || len(s.history) <= s.config.MaxHistory {
		return
	}
	
	// Keep the first message (system prompt) and the most recent MaxHistory messages
	s.history = append(s.history[:1], s.history[len(s.history)-s.config.MaxHistory+1:]...)
	
	// Handle the API constraint: tool responses must follow tool_calls
	// If the second message is a tool response, remove it
	if len(s.history) > 1 && s.history[1].Role == messages.MessageRoleTool {
		s.history = slices.Delete(s.history, 1, 2)
	}
}

// Clear clears the session history
func (s *LocalSession) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	
	s.history = s.history[:0]
	if s.config != nil && s.config.SystemPrompt != "" {
		s.history = append(s.history, messages.ChatMessage{
			Role:    messages.MessageRoleSystem,
			Content: s.config.SystemPrompt,
		})
	}
	s.last = time.Now()
}
