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
	config := &SessionConfig{
		MaxHistory:   10,
		TTL:          0, // No expiry for tests
		SystemPrompt: "test system prompt",
	}

	// Create file store in temp directory
	fileStore, err := NewFileSessionStore(t.TempDir(), config)
	if err != nil {
		t.Fatalf("Failed to create file store: %v", err)
	}

	return map[string]SessionStore{
		"SyncMap": NewSyncMapSessionStore(config),
		"File":    fileStore,
	}
}

// TestAddMessage verifies messages are added to history
func TestAddMessage(t *testing.T) {
	for name, store := range testStores(t) {
		t.Run(name, func(t *testing.T) {
			session := store.Get("test")

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
			session := store.Get("test")

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
			session1 := store.Get("deleteme")
			session1.AddMessage(messages.ChatMessage{Role: messages.MessageRoleUser, Content: "test"})

			// Delete it
			store.Delete("deleteme")

			// Get it again - should be fresh
			session2 := store.Get("deleteme")
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
			session := store.Get("test")

			// Add more than MaxHistory messages
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

			// Should have at most MaxHistory + 1 (system prompt)
			if len(history) > 11 { // 10 + system prompt
				t.Errorf("History too long: %d messages", len(history))
			}
		})
	}
}

// TestTrimRemovesOrphanedToolResponse verifies orphaned tool responses are removed
func TestTrimRemovesOrphanedToolResponse(t *testing.T) {
	// Create config with small MaxHistory to make test clearer
	config := &SessionConfig{
		MaxHistory:   3,
		TTL:          0,
		SystemPrompt: "system",
	}

	stores := map[string]SessionStore{
		"SyncMap": NewSyncMapSessionStore(config),
	}

	// Add File store
	fileStore, err := NewFileSessionStore(t.TempDir(), config)
	if err == nil {
		stores["File"] = fileStore
	}

	for name, store := range stores {
		t.Run(name, func(t *testing.T) {
			session := store.Get("test")

			// Add messages: system (auto), user, assistant with tool_calls, tool response, user
			session.AddMessage(messages.ChatMessage{Role: messages.MessageRoleUser, Content: "first"})
			session.AddMessage(messages.ChatMessage{Role: messages.MessageRoleAssistant, Content: "calling tool"})
			session.AddMessage(messages.ChatMessage{Role: messages.MessageRoleTool, Content: "tool response"})
			session.AddMessage(messages.ChatMessage{Role: messages.MessageRoleUser, Content: "second"})

			// This should trigger trim - with MaxHistory=3, we keep system + last 2
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

// TestTrimKeepsMaxHistory verifies only MaxHistory messages are kept
func TestTrimKeepsMaxHistory(t *testing.T) {
	config := &SessionConfig{
		MaxHistory:   5,
		TTL:          0,
		SystemPrompt: "system",
	}

	stores := map[string]SessionStore{
		"SyncMap": NewSyncMapSessionStore(config),
	}

	// Add File store
	fileStore, err := NewFileSessionStore(t.TempDir(), config)
	if err == nil {
		stores["File"] = fileStore
	}

	for name, store := range stores {
		t.Run(name, func(t *testing.T) {
			session := store.Get("test")

			// Add 10 messages
			for i := range 10 {
				session.AddMessage(messages.ChatMessage{
					Role:    messages.MessageRoleUser,
					Content: fmt.Sprintf("msg-%d", i),
				})
			}

			history := session.GetHistory()

			// Should have system + MaxHistory messages
			expectedLen := 6 // system + 5
			if len(history) != expectedLen {
				t.Errorf("Expected %d messages, got %d", expectedLen, len(history))
			}

			// Verify we kept the most recent messages
			lastMsg := history[len(history)-1]
			if lastMsg.Content != "msg-9" {
				t.Errorf("Expected last message to be 'msg-9', got '%s'", lastMsg.Content)
			}
		})
	}
}

// TestConcurrentAddMessage verifies no messages are lost during concurrent access
func TestConcurrentAddMessage(t *testing.T) {
	for name, store := range testStores(t) {
		t.Run(name, func(t *testing.T) {
			session := store.Get("concurrent")

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

			// Should have system prompt + all messages (may be trimmed if > MaxHistory)
			// But we should have at least MaxHistory messages
			minExpected := 11 // MaxHistory (10) + system prompt
			if len(history) < minExpected {
				t.Errorf("Expected at least %d messages, got %d", minExpected, len(history))
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

// TestExpiryGoroutine verifies the expiry goroutine actually runs and cleans up sessions
func TestExpiryGoroutine(t *testing.T) {
	// Create store with very short TTL
	config := &SessionConfig{
		MaxHistory:   10,
		TTL:          50 * time.Millisecond, // Very short TTL for testing
		SystemPrompt: "test",
	}

	store := NewSyncMapSessionStore(config)

	// Create multiple sessions with staggered access times
	session1 := store.Get("session1")
	session1.AddMessage(messages.ChatMessage{Role: messages.MessageRoleUser, Content: "msg1"})

	time.Sleep(30 * time.Millisecond)

	session2 := store.Get("session2")
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
	newSession1 := store.Get("session1")
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
	finalSession2 := store.Get("session2")
	finalHistory2 := finalSession2.GetHistory()
	if len(finalHistory2) != 1 {
		t.Errorf("Session2 should have expired, expected 1 message, got %d", len(finalHistory2))
	}
}
