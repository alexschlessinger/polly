package sessions

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lithammer/shortuuid/v4"
	"github.com/pkdindustries/pollytool/messages"
)

// FileSession implements a file-based persistent session
type FileSession struct {
	ID      string                 `json:"id"`
	History []messages.ChatMessage `json:"history"`
	Created time.Time              `json:"created"`
	Updated time.Time              `json:"updated"`
	path    string
	file    *os.File // Keep file open for locking
}

// ContextInfo stores metadata about a context
type ContextInfo struct {
	ID           string    `json:"id"`
	Name         string    `json:"name,omitempty"`
	Created      time.Time `json:"created"`
	LastUsed     time.Time `json:"lastUsed"`
	Model        string    `json:"model,omitempty"`
	Temperature  float64   `json:"temperature,omitempty"`
	SystemPrompt string    `json:"systemPrompt,omitempty"`
	Description  string    `json:"description,omitempty"`
	ToolPaths    []string  `json:"toolPaths,omitempty"`
	MCPServers   []string  `json:"mcpServers,omitempty"`
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
}

// NewFileSessionStore creates a new file-based session store
func NewFileSessionStore(baseDir string) (SessionStore, error) {
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

// GenerateSessionID creates a new unique session ID using ShortUUID
// ShortUUID generates a base57-encoded UUID that's typically 22 characters long
func GenerateSessionID() string {
	return shortuuid.New()
}

// Get retrieves or creates a session
func (s *FileSessionStore) Get(id string) Session {
	// Generate ID if empty
	if id == "" {
		id = GenerateSessionID()
	}

	originalID := id

	// Resolve name to ID if needed
	if strings.HasPrefix(id, "@") {
		resolvedID := s.ResolveContext(id)
		if resolvedID == "" {
			// Name doesn't exist yet, create a new ID for it
			newID := GenerateSessionID()
			s.SaveContextName(originalID, newID)
			id = newID
		} else {
			id = resolvedID
		}
	}

	// Track last context (use actual ID)
	s.SetLastContext(id)

	sessionPath := filepath.Join(s.baseDir, id+".json")
	lockPath := sessionPath + ".lock"

	// Open lock file with exclusive access
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		// Fall back to creating session without lock
		fmt.Fprintf(os.Stderr, "Warning: Could not create lock file: %v\n", err)
		lockFile = nil
	}

	// Try to acquire exclusive lock (blocking)
	if lockFile != nil {
		if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
			lockFile.Close()
			fmt.Fprintf(os.Stderr, "Warning: Could not acquire lock: %v\n", err)
			lockFile = nil
		}
	}

	// Try to load existing session
	if data, err := os.ReadFile(sessionPath); err == nil {
		var session FileSession
		if err := json.Unmarshal(data, &session); err == nil {
			session.path = sessionPath
			session.file = lockFile
			session.Updated = time.Now()
			session.save()
			return &session
		}
	}

	// Create new session
	session := &FileSession{
		ID:      id,
		History: []messages.ChatMessage{},
		Created: time.Now(),
		Updated: time.Now(),
		path:    sessionPath,
		file:    lockFile,
	}
	session.save()
	return session
}

// Delete removes a session
func (s *FileSessionStore) Delete(id string) {
	// Resolve name to ID if needed
	originalID := id
	if strings.HasPrefix(id, "@") {
		id = s.ResolveContext(id)
		if id == "" {
			return // Name doesn't exist
		}
		// Remove the name mapping
		s.DeleteContextName(originalID)
	}

	sessionPath := filepath.Join(s.baseDir, id+".json")
	lockPath := sessionPath + ".lock"
	os.Remove(sessionPath)
	os.Remove(lockPath)

	// Also remove any name mappings that point to this ID
	for name, info := range s.GetContextInfo() {
		if info.ID == id {
			s.DeleteContextName(name)
		}
	}
}

// Expire removes old sessions
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
		lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
		if err != nil {
			continue
		}

		if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			// Session is in use, skip
			lockFile.Close()
			continue
		}

		data, err := os.ReadFile(filePath)
		if err != nil {
			lockFile.Close()
			continue
		}

		var session FileSession
		if err := json.Unmarshal(data, &session); err != nil {
			lockFile.Close()
			continue
		}

		if now.Sub(session.Updated) > expiry {
			os.Remove(filePath)
			os.Remove(lockPath)
		}

		syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		lockFile.Close()
	}
}

