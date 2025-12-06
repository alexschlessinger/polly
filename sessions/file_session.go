package sessions

import (
	"context"
	"encoding/json"
	"fmt"
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
	ID       string                 `json:"id"`
	History  []messages.ChatMessage `json:"history"`
	Created  time.Time              `json:"created"`
	Updated  time.Time              `json:"updated"`
	Metadata *Metadata              `json:"metadata"`
	path     string
	lock     *flock.Flock // File lock using flock
	mu       sync.RWMutex
}

// FileSessionStore implements a file-based session store
type FileSessionStore struct {
	baseDir     string
	defaultInfo *Metadata // Default values for new contexts
}

// NewFileSessionStore creates a new file-based session store
func NewFileSessionStore(baseDir string, defaultInfo *Metadata) (SessionStore, error) {
	// Use empty defaults if none provided
	if defaultInfo == nil {
		defaultInfo = &Metadata{}
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
		baseDir:     baseDir,
		defaultInfo: defaultInfo,
	}

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
			if session.Metadata == nil {
				session.Metadata = &Metadata{
					Name:             name,
					Created:          session.Created,
					LastUsed:         time.Now(),
					SystemPrompt:     s.defaultInfo.SystemPrompt,
					MaxHistoryTokens: s.defaultInfo.MaxHistoryTokens,
					TTL:              s.defaultInfo.TTL,
				}
			} else {
				session.Metadata.LastUsed = time.Now()
			}

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
		Metadata: &Metadata{
			Name:             name,
			Created:          time.Now(),
			LastUsed:         time.Now(),
			SystemPrompt:     s.defaultInfo.SystemPrompt,
			MaxHistoryTokens: s.defaultInfo.MaxHistoryTokens,
			TTL:              s.defaultInfo.TTL,
		},
		path: sessionPath,
		lock: fileLock,
	}
	// Initialize with system prompt if configured
	if session.Metadata.SystemPrompt != "" {
		session.History = append(session.History, messages.ChatMessage{
			Role:    messages.MessageRoleSystem,
			Content: session.Metadata.SystemPrompt,
		})
	}

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
}

// Range iterates over all sessions
func (s *FileSessionStore) Range(f func(key, value any) bool) {
	entries, err := os.ReadDir(s.baseDir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}

		name := strings.TrimSuffix(entry.Name(), ".json")
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
	entries, err := os.ReadDir(s.baseDir)
	if err != nil {
		return nil, err
	}

	var contexts []string
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".json" {
			name := strings.TrimSuffix(entry.Name(), ".json")
			contexts = append(contexts, name)
		}
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

// trimHistory limits the session history to MaxHistoryTokens
func (s *FileSession) trimHistory() {
	if s.Metadata.MaxHistoryTokens > 0 {
		s.History = TrimHistory(s.History, s.Metadata.MaxHistoryTokens)
	}
}

// Clear clears the session history
func (s *FileSession) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Clear history and re-initialize with system prompt if configured
	s.History = s.History[:0]
	if s.Metadata.SystemPrompt != "" {
		s.History = append(s.History, messages.ChatMessage{
			Role:    messages.MessageRoleSystem,
			Content: s.Metadata.SystemPrompt,
		})
	}
	s.Updated = time.Now()
	s.save()
}

// GetName returns the session name
func (s *FileSession) GetName() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ID
}

// GetMetadata returns the context metadata
func (s *FileSession) GetMetadata() *Metadata {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Metadata
}

// SetMetadata updates the context metadata
func (s *FileSession) SetMetadata(info *Metadata) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Metadata = info
	s.Updated = time.Now()
	s.save()
}

// UpdateMetadata applies a partial update to the context metadata
func (s *FileSession) UpdateMetadata(update *Metadata) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Apply the update to current context info (only non-zero values)
	s.Metadata = MergeMetadata(s.Metadata, update)
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

// GetLast returns the last used context name based on file modification time
func (s *FileSessionStore) GetLast() string {
	entries, err := os.ReadDir(s.baseDir)
	if err != nil {
		return ""
	}

	var lastFile string
	var lastTime time.Time

	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		if info.ModTime().After(lastTime) {
			lastTime = info.ModTime()
			lastFile = strings.TrimSuffix(entry.Name(), ".json")
		}
	}

	return lastFile
}

// GetAllMetadata returns information about all contexts
func (s *FileSessionStore) GetAllMetadata() map[string]*Metadata {
	result := make(map[string]*Metadata)

	entries, err := os.ReadDir(s.baseDir)
	if err != nil {
		return result
	}

	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}

		name := strings.TrimSuffix(entry.Name(), ".json")
		sessionPath := filepath.Join(s.baseDir, name+".json")

		if data, err := os.ReadFile(sessionPath); err == nil {
			var session FileSession
			if err := json.Unmarshal(data, &session); err == nil && session.Metadata != nil {
				result[name] = session.Metadata
			}
		}
	}

	return result
}

// Exists checks if a context with the given name exists
func (s *FileSessionStore) Exists(name string) bool {
	sessionPath := filepath.Join(s.baseDir, name+".json")
	_, err := os.Stat(sessionPath)
	return err == nil
}

// GetBaseDir returns the base directory for the file session store
func (s *FileSessionStore) GetBaseDir() string {
	return s.baseDir
}
