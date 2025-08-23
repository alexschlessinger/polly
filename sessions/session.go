package sessions

import (
	"sync"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
)

// LocalSession implements an in-memory session
type LocalSession struct {
	history     []messages.ChatMessage
	last        time.Time
	name        string
	mu          sync.RWMutex
	contextInfo *ContextInfo
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
func (s *SyncMapSessionStore) Get(id string) (Session, error) {
	if value, ok := s.Load(id); ok {
		session := value.(*LocalSession)
		session.mu.Lock()
		session.last = time.Now()
		session.mu.Unlock()
		return session, nil
	}

	// Initialize context info from default config
	contextInfo := &ContextInfo{
		Name:         id,
		Created:      time.Now(),
		LastUsed:     time.Now(),
		SystemPrompt: s.config.SystemPrompt,
		MaxHistory:   s.config.MaxHistory,
		TTL:          s.config.TTL,
	}

	session := &LocalSession{
		name:        id,
		last:        time.Now(),
		contextInfo: contextInfo,
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
		contextInfo := session.contextInfo
		session.mu.RUnlock()

		// Use per-context TTL if available, otherwise use default
		ttl := contextInfo.TTL
		if ttl == 0 {
			ttl = s.config.TTL
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

// GetAllContextInfo returns metadata for all contexts
func (s *SyncMapSessionStore) GetAllContextInfo() map[string]*ContextInfo {
	result := make(map[string]*ContextInfo)
	s.Range(func(key, value any) bool {
		session := value.(*LocalSession)
		session.mu.RLock()
		info := session.contextInfo
		session.mu.RUnlock()
		result[key.(string)] = info
		return true
	})
	return result
}

// SaveContextInfo saves context information
func (s *SyncMapSessionStore) SaveContextInfo(info *ContextInfo) error {
    // Get the session and update its context info
    if value, ok := s.Load(info.Name); ok {
        session := value.(*LocalSession)
        session.SetContextInfo(info)
    }
    return nil
}

// SaveContextUpdate applies a partial update to context info
func (s *SyncMapSessionStore) SaveContextUpdate(upd *ContextUpdate) error {
    if upd == nil || upd.Name == "" {
        return nil
    }
    // Ensure the session exists
    value, ok := s.Load(upd.Name)
    if !ok {
        // Create new session with default info
        sess, err := s.Get(upd.Name)
        if err != nil {
            return err
        }
        value = sess
    }
    session := value.(*LocalSession)

    // Merge update into existing context info via helper
    session.mu.Lock()
    session.contextInfo = ApplyContextUpdate(session.contextInfo, upd)
    session.mu.Unlock()
    return nil
}

// GetLastContext returns the name of the most recently used session
func (s *SyncMapSessionStore) GetLastContext() string {
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
	if s.contextInfo.MaxHistory == 0 {
		return
	}
	s.history = TrimHistory(s.history, s.contextInfo.MaxHistory)
}

// Clear clears the session history
func (s *LocalSession) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Create a temporary config from contextInfo for initialization
	config := &SessionConfig{
		SystemPrompt: s.contextInfo.SystemPrompt,
		MaxHistory:   s.contextInfo.MaxHistory,
		TTL:          s.contextInfo.TTL,
	}
	s.history = InitializeWithSystemPrompt(s.history, config)
	s.last = time.Now()
}

// GetName returns the session name
func (s *LocalSession) GetName() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.name
}

// GetContextInfo returns the context metadata
func (s *LocalSession) GetContextInfo() *ContextInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.contextInfo
}

// SetContextInfo updates the context metadata
func (s *LocalSession) SetContextInfo(info *ContextInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.contextInfo = info
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
