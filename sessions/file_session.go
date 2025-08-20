package sessions

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/gofrs/flock"
)

// FileSession implements a file-based persistent session
type FileSession struct {
	ID      string                 `json:"id"`
	History []messages.ChatMessage `json:"history"`
	Created time.Time              `json:"created"`
	Updated time.Time              `json:"updated"`
	path    string
	lock    *flock.Flock // File lock using flock
	config  *SessionConfig
	mu      sync.RWMutex
}

// ContextInfo stores metadata about a context
type ContextInfo struct {
	Name         string    `json:"name"` // Name is the primary identifier (e.g., "@stocks" or random ID)
	Created      time.Time `json:"created"`
	LastUsed     time.Time `json:"lastUsed"`
	Model        string    `json:"model,omitempty"`
	Temperature  float64   `json:"temperature,omitempty"`
	SystemPrompt string    `json:"systemPrompt,omitempty"`
	Description  string    `json:"description,omitempty"`
	ToolPaths    []string  `json:"toolPaths,omitempty"`
	MCPServers   []string  `json:"mcpServers,omitempty"`
	MaxTokens    int       `json:"maxTokens,omitempty"`
}

// ContextIndex manages the mapping of names to IDs
type ContextIndex struct {
	Contexts    map[string]*ContextInfo `json:"contexts"`
	LastContext string                  `json:"lastContext,omitempty"`
}

// FileSessionStore implements a file-based session store
type FileSessionStore struct {
	baseDir string
	index   *ContextIndex
	indexMu sync.RWMutex
	config  *SessionConfig
}

// NewFileSessionStore creates a new file-based session store
func NewFileSessionStore(baseDir string, config *SessionConfig) (SessionStore, error) {
	if config == nil {
		config = DefaultConfig()
	}

	// Use default directory if not specified
	if baseDir == "" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("failed to get home directory: %w", err)
		}
		baseDir = filepath.Join(homeDir, ".pollytool", "contexts")
	}

	// Create directory if it doesn't exist
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create context directory: %w", err)
	}

	store := &FileSessionStore{
		baseDir: baseDir,
		config:  config,
	}

	// Load or create index
	if err := store.loadIndex(); err != nil {
		// Create new index if loading fails
		store.index = &ContextIndex{
			Contexts: make(map[string]*ContextInfo),
		}
	}

	return store, nil
}

// Get retrieves or creates a session
func (s *FileSessionStore) Get(name string) Session {
	// The name should already be validated at the application level
	// We just use it directly as the filename
	if name == "" {
		// This shouldn't happen - the app layer should ensure a name
		panic("FileSessionStore.Get called with empty name")
	}

	// Track last context
	s.SetLastContext(name)

	// Use name directly as filename (already validated at app level)
	sessionPath := filepath.Join(s.baseDir, name+".json")
	lockPath := sessionPath + ".lock"

	// Create a new flock instance
	fileLock := flock.New(lockPath)

	// Try to acquire exclusive lock with 10 second timeout, retrying every 100ms
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	locked, err := fileLock.TryLockContext(ctx, 100*time.Millisecond)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to acquire lock: %v\n", err)
		return nil // Abort on failure
	}
	if !locked {
		fmt.Fprintf(os.Stderr, "Error: Could not acquire lock within 10 seconds\n")
		return nil // Abort on failure
	}

	// Try to load existing session
	if data, err := os.ReadFile(sessionPath); err == nil {
		var session FileSession
		if err := json.Unmarshal(data, &session); err == nil {
			session.path = sessionPath
			session.lock = fileLock
			session.config = s.config
			session.Updated = time.Now()
			session.save()
			return &session
		}
	}

	// Create new session
	session := &FileSession{
		ID:      name,
		History: []messages.ChatMessage{},
		Created: time.Now(),
		Updated: time.Now(),
		path:    sessionPath,
		lock:    fileLock,
		config:  s.config,
	}
	// Initialize with system prompt if configured
	session.History = InitializeWithSystemPrompt(session.History, s.config)
	session.save()
	return session
}

// Delete removes a session
func (s *FileSessionStore) Delete(name string) {
	// Use name directly (already validated at app level)
	sessionPath := filepath.Join(s.baseDir, name+".json")
	lockPath := sessionPath + ".lock"
	os.Remove(sessionPath)
	os.Remove(lockPath)

	// Remove from index if present
	s.DeleteContextName(name)
}

