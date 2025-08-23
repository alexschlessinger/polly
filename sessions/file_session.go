package sessions

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/gofrs/flock"
)

// FileSession implements a file-based persistent session
type FileSession struct {
	ID          string                 `json:"id"`
	History     []messages.ChatMessage `json:"history"`
	Created     time.Time              `json:"created"`
	Updated     time.Time              `json:"updated"`
	ContextInfo *ContextInfo           `json:"contextInfo"`
	path        string
	lock        *flock.Flock // File lock using flock
	mu          sync.RWMutex
}

// ContextInfo stores metadata about a context
type ContextInfo struct {
	Name           string        `json:"name"` // Name is the primary identifier (e.g., "@stocks" or random ID)
	Created        time.Time     `json:"created"`
	LastUsed       time.Time     `json:"lastUsed"`
	Model          string        `json:"model,omitempty"`
	Temperature    float64       `json:"temperature,omitempty"`
	SystemPrompt   string        `json:"systemPrompt,omitempty"`
	Description    string        `json:"description,omitempty"`
	ToolPaths      []string      `json:"toolPaths,omitempty"`
	MCPServers     []string      `json:"mcpServers,omitempty"`
	MaxTokens      int           `json:"maxTokens,omitempty"`
	MaxHistory     int           `json:"maxHistory,omitempty"` // Maximum messages to keep (0 = unlimited)
	TTL            time.Duration `json:"ttl,omitempty"`        // Time before context expires (0 = never)
	ThinkingEffort string        `json:"thinkingEffort,omitempty"` // Thinking effort level (e.g., "low", "medium", "high")
}

// IndexEntry is a lightweight reference for fast lookups
type IndexEntry struct {
	Name     string    `json:"name"`
	LastUsed time.Time `json:"lastUsed"`
}

// ContextIndex manages the mapping of names to lightweight references
type ContextIndex struct {
	Entries     map[string]*IndexEntry `json:"entries"`
	LastContext string                 `json:"lastContext,omitempty"`
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
			Entries: make(map[string]*IndexEntry),
		}
	}

	// Cleanup any legacy lock file (index.json.lock) from previous versions
	base := filepath.Dir(store.baseDir)
	legacyLock := filepath.Join(base, "index.json.lock")
	_ = os.Remove(legacyLock)

	return store, nil
}

// validateContextName checks if a context name is valid for filesystem use
func validateContextName(name string) error {
	if name == "" {
		return fmt.Errorf("context name cannot be empty")
	}

	// Check for problematic characters that could cause filesystem issues
	if strings.ContainsAny(name, "/\\:*?\"<>|") {
		return fmt.Errorf("context name contains invalid characters (/, \\, :, *, ?, \", <, >, |)")
	}

	// Check for names that could be problematic on any OS
	if name == "." || name == ".." {
		return fmt.Errorf("context name cannot be '.' or '..'")
	}

	// Check for names starting or ending with spaces or dots
	if strings.HasPrefix(name, " ") || strings.HasSuffix(name, " ") {
		return fmt.Errorf("context name cannot start or end with spaces")
	}
	if strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") {
		return fmt.Errorf("context name cannot start or end with dots")
	}

	// Check for control characters
	for _, r := range name {
		if r < 32 || r == 127 {
			return fmt.Errorf("context name contains control characters")
		}
	}

	return nil
}

