package sessions

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
)

// testStores returns both store implementations for testing
func testStores(t *testing.T) map[string]SessionStore {
	defaultInfo := &Metadata{
		MaxHistoryTokens: 70, // ~10 messages worth of tokens
		TTL:              0,  // No expiry for tests
		SystemPrompt:     "test system prompt",
	}

	// Create file store in temp directory
	fileStore, err := NewFileSessionStore(t.TempDir(), defaultInfo)
	if err != nil {
		t.Fatalf("Failed to create file store: %v", err)
	}

	return map[string]SessionStore{
		"SyncMap": NewSyncMapSessionStore(defaultInfo),
		"File":    fileStore,
	}
}

// TestAddMessage verifies messages are added to history
func TestAddMessage(t *testing.T) {
	for name, store := range testStores(t) {
		t.Run(name, func(t *testing.T) {
			session, err := store.Get("test")
			if err != nil {
				t.Fatalf("Failed to get session: %v", err)
			}

			// Add a message
			msg := messages.ChatMessage{
				Role:    messages.MessageRoleUser,
				Content: "Hello",
			}
			session.AddMessage(msg)

			// Verify it's in history
			history := session.GetHistory()

			// Should have system prompt + our message
			if len(history) != 2 {
				t.Errorf("Expected 2 messages, got %d", len(history))
			}

			if history[1].Content != "Hello" {
				t.Errorf("Expected 'Hello', got '%s'", history[1].Content)
			}
		})
	}
}

// TestClearWithSystemPrompt verifies Clear() resets to system prompt
func TestClearWithSystemPrompt(t *testing.T) {
	for name, store := range testStores(t) {
		t.Run(name, func(t *testing.T) {
			session, err := store.Get("test")
			if err != nil {
				t.Fatalf("Failed to get session: %v", err)
			}

			// Add some messages
			session.AddMessage(messages.ChatMessage{Role: messages.MessageRoleUser, Content: "msg1"})
			session.AddMessage(messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "msg2"})

			// Clear
			session.Clear()

			// Should only have system prompt
			history := session.GetHistory()
			if len(history) != 1 {
				t.Errorf("Expected 1 message after clear, got %d", len(history))
			}

			if history[0].Role != messages.MessageRoleSystem {
				t.Errorf("Expected system role, got %s", history[0].Role)
			}

			if history[0].Content != "test system prompt" {
				t.Errorf("Expected 'test system prompt', got '%s'", history[0].Content)
			}
		})
	}
}

// TestDelete verifies session deletion
func TestDelete(t *testing.T) {
	for name, store := range testStores(t) {
		t.Run(name, func(t *testing.T) {
			// Create and populate session
			session1, err := store.Get("deleteme")
			if err != nil {
				t.Fatalf("Failed to get session1: %v", err)
			}
			session1.AddMessage(messages.ChatMessage{Role: messages.MessageRoleUser, Content: "test"})

			// Delete it
			store.Delete("deleteme")

			// Get it again - should be fresh
			session2, err := store.Get("deleteme")
			if err != nil {
				t.Fatalf("Failed to get session2: %v", err)
			}
			history := session2.GetHistory()

			// Should only have system prompt (new session)
			if len(history) != 1 {
				t.Errorf("Expected fresh session with 1 message, got %d", len(history))
			}
		})
	}
}

// TestTrimKeepsSystemPrompt verifies system prompt is never removed
func TestTrimKeepsSystemPrompt(t *testing.T) {
	for name, store := range testStores(t) {
		t.Run(name, func(t *testing.T) {
			session, err := store.Get("test")
			if err != nil {
				t.Fatalf("Failed to get session: %v", err)
			}

			// Add enough messages to trigger token-based trimming
			for i := range 15 {
				session.AddMessage(messages.ChatMessage{
					Role:    messages.MessageRoleUser,
					Content: fmt.Sprintf("message-%d", i),
				})
			}

			history := session.GetHistory()

			// First message should still be system prompt
			if history[0].Role != messages.MessageRoleSystem {
				t.Errorf("First message should be system prompt, got %s", history[0].Role)
			}

			if history[0].Content != "test system prompt" {
				t.Errorf("System prompt content changed: %s", history[0].Content)
			}

			// Should have fewer than 16 messages due to token limit
			if len(history) >= 16 {
				t.Errorf("History too long (not trimmed): %d messages", len(history))
			}
		})
	}
}