// Expire removes old sessions
// Range iterates over all sessions using the index
func (s *FileSessionStore) Range(f func(key, value any) bool) {
	s.indexMu.RLock()
	defer s.indexMu.RUnlock()

	// Iterate over contexts in the index
	for name := range s.index.Contexts {
		// Load the session for this context
		session := s.Get(name)

		// Call the function with the session
		if !f(name, session) {
			break
		}
	}
}

func (s *FileSessionStore) Expire() {
	// Clean up sessions older than 7 days
	expiry := 7 * 24 * time.Hour
	now := time.Now()

	entries, err := os.ReadDir(s.baseDir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}

		filePath := filepath.Join(s.baseDir, entry.Name())
		lockPath := filePath + ".lock"

		// Try to acquire lock to check if session is in use
		fileLock := flock.New(lockPath)
		locked, err := fileLock.TryLock()
		if err != nil || !locked {
			// Session is in use or error, skip
			continue
		}

		data, err := os.ReadFile(filePath)
		if err != nil {
			fileLock.Unlock()
			continue
		}

		var session FileSession
		if err := json.Unmarshal(data, &session); err != nil {
			fileLock.Unlock()
			continue
		}

		if now.Sub(session.Updated) > expiry {
			os.Remove(filePath)
			os.Remove(lockPath)
		}

		fileLock.Unlock()
	}
}

// ListContexts returns all available context names
func (s *FileSessionStore) ListContexts() ([]string, error) {
	s.indexMu.RLock()
	defer s.indexMu.RUnlock()
	
	var contexts []string
	for name := range s.index.Contexts {
		contexts = append(contexts, name)
	}
	return contexts, nil
}

// GetHistory returns a copy of the session history
func (s *FileSession) GetHistory() []messages.ChatMessage {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return CopyHistory(s.History)
}

// AddMessage adds a message to the session history
func (s *FileSession) AddMessage(msg messages.ChatMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.History = append(s.History, msg)
	s.Updated = time.Now()
	s.trimHistory()
	s.save()
}

// trimHistory limits the session history to MaxHistory messages
func (s *FileSession) trimHistory() {
	if s.config == nil {
		return
	}
	s.History = TrimHistory(s.History, s.config.MaxHistory)
}

// Clear clears the session history
func (s *FileSession) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.History = InitializeWithSystemPrompt(s.History, s.config)
	s.Updated = time.Now()
	s.save()
}

// save persists the session to disk
func (s *FileSession) save() error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0644)
}

// Close releases the file lock and removes the lock file
func (s *FileSession) Close() {
	if s.lock != nil {
		s.lock.Unlock()
		// Remove the lock file
		os.Remove(s.lock.Path())
		s.lock = nil
	}
}

// Index management methods

// withFileLock acquires a file lock and executes the provided function
func withFileLock(lockPath string, exclusive bool, fn func() error) error {
	fileLock := flock.New(lockPath)
	
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	
	var locked bool
	var err error
	if exclusive {
		locked, err = fileLock.TryLockContext(ctx, 100*time.Millisecond)
	} else {
		locked, err = fileLock.TryRLockContext(ctx, 100*time.Millisecond)
	}
	
	if err != nil {
		return fmt.Errorf("failed to acquire lock: %w", err)
	}
	if !locked {
		return fmt.Errorf("could not acquire lock within 5 seconds")
	}
	defer fileLock.Unlock()
	
	return fn()
}

// loadIndex loads the index from disk with file locking
func (s *FileSessionStore) loadIndex() error {
	s.indexMu.Lock()
	defer s.indexMu.Unlock()

	// Location: ~/.pollytool/index.json
	baseDir := filepath.Dir(s.baseDir) // Get ~/.pollytool from ~/.pollytool/contexts
	indexPath := filepath.Join(baseDir, "index.json")
	lockPath := indexPath + ".lock"

	return withFileLock(lockPath, false, func() error {
		data, err := os.ReadFile(indexPath)
		if err != nil {
			return err
		}

		var index ContextIndex
		if err := json.Unmarshal(data, &index); err != nil {
			return err
		}

		s.index = &index
		if s.index.Contexts == nil {
			s.index.Contexts = make(map[string]*ContextInfo)
		}
		return nil
	})
}

