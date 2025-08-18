package sessions

import (
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
		session.mu.RLock()
		lastAccess := session.last
		session.mu.RUnlock()
		
		if time.Since(lastAccess) > s.config.TTL {
			s.Delete(key.(string))
		}
		return true
	})
}

// GetHistory returns a copy of the session history
func (s *LocalSession) GetHistory() []messages.ChatMessage {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return CopyHistory(s.history)
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
	if s.config == nil {
		return
	}
	s.history = TrimHistory(s.history, s.config.MaxHistory)
}

// Clear clears the session history
func (s *LocalSession) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	
	s.history = InitializeWithSystemPrompt(s.history, s.config)
	s.last = time.Now()
}