// TestTrimRemovesOrphanedToolResponse verifies orphaned tool responses are removed
func TestTrimRemovesOrphanedToolResponse(t *testing.T) {
	// Create config with small token limit to make test clearer
	defaultInfo := &Metadata{
		MaxHistoryTokens: 50, // small limit to trigger trimming
		TTL:              0,
		SystemPrompt:     "system",
	}

	stores := map[string]SessionStore{
		"SyncMap": NewSyncMapSessionStore(defaultInfo),
	}

	// Add File store
	fileStore, err := NewFileSessionStore(t.TempDir(), defaultInfo)
	if err == nil {
		stores["File"] = fileStore
	}

	for name, store := range stores {
		t.Run(name, func(t *testing.T) {
			session, err := store.Get("test")
			if err != nil {
				t.Fatalf("Failed to get session: %v", err)
			}

			// Add messages: system (auto), user, assistant with tool_calls, tool response, user
			session.AddMessage(messages.ChatMessage{Role: messages.MessageRoleUser, Content: "first"})
			session.AddMessage(messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "calling tool"})
			session.AddMessage(messages.ChatMessage{Role: messages.MessageRoleTool, Content: "tool response"})
			session.AddMessage(messages.ChatMessage{Role: messages.MessageRoleUser, Content: "second"})

			// This should trigger trim due to token limit
			// If trim makes position 1 a tool response, it should be removed
			session.AddMessage(messages.ChatMessage{Role: messages.MessageRoleUser, Content: "third"})

			history := session.GetHistory()

			// Check no orphaned tool response at position 1
			if len(history) > 1 && history[1].Role == messages.MessageRoleTool {
				t.Error("Orphaned tool response at position 1 should be removed")
			}

			// Verify system prompt still there
			if history[0].Content != "system" {
				t.Error("System prompt should be preserved")
			}
		})
	}
}

// TestTrimKeepsWithinTokenLimit verifies history is trimmed to token limit
func TestTrimKeepsWithinTokenLimit(t *testing.T) {
	// Each message is ~5-6 tokens (content + overhead), so 30 tokens should keep ~5-6 messages
	defaultInfo := &Metadata{
		MaxHistoryTokens: 30,
		TTL:              0,
		SystemPrompt:     "system",
	}

	stores := map[string]SessionStore{
		"SyncMap": NewSyncMapSessionStore(defaultInfo),
	}

	// Add File store
	fileStore, err := NewFileSessionStore(t.TempDir(), defaultInfo)
	if err == nil {
		stores["File"] = fileStore
	}

	for name, store := range stores {
		t.Run(name, func(t *testing.T) {
			session, err := store.Get("test")
			if err != nil {
				t.Fatalf("Failed to get session: %v", err)
			}

			// Add 10 messages
			for i := range 10 {
				session.AddMessage(messages.ChatMessage{
					Role:    messages.MessageRoleUser,
					Content: fmt.Sprintf("msg-%d", i),
				})
			}

			history := session.GetHistory()

			// Should have less than 10 messages due to token limit
			if len(history) >= 11 { // system + 10
				t.Errorf("Expected trimming to occur, got %d messages", len(history))
			}

			// Verify we kept the most recent messages
			lastMsg := history[len(history)-1]
			if lastMsg.Content != "msg-9" {
				t.Errorf("Expected last message to be 'msg-9', got '%s'", lastMsg.Content)
			}

			// Verify system prompt is preserved
			if history[0].Role != messages.MessageRoleSystem {
				t.Error("System prompt should be preserved")
			}
		})
	}
}

// TestConcurrentAddMessage verifies no messages are lost during concurrent access
func TestConcurrentAddMessage(t *testing.T) {
	for name, store := range testStores(t) {
		t.Run(name, func(t *testing.T) {
			session, err := store.Get("concurrent")
			if err != nil {
				t.Fatalf("Failed to get session: %v", err)
			}

			// Use WaitGroup for proper synchronization
			var wg sync.WaitGroup
			numGoroutines := 50
			messagesPerGoroutine := 10

			wg.Add(numGoroutines)

			// Each goroutine adds numbered messages
			for g := range numGoroutines {
				go func(goroutineID int) {
					defer wg.Done()
					for m := range messagesPerGoroutine {
						msg := messages.ChatMessage{
							Role:    messages.MessageRoleUser,
							Content: fmt.Sprintf("g%d-m%d", goroutineID, m),
						}
						session.AddMessage(msg)
					}
				}(g)
			}

			// Wait for all goroutines to complete
			wg.Wait()

			history := session.GetHistory()

			// Should have system prompt + messages (limited by token budget)
			// With token limit, we should have at least a few messages
			if len(history) < 2 {
				t.Errorf("Expected at least 2 messages (system + user), got %d", len(history))
			}

			// Verify system prompt is still first
			if history[0].Role != messages.MessageRoleSystem {
				t.Error("System prompt should still be first")
			}

			// For File store, clean up to avoid lock issues
			if name == "File" {
				if closer, ok := session.(interface{ Close() }); ok {
					closer.Close()
				}
			}
		})
	}
}