// Get retrieves or creates a session
func (s *FileSessionStore) Get(name string) (Session, error) {
	// Validate context name for filesystem safety
	if err := validateContextName(name); err != nil {
		return nil, fmt.Errorf("invalid context name '%s': %w", name, err)
	}

	// Update last context in index only when creating or modifying
	// Not on every read to avoid unnecessary disk writes

	// Use name directly as filename (already validated at app level)
	sessionPath := filepath.Join(s.baseDir, name+".json")

	// Lock the session file itself (no separate .lock file)
	fileLock := flock.New(sessionPath)

	// Try to acquire exclusive lock with 10 second timeout, retrying every 100ms
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	locked, err := fileLock.TryLockContext(ctx, 100*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("failed to acquire lock: %w", err)
	}
	if !locked {
		return nil, fmt.Errorf("could not acquire lock within 10 seconds")
	}

	// Try to load existing session
	if data, err := os.ReadFile(sessionPath); err == nil {
		var session FileSession
		if err := json.Unmarshal(data, &session); err == nil {
			session.path = sessionPath
			session.lock = fileLock
			session.Updated = time.Now()

			// Ensure ContextInfo exists
			if session.ContextInfo == nil {
				session.ContextInfo = &ContextInfo{
					Name:         name,
					Created:      session.Created,
					LastUsed:     time.Now(),
					SystemPrompt: s.config.SystemPrompt,
					MaxHistory:   s.config.MaxHistory,
					TTL:          s.config.TTL,
				}
			} else {
				session.ContextInfo.LastUsed = time.Now()
			}

			// Update index with lightweight entry
			s.indexMu.Lock()
			s.index.Entries[name] = &IndexEntry{
				Name:     name,
				LastUsed: time.Now(),
			}
			s.index.LastContext = name
			s.indexMu.Unlock()
			s.saveIndex()

			session.save()
			return &session, nil
		}
	}

	// Create new session
	session := &FileSession{
		ID:      name,
		History: []messages.ChatMessage{},
		Created: time.Now(),
		Updated: time.Now(),
		ContextInfo: &ContextInfo{
			Name:         name,
			Created:      time.Now(),
			LastUsed:     time.Now(),
			SystemPrompt: s.config.SystemPrompt,
			MaxHistory:   s.config.MaxHistory,
			TTL:          s.config.TTL,
		},
		path: sessionPath,
		lock: fileLock,
	}
	// Initialize with system prompt if configured
	config := &SessionConfig{
		SystemPrompt: session.ContextInfo.SystemPrompt,
		MaxHistory:   session.ContextInfo.MaxHistory,
		TTL:          session.ContextInfo.TTL,
	}
	session.History = InitializeWithSystemPrompt(session.History, config)

	// Create index entry and mark as last used
	s.indexMu.Lock()
	s.index.Entries[name] = &IndexEntry{
		Name:     name,
		LastUsed: time.Now(),
	}
	s.index.LastContext = name
	s.indexMu.Unlock()
	s.saveIndex()

	session.save()
	return session, nil
}

// Delete removes a session
func (s *FileSessionStore) Delete(name string) {
	// Use name directly (already validated at app level)
	sessionPath := filepath.Join(s.baseDir, name+".json")

	// Unlink the session file even if another process holds it; open FDs will keep it
	// alive for that process, but it disappears for new reads, matching expected semantics.
	_ = os.Remove(sessionPath)

	// Remove from index if present
	s.indexMu.Lock()
	delete(s.index.Entries, name)
	s.indexMu.Unlock()
	s.saveIndex()
}

// Expire removes old sessions
// Range iterates over all sessions using the index
func (s *FileSessionStore) Range(f func(key, value any) bool) {
	s.indexMu.RLock()
	defer s.indexMu.RUnlock()

	// Iterate over contexts in the index
	for name := range s.index.Entries {
		// Load the session for this context
		session, err := s.Get(name)
		if err != nil {
			continue // Skip sessions that can't be loaded
		}

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
		// Try to acquire lock on the session file to check if session is in use
		fileLock := flock.New(filePath)
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
		}

		fileLock.Unlock()
	}
}