// ListContexts returns all available context IDs
func (s *FileSessionStore) ListContexts() ([]string, error) {
	entries, err := os.ReadDir(s.baseDir)
	if err != nil {
		return nil, err
	}

	var sessions []string
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".json" && !strings.HasPrefix(entry.Name(), ".") {
			sessionID := entry.Name()[:len(entry.Name())-5] // Remove .json extension
			sessions = append(sessions, sessionID)
		}
	}
	return sessions, nil
}

// GetHistory returns a copy of the session history
func (s *FileSession) GetHistory() []messages.ChatMessage {
	history := make([]messages.ChatMessage, len(s.History))
	copy(history, s.History)
	return history
}

// AddMessage adds a message to the session history
func (s *FileSession) AddMessage(msg messages.ChatMessage) {
	s.History = append(s.History, msg)
	s.Updated = time.Now()
	s.save()
}

// Clear clears the session history
func (s *FileSession) Clear() {
	s.History = []messages.ChatMessage{}
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
	if s.file != nil {
		syscall.Flock(int(s.file.Fd()), syscall.LOCK_UN)
		lockPath := s.file.Name()
		s.file.Close()
		s.file = nil
		// Remove the lock file
		os.Remove(lockPath)
	}
}

// Index management methods

// loadIndex loads the index from disk
func (s *FileSessionStore) loadIndex() error {
	s.indexMu.Lock()
	defer s.indexMu.Unlock()

	indexPath := filepath.Join(s.baseDir, ".index.json")
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
}

// saveIndex saves the index to disk
func (s *FileSessionStore) saveIndex() error {
	s.indexMu.RLock()
	defer s.indexMu.RUnlock()

	indexPath := filepath.Join(s.baseDir, ".index.json")
	data, err := json.MarshalIndent(s.index, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(indexPath, data, 0644)
}

// ResolveContext resolves a context name or ID to an actual ID
func (s *FileSessionStore) ResolveContext(nameOrID string) string {
	if !strings.HasPrefix(nameOrID, "@") {
		return nameOrID // It's already an ID
	}

	s.indexMu.RLock()
	defer s.indexMu.RUnlock()

	if info, exists := s.index.Contexts[nameOrID]; exists {
		return info.ID
	}
	return "" // Name not found
}

// SaveContextName saves a name mapping for a context
func (s *FileSessionStore) SaveContextName(name, id string) error {
	if !strings.HasPrefix(name, "@") {
		name = "@" + name
	}

	s.indexMu.Lock()
	s.index.Contexts[name] = &ContextInfo{
		ID:       id,
		Name:     name,
		Created:  time.Now(),
		LastUsed: time.Now(),
	}
	s.indexMu.Unlock()

	return s.saveIndex()
}

// SaveContextInfo saves full context information including model and settings
func (s *FileSessionStore) SaveContextInfo(info *ContextInfo) error {
	if !strings.HasPrefix(info.Name, "@") {
		info.Name = "@" + info.Name
	}

	s.indexMu.Lock()
	// Preserve existing info if updating
	if existing, exists := s.index.Contexts[info.Name]; exists {
		if info.Model == "" {
			info.Model = existing.Model
		}
		if info.Temperature == 0 {
			info.Temperature = existing.Temperature
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

// GetLastContext returns the last used context ID
func (s *FileSessionStore) GetLastContext() string {
	s.indexMu.RLock()
	defer s.indexMu.RUnlock()
	return s.index.LastContext
}

// SetLastContext updates the last used context
func (s *FileSessionStore) SetLastContext(id string) error {
	s.indexMu.Lock()
	s.index.LastContext = id

	// Update last used time for named contexts
	for _, info := range s.index.Contexts {
		if info.ID == id {
			info.LastUsed = time.Now()
			break
		}
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
	for k, v := range s.index.Contexts {
		result[k] = v
	}
	return result
}

// GetContextByNameOrID returns context info for a specific context
func (s *FileSessionStore) GetContextByNameOrID(nameOrID string) *ContextInfo {
	s.indexMu.RLock()
	defer s.indexMu.RUnlock()

	// Try as name first
	if info, exists := s.index.Contexts[nameOrID]; exists {
		return info
	}

	// Try to find by ID
	for _, info := range s.index.Contexts {
		if info.ID == nameOrID {
			return info
		}
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

// ContextExists checks if a context with the given ID exists
func (s *FileSessionStore) ContextExists(id string) bool {
	// Check if it's a named context
	if strings.HasPrefix(id, "@") {
		return s.ResolveContext(id) != ""
	}

	// Check if session file exists
	sessionPath := filepath.Join(s.baseDir, id+".json")
	_, err := os.Stat(sessionPath)
	return err == nil
}

// GetBaseDir returns the base directory for the file session store
func (s *FileSessionStore) GetBaseDir() string {
	return s.baseDir
}