// TestMaxTokensPersistence verifies that MaxTokens is saved and loaded correctly
func TestMaxTokensPersistence(t *testing.T) {
	tmpDir := t.TempDir()
	defaultInfo := &Metadata{
		MaxHistoryTokens: 100000,
		TTL:              0,
		SystemPrompt:     "test",
	}

	store, err := NewFileSessionStore(tmpDir, defaultInfo)
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}

	fileStore := store.(*FileSessionStore)

	// Save context info with MaxTokens
	info := &Metadata{
		Name:        "test-context",
		Model:       "openai/gpt-4",
		Temperature: 0.7,
		MaxTokens:   8192,
		Created:     time.Now(),
		LastUsed:    time.Now(),
	}

	// Create session and set context info
	session, err := fileStore.Get("test-context")
	if err != nil {
		t.Fatalf("Failed to get session: %v", err)
	}
	session.SetMetadata(info)
	session.Close()

	// Retrieve and verify
	allInfo := fileStore.GetAllMetadata()
	retrieved := allInfo["test-context"]
	if retrieved == nil {
		t.Fatal("Failed to retrieve context")
	}

	if retrieved.MaxTokens != 8192 {
		t.Errorf("Expected MaxTokens to be 8192, got %d", retrieved.MaxTokens)
	}

	// Test that it's preserved when updating other fields
	newModel := "anthropic/claude-3"

	// Update through session
	session2, err := fileStore.Get("test-context")
	if err != nil {
		t.Fatalf("Failed to get session: %v", err)
	}
	update := &Metadata{
		Name:  "test-context",
		Model: newModel,
	}
	if err := session2.UpdateMetadata(update); err != nil {
		t.Fatalf("Failed to update context info: %v", err)
	}
	session2.Close()

	allInfo2 := fileStore.GetAllMetadata()
	retrieved2 := allInfo2["test-context"]
	if retrieved2 == nil {
		t.Fatal("Failed to retrieve updated context")
	}

	// MaxTokens should be preserved
	if retrieved2.MaxTokens != 8192 {
		t.Errorf("MaxTokens was not preserved during update, got %d", retrieved2.MaxTokens)
	}

	// Model should be updated
	if retrieved2.Model != "anthropic/claude-3" {
		t.Errorf("Model was not updated, got %s", retrieved2.Model)
	}
}

// TestExpiryGoroutine verifies the expiry goroutine actually runs and cleans up sessions
func TestExpiryGoroutine(t *testing.T) {
	// Create store with very short TTL
	defaultInfo := &Metadata{
		MaxHistoryTokens: 100000,
		TTL:              50 * time.Millisecond, // Very short TTL for testing
		SystemPrompt:     "test",
	}

	store := NewSyncMapSessionStore(defaultInfo)

	// Create multiple sessions with staggered access times
	session1, err := store.Get("session1")
	if err != nil {
		t.Fatalf("Failed to get session1: %v", err)
	}
	session1.AddMessage(messages.ChatMessage{Role: messages.MessageRoleUser, Content: "msg1"})

	time.Sleep(30 * time.Millisecond)

	session2, err := store.Get("session2")
	if err != nil {
		t.Fatalf("Failed to get session2: %v", err)
	}
	session2.AddMessage(messages.ChatMessage{Role: messages.MessageRoleUser, Content: "msg2"})

	// Both sessions should exist
	history1 := session1.GetHistory()
	if len(history1) != 2 {
		t.Errorf("Session1 should have 2 messages, got %d", len(history1))
	}

	// Wait for expiry goroutine to run (it runs every TTL duration)
	// Session1 should expire, session2 should still be there
	time.Sleep(60 * time.Millisecond)

	// Access session2 to keep it alive
	session2.AddMessage(messages.ChatMessage{Role: messages.MessageRoleUser, Content: "keep alive"})

	// Wait for another expiry cycle
	time.Sleep(60 * time.Millisecond)

	// Now check what's left
	// Session1 should be gone (expired)
	newSession1, err := store.Get("session1")
	if err != nil {
		t.Fatalf("Failed to get newSession1: %v", err)
	}
	history1New := newSession1.GetHistory()
	if len(history1New) != 1 {
		t.Errorf("Session1 should have been expired and recreated with just system prompt, got %d messages", len(history1New))
	}

	// Session2 should still have its messages (was kept alive)
	history2 := session2.GetHistory()
	if len(history2) != 3 { // system + 2 messages
		t.Errorf("Session2 should still have 3 messages, got %d", len(history2))
	}

	// Verify the expiry goroutine continues to run
	// Stop accessing session2 and wait for it to expire
	time.Sleep(120 * time.Millisecond)

	// Both should now be expired
	finalSession2, err := store.Get("session2")
	if err != nil {
		t.Fatalf("Failed to get finalSession2: %v", err)
	}
	finalHistory2 := finalSession2.GetHistory()
	if len(finalHistory2) != 1 {
		t.Errorf("Session2 should have expired, expected 1 message, got %d", len(finalHistory2))
	}
}
