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
	"github.com/alexschlessinger/pollytool/tools"
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

	sessionPath := filepath.Join(s.baseDir, name+".json")

	// Lock a dedicated lock file rather than the data file: atomic saves replace
	// the data file's inode via rename, which would drop a lock held on it.
	fileLock := flock.New(lockPath(sessionPath))

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

	// Try to load an existing session.
	data, readErr := os.ReadFile(sessionPath)
	switch {
	case readErr == nil && len(data) > 0:
		var session FileSession
		if err := json.Unmarshal(data, &session); err != nil {
			// The file exists but is unreadable. Preserve the original bytes for
			// recovery instead of silently discarding them, then fall through to
			// create a fresh session.
			if _, backupErr := backupCorruptSession(sessionPath); backupErr != nil {
				fileLock.Unlock()
				return nil, fmt.Errorf("failed to preserve corrupt session '%s': %w", name, backupErr)
			}
		} else {
			session.path = sessionPath
			session.lock = fileLock

			if session.Metadata == nil {
				fileLock.Unlock()
				return nil, fmt.Errorf("session '%s' has no metadata", name)
			}

			// Loading an existing session is a pure read: attach the runtime
			// handles but do not bump timestamps or rewrite the file. The next
			// real mutation (AddMessage/SetMetadata/UpdateMetadata) persists any
			// changes, so merely opening a context costs no disk write.
			return &session, nil
		}
	case readErr != nil && !os.IsNotExist(readErr):
		// A real read error (permissions, I/O): do not clobber the file.
		fileLock.Unlock()
		return nil, fmt.Errorf("failed to read session '%s': %w", name, readErr)
	}

	// File absent, empty, or corrupt-and-backed-up: create a new session.
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

	if err := session.save(); err != nil {
		fileLock.Unlock()
		return nil, fmt.Errorf("failed to persist new session '%s': %w", name, err)
	}
	return session, nil
}

// Delete removes a session
func (s *FileSessionStore) Delete(name string) {
	// Validate the name so a malformed/hostile name can't escape baseDir.
	if err := validateContextName(name); err != nil {
		return
	}
	sessionPath := filepath.Join(s.baseDir, name+".json")

	fileLock := flock.New(lockPath(sessionPath))
	locked, err := fileLock.TryLock()
	if err != nil || !locked {
		return
	}
	defer fileLock.Unlock()

	// Leave the dedicated lock file in place so future sessions keep
	// coordinating on the same filesystem path.
	_ = os.Remove(sessionPath)
}

// Range iterates over all sessions. It is read-only: each session is loaded
// directly without taking a lock or rewriting it, so iteration neither leaks
// file locks nor mutates sessions on disk.
func (s *FileSessionStore) Range(f func(key, value any) bool) {
	entries, err := os.ReadDir(s.baseDir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}

		sessionPath := filepath.Join(s.baseDir, entry.Name())
		data, err := os.ReadFile(sessionPath)
		if err != nil {
			continue // Skip sessions that can't be read
		}

		var session FileSession
		if err := json.Unmarshal(data, &session); err != nil {
			continue // Skip sessions that can't be parsed
		}

		name := strings.TrimSuffix(entry.Name(), ".json")
		if !f(name, &session) {
			break
		}
	}
}

