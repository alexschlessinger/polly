package sessions

import (
	"sync"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
)

// LocalSession implements an in-memory session
type LocalSession struct {
	history  []messages.ChatMessage
	last     time.Time
	name     string
	mu       sync.RWMutex
	metadata *Metadata
}

// SyncMapSessionStore implements a thread-safe in-memory session store
type SyncMapSessionStore struct {
	sync.Map
	defaults *Metadata // Default values for new contexts
}

// NewSyncMapSessionStore creates a new thread-safe in-memory session store
func NewSyncMapSessionStore(metadata *Metadata) SessionStore {
	// Use empty defaults if none provided
	if metadata == nil {
		metadata = &Metadata{}
	}

	store := &SyncMapSessionStore{
		defaults: metadata,
	}

	// Start expiry goroutine if TTL is set
	if metadata.TTL > 0 {
		go func() {
			ticker := time.NewTicker(metadata.TTL)
			defer ticker.Stop()
			for range ticker.C {
				store.Expire()
			}
		}()
	}

	return store
}

// Get retrieves or creates a session
func (s *SyncMapSessionStore) Get(id string) (Session, error) {
	if value, ok := s.Load(id); ok {
		session := value.(*LocalSession)
		session.mu.Lock()
		session.last = time.Now()
		session.mu.Unlock()
		return session, nil
	}

	// Initialize context info from defaults
	contextInfo := &Metadata{
		Name:         id,
		Created:      time.Now(),
		LastUsed:     time.Now(),
		SystemPrompt: s.defaults.SystemPrompt,
		MaxHistory:   s.defaults.MaxHistory,
		TTL:          s.defaults.TTL,
	}

	session := &LocalSession{
		name:     id,
		last:     time.Now(),
		metadata: contextInfo,
	}
	session.Clear()
	s.Store(id, session)
	return session, nil
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
		contextInfo := session.metadata
		session.mu.RUnlock()

		// Use per-context TTL if available, otherwise use default
		ttl := contextInfo.TTL
		if ttl == 0 {
			ttl = s.defaults.TTL
		}

		if ttl > 0 && time.Since(lastAccess) > ttl {
			s.Delete(key.(string))
		}
		return true
	})
}

// List returns all session names
func (s *SyncMapSessionStore) List() ([]string, error) {
	var names []string
	s.Range(func(key, value any) bool {
		names = append(names, key.(string))
		return true
	})
	return names, nil
}

// Exists checks if a session exists without creating it
func (s *SyncMapSessionStore) Exists(id string) bool {
	_, ok := s.Load(id)
	return ok
}

// GetAllMetadata returns metadata for all contexts
func (s *SyncMapSessionStore) GetAllMetadata() map[string]*Metadata {
	result := make(map[string]*Metadata)
	s.Range(func(key, value any) bool {
		session := value.(*LocalSession)
		session.mu.RLock()
		info := session.metadata
		session.mu.RUnlock()
		result[key.(string)] = info
		return true
	})
	return result
}

// GetLast returns the name of the most recently used session
func (s *SyncMapSessionStore) GetLast() string {
	var lastContext string
	var lastTime time.Time

	s.Range(func(key, value any) bool {
		session := value.(*LocalSession)
		session.mu.RLock()
		sessionTime := session.last
		session.mu.RUnlock()

		if sessionTime.After(lastTime) {
			lastTime = sessionTime
			lastContext = key.(string)
		}
		return true
	})

	return lastContext
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
	if s.metadata.MaxHistory == 0 {
		return
	}
	s.history = TrimHistory(s.history, s.metadata.MaxHistory)
}

// Clear clears the session history
func (s *LocalSession) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Clear history and re-initialize with system prompt if configured
	s.history = s.history[:0]
	if s.metadata.SystemPrompt != "" {
		s.history = append(s.history, messages.ChatMessage{
			Role:    messages.MessageRoleSystem,
			Content: s.metadata.SystemPrompt,
		})
	}
	s.last = time.Now()
}

// GetName returns the session name
func (s *LocalSession) GetName() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.name
}

// GetMetadata returns the context metadata
func (s *LocalSession) GetMetadata() *Metadata {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.metadata
}

// SetMetadata updates the context metadata
func (s *LocalSession) SetMetadata(info *Metadata) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.metadata = info
}

// UpdateMetadata applies a partial update to the context metadata
func (s *LocalSession) UpdateMetadata(update *Metadata) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Apply the update to current context info (only non-zero values)
	s.metadata = MergeMetadata(s.metadata, update)
	s.last = time.Now()
	return nil // LocalSession has no persistence errors
}

// GetLastUsed returns when the session was last accessed
func (s *LocalSession) GetLastUsed() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.last
}

// Close is a no-op for LocalSession (no resources to clean up)
func (s *LocalSession) Close() {
	// No-op: LocalSession doesn't hold any resources that need cleanup
}