// List returns all available context names
func (s *FileSessionStore) List() ([]string, error) {
	s.indexMu.RLock()
	defer s.indexMu.RUnlock()

	var contexts []string
	for name := range s.index.Entries {
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
	if s.ContextInfo.MaxHistory > 0 {
		s.History = TrimHistory(s.History, s.ContextInfo.MaxHistory)
	}
}

// Clear clears the session history
func (s *FileSession) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Create a temporary config from context info for initialization
	config := &SessionConfig{
		SystemPrompt: s.ContextInfo.SystemPrompt,
		MaxHistory:   s.ContextInfo.MaxHistory,
		TTL:          s.ContextInfo.TTL,
	}

	s.History = InitializeWithSystemPrompt(s.History, config)
	s.Updated = time.Now()
	s.save()
}

// GetName returns the session name
func (s *FileSession) GetName() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ID
}

// GetContextInfo returns the context metadata
func (s *FileSession) GetContextInfo() *ContextInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ContextInfo
}

// SetContextInfo updates the context metadata
func (s *FileSession) SetContextInfo(info *ContextInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ContextInfo = info
	s.Updated = time.Now()
	s.save()
}

// UpdateContextInfo applies a partial update to the context metadata
func (s *FileSession) UpdateContextInfo(update *ContextUpdate) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	
	// Apply the update to current context info
	s.ContextInfo = ApplyContextUpdate(s.ContextInfo, update)
	s.Updated = time.Now()
	return s.save()
}

// GetLastUsed returns when the session was last accessed
func (s *FileSession) GetLastUsed() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Updated
}

// save persists the session to disk
func (s *FileSession) save() error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0644)
}

// Close releases the file lock on the session file.
// No files are removed here; the lock is ephemeral.
func (s *FileSession) Close() {
	if s.lock != nil {
		s.lock.Unlock()
		s.lock = nil
	}
}

// Index management methods

// withFileLock acquires a file lock and executes the provided function
func withFileLock(lockPath string, exclusive bool, fn func() error) error {
	// Ensure the lock target file exists so flock can operate reliably
	if _, err := os.Stat(lockPath); os.IsNotExist(err) {
		if f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644); err == nil {
			f.Close()
		}
	}

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

	// Lock the index file itself (shared) to avoid a separate persistent lock file
	return withFileLock(indexPath, false, func() error {
		data, err := os.ReadFile(indexPath)
		if err != nil {
			return err
		}

		var index ContextIndex
		if err := json.Unmarshal(data, &index); err != nil {
			return err
		}

		s.index = &index
		if s.index.Entries == nil {
			s.index.Entries = make(map[string]*IndexEntry)
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

	// Lock the index file itself (exclusive) to avoid a separate persistent lock file
	return withFileLock(indexPath, true, func() error {
		data, err := json.MarshalIndent(s.index, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(indexPath, data, 0644)
	})
}


// GetLastContext returns the last used context name
func (s *FileSessionStore) GetLastContext() string {
	s.indexMu.RLock()
	defer s.indexMu.RUnlock()
	return s.index.LastContext
}

// GetAllContextInfo returns information about all contexts
func (s *FileSessionStore) GetAllContextInfo() map[string]*ContextInfo {
	s.indexMu.RLock()
	entries := make(map[string]*IndexEntry)
	maps.Copy(entries, s.index.Entries)
	s.indexMu.RUnlock()

	result := make(map[string]*ContextInfo)

	// Load context info from actual session files
	for name := range entries {
		sessionPath := filepath.Join(s.baseDir, name+".json")
		if data, err := os.ReadFile(sessionPath); err == nil {
			var session FileSession
			if err := json.Unmarshal(data, &session); err == nil && session.ContextInfo != nil {
				result[name] = session.ContextInfo
			}
		}
	}

	return result
}

// Exists checks if a context with the given name exists
func (s *FileSessionStore) Exists(name string) bool {
	s.indexMu.RLock()
	defer s.indexMu.RUnlock()
	_, exists := s.index.Entries[name]
	return exists
}

// GetBaseDir returns the base directory for the file session store
func (s *FileSessionStore) GetBaseDir() string {
	return s.baseDir
}