// Expire removes sessions that have outlived their TTL. The effective TTL is the
// session's own TTL, then the store's default TTL, and finally a 7-day safety net
// when neither is configured (so the directory can't grow without bound).
func (s *FileSessionStore) Expire() {
	const defaultExpiry = 7 * 24 * time.Hour
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
		// Try to acquire the session's lock to check if it is in use
		fileLock := flock.New(lockPath(filePath))
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

		expiry := defaultExpiry
		if session.Metadata != nil && session.Metadata.TTL > 0 {
			expiry = session.Metadata.TTL
		} else if s.defaultInfo != nil && s.defaultInfo.TTL > 0 {
			expiry = s.defaultInfo.TTL
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

// AddMessage adds a message to the session history and persists it.
func (s *FileSession) AddMessage(msg messages.ChatMessage) error {
	return s.AddMessages([]messages.ChatMessage{msg})
}

// AddMessages appends a batch of messages and persists them with a single
// write. A whole agentic turn (the assistant message for each iteration plus
// every tool result) is replayed at once; adding them one at a time would
// rewrite the entire history file once per message. A persistence failure is
// returned so callers can surface it rather than silently losing the messages.
func (s *FileSession) AddMessages(msgs []messages.ChatMessage) error {
	if len(msgs) == 0 {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	candidate, err := s.candidateForWriteLocked()
	if err != nil {
		return err
	}

	now := time.Now()
	candidate.History = append(candidate.History, msgs...)
	candidate.Updated = now
	candidate.touchMetadata(now)
	candidate.trimHistory()
	if err := candidate.save(); err != nil {
		return err
	}

	s.commitCandidateLocked(candidate)
	return nil
}

// trimHistory limits the session history to MaxHistoryTokens
func (s *FileSession) trimHistory() {
	if s.Metadata != nil && s.Metadata.MaxHistoryTokens > 0 {
		s.History = TrimHistory(s.History, s.Metadata.MaxHistoryTokens)
	}
}

// Clear clears the session history
func (s *FileSession) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	candidate, err := s.candidateForWriteLocked()
	if err != nil {
		return err
	}

	// Clear history and re-initialize with system prompt if configured
	candidate.History = candidate.History[:0]
	if candidate.Metadata.SystemPrompt != "" {
		candidate.History = append(candidate.History, messages.ChatMessage{
			Role:    messages.MessageRoleSystem,
			Content: candidate.Metadata.SystemPrompt,
		})
	}
	now := time.Now()
	candidate.Updated = now
	candidate.touchMetadata(now)
	if err := candidate.save(); err != nil {
		return err
	}

	s.commitCandidateLocked(candidate)
	return nil
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
	return cloneMetadata(s.Metadata)
}

// SetMetadata updates the context metadata
func (s *FileSession) SetMetadata(info *Metadata) error {
	if info == nil {
		return fmt.Errorf("metadata cannot be nil")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	candidate, err := s.metadataCandidateLocked()
	if err != nil {
		return err
	}

	now := time.Now()
	metadata := cloneMetadata(info)
	if metadata.Name == "" {
		metadata.Name = s.ID
	}
	if metadata.Created.IsZero() {
		metadata.Created = s.Created
	}
	metadata.LastUsed = now
	candidate.Metadata = metadata
	candidate.Updated = now
	if err := candidate.save(); err != nil {
		return err
	}

	s.commitCandidateLocked(candidate)
	return nil
}

// UpdateMetadata applies a partial update to the context metadata
func (s *FileSession) UpdateMetadata(update *Metadata) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	candidate, err := s.metadataCandidateLocked()
	if err != nil {
		return err
	}

	// Apply the update to current context info (only non-zero values)
	now := time.Now()
	candidate.Metadata = MergeMetadata(candidate.Metadata, update)
	candidate.touchMetadata(now)
	candidate.Updated = now
	if err := candidate.save(); err != nil {
		return err
	}

	s.commitCandidateLocked(candidate)
	return nil
}

// GetLastUsed returns when the session was last accessed
func (s *FileSession) GetLastUsed() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Updated
}

// candidateForWriteLocked builds a working copy for mutations that change
// history. History is copied so an interrupted save never leaves the in-memory
// slice diverging from what reached disk; the candidate is only committed back
// once save() succeeds.
func (s *FileSession) candidateForWriteLocked() (*FileSession, error) {
	candidate, err := s.metadataCandidateLocked()
	if err != nil {
		return nil, err
	}
	candidate.History = CopyHistory(s.History)
	return candidate, nil
}

// metadataCandidateLocked builds a working copy for mutations that touch only
// metadata. It shares the existing history slice rather than cloning it: these
// paths never read or mutate history beyond marshalling it unchanged, so the
// O(len(history)) copy would be pure waste. The clone happens in
// candidateForWriteLocked for paths that actually modify history.
func (s *FileSession) metadataCandidateLocked() (*FileSession, error) {
	if s.Metadata == nil {
		return nil, fmt.Errorf("session '%s' has no metadata", s.ID)
	}

	candidate := &FileSession{
		ID:       s.ID,
		History:  s.History,
		Created:  s.Created,
		Updated:  s.Updated,
		Metadata: cloneMetadata(s.Metadata),
		path:     s.path,
	}
	return candidate, nil
}

func (s *FileSession) commitCandidateLocked(candidate *FileSession) {
	s.History = candidate.History
	s.Updated = candidate.Updated
	s.Metadata = candidate.Metadata
}

func (s *FileSession) touchMetadata(now time.Time) {
	if s.Metadata == nil {
		return
	}
	if s.Metadata.Name == "" {
		s.Metadata.Name = s.ID
	}
	if s.Metadata.Created.IsZero() {
		s.Metadata.Created = s.Created
	}
	s.Metadata.LastUsed = now
}

func cloneMetadata(metadata *Metadata) *Metadata {
	if metadata == nil {
		return nil
	}

	out := *metadata
	if metadata.ActiveTools != nil {
		out.ActiveTools = append([]tools.ToolLoaderInfo(nil), metadata.ActiveTools...)
	}
	if metadata.ActiveSkills != nil {
		out.ActiveSkills = append([]string(nil), metadata.ActiveSkills...)
	}
	if metadata.SkillDirs != nil {
		out.SkillDirs = append([]string(nil), metadata.SkillDirs...)
	}
	if metadata.SkillSources != nil {
		out.SkillSources = append([]string(nil), metadata.SkillSources...)
	}
	return &out
}

// save persists the session to disk atomically (write temp + fsync + rename),
// so an interrupted write can never leave a truncated/corrupt session file.
func (s *FileSession) save() error {
	if s.path == "" {
		return fmt.Errorf("session has no backing file")
	}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(s.path, data, 0600)
}

// atomicWriteFile writes data to a temporary file in the same directory as path,
// flushes it to stable storage, then renames it over path. The rename is atomic
// on POSIX filesystems, so readers and crash recovery only ever observe the old
// complete file or the new complete file, never a partial write.
var atomicWriteFile = func(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	if err = tmp.Chmod(perm); err != nil {
		return err
	}
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	err = os.Rename(tmpName, path)
	return err
}

// backupCorruptSession moves an unreadable session file aside without
// overwriting an existing recovery file.
func backupCorruptSession(path string) (string, error) {
	candidates := []string{path + ".corrupt"}
	stamp := time.Now().UTC().Format("20060102T150405.000000000")
	for i := range 10 {
		candidates = append(candidates, fmt.Sprintf("%s.corrupt.%s.%d", path, stamp, i))
	}

	for _, candidate := range candidates {
		if _, err := os.Lstat(candidate); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return "", err
		}

		if err := os.Rename(path, candidate); err != nil {
			return "", err
		}
		return candidate, nil
	}

	return "", fmt.Errorf("could not find a unique corrupt-session backup path for %s", path)
}