// saveIndex saves the index to disk with file locking
func (s *FileSessionStore) saveIndex() error {
	s.indexMu.RLock()
	defer s.indexMu.RUnlock()

	// Location: ~/.pollytool/index.json
	baseDir := filepath.Dir(s.baseDir) // Get ~/.pollytool from ~/.pollytool/contexts
	indexPath := filepath.Join(baseDir, "index.json")
	lockPath := indexPath + ".lock"

	return withFileLock(lockPath, true, func() error {
		data, err := json.MarshalIndent(s.index, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(indexPath, data, 0644)
	})
}

// ResolveContext now just returns the name as-is (kept for compatibility)
func (s *FileSessionStore) ResolveContext(name string) string {
	return name
}

// SaveContextName saves context metadata
func (s *FileSessionStore) SaveContextName(name, _ string) error {
	s.indexMu.Lock()
	if s.index.Contexts[name] == nil {
		s.index.Contexts[name] = &ContextInfo{
			Name:     name,
			Created:  time.Now(),
			LastUsed: time.Now(),
		}
	} else {
		s.index.Contexts[name].LastUsed = time.Now()
	}
	s.indexMu.Unlock()

	return s.saveIndex()
}

// SaveContextInfo saves full context information including model and settings
func (s *FileSessionStore) SaveContextInfo(info *ContextInfo) error {

	s.indexMu.Lock()
	// Preserve existing info if updating
	if existing, exists := s.index.Contexts[info.Name]; exists {
		if info.Model == "" {
			info.Model = existing.Model
		}
		if info.Temperature == 0 {
			info.Temperature = existing.Temperature
		}
		if info.MaxTokens == 0 {
			info.MaxTokens = existing.MaxTokens
		}
		if info.SystemPrompt == "" {
			info.SystemPrompt = existing.SystemPrompt
		}
		if len(info.ToolPaths) == 0 {
			info.ToolPaths = existing.ToolPaths
		}
		if len(info.MCPServers) == 0 {
			info.MCPServers = existing.MCPServers
		}
		if info.Created.IsZero() {
			info.Created = existing.Created
		}
	} else {
		if info.Created.IsZero() {
			info.Created = time.Now()
		}
	}

	info.LastUsed = time.Now()
	s.index.Contexts[info.Name] = info
	s.indexMu.Unlock()

	return s.saveIndex()
}

// GetLastContext returns the last used context name
func (s *FileSessionStore) GetLastContext() string {
	s.indexMu.RLock()
	defer s.indexMu.RUnlock()
	return s.index.LastContext
}

// SetLastContext updates the last used context
func (s *FileSessionStore) SetLastContext(name string) error {
	s.indexMu.Lock()
	s.index.LastContext = name

	// Update last used time for named contexts
	if info, exists := s.index.Contexts[name]; exists {
		info.LastUsed = time.Now()
	}
	s.indexMu.Unlock()

	return s.saveIndex()
}

// GetContextInfo returns information about all contexts
func (s *FileSessionStore) GetContextInfo() map[string]*ContextInfo {
	s.indexMu.RLock()
	defer s.indexMu.RUnlock()

	// Create a copy to avoid race conditions
	result := make(map[string]*ContextInfo)
	maps.Copy(result, s.index.Contexts)
	return result
}

// GetContextByNameOrID returns context info for a specific context
func (s *FileSessionStore) GetContextByNameOrID(name string) *ContextInfo {
	s.indexMu.RLock()
	defer s.indexMu.RUnlock()

	// Direct lookup by name
	if info, exists := s.index.Contexts[name]; exists {
		return info
	}

	return nil
}

// DeleteContextName removes a context name mapping
func (s *FileSessionStore) DeleteContextName(name string) error {
	s.indexMu.Lock()
	delete(s.index.Contexts, name)
	s.indexMu.Unlock()

	return s.saveIndex()
}

// ContextExists checks if a context with the given name exists
func (s *FileSessionStore) ContextExists(name string) bool {
	// Check if context exists in index (the index is the source of truth)
	return s.GetContextByNameOrID(name) != nil
}

// GetBaseDir returns the base directory for the file session store
func (s *FileSessionStore) GetBaseDir() string {
	return s.baseDir
}

// ClearIndex clears the entire index
func (s *FileSessionStore) ClearIndex() error {
	s.indexMu.Lock()
	s.index = &ContextIndex{
		Contexts: make(map[string]*ContextInfo),
	}
	s.indexMu.Unlock()

	return s.saveIndex()
}