// lockPath returns the path of the dedicated lock file guarding a session file.
// The lock is kept separate from the data file because atomic saves replace the
// data file's inode via rename, which would silently drop a flock held on it.
func lockPath(sessionPath string) string {
	return sessionPath + ".lock"
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
	// Validate the name so we never stat paths outside baseDir.
	if err := validateContextName(name); err != nil {
		return false
	}
	sessionPath := filepath.Join(s.baseDir, name+".json")
	_, err := os.Stat(sessionPath)
	return err == nil
}

// GetBaseDir returns the base directory for the file session store
func (s *FileSessionStore) GetBaseDir() string {
	return s.baseDir
}

// GetTotalTokens returns the sum of all message tokens in history
func (s *FileSession) GetTotalTokens() int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	total := 0
	for _, msg := range s.History {
		total += GetMessageTokens(msg)
	}
	return total
}

// GetCapacityPercentage returns the percentage of capacity used (0-100)
// Returns 0 if no limit is set
func (s *FileSession) GetCapacityPercentage() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.Metadata == nil || s.Metadata.MaxHistoryTokens == 0 {
		return 0 // No limit set
	}

	total := 0
	for _, msg := range s.History {
		total += GetMessageTokens(msg)
	}

	return float64(total) / float64(s.Metadata.MaxHistoryTokens) * 100
}

// GetTimeToExpiry returns the time remaining until the session expires
// Returns 0 if no TTL is set or if the session has already expired
func (s *FileSession) GetTimeToExpiry() time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.Metadata == nil || s.Metadata.TTL == 0 {
		return 0 // No expiry
	}

	remaining := s.Metadata.TTL - time.Since(s.Updated)
	if remaining < 0 {
		return 0 // Expired
	}
	return remaining
}

// GetMessageCounts returns the count of messages by role
func (s *FileSession) GetMessageCounts() map[string]int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	counts := make(map[string]int)
	for _, msg := range s.History {
		counts[string(msg.Role)]++
	}
	return counts
}

// GetToolCallCount returns the total number of tool calls in the session
func (s *FileSession) GetToolCallCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	total := 0
	for _, msg := range s.History {
		total += len(msg.ToolCalls)
	}
	return total
}
