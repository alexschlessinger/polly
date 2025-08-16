package main

import (
	"fmt"
	"os"
	"time"

	"github.com/pkdindustries/pollytool/messages"
	"github.com/pkdindustries/pollytool/sessions"
)

// setupSessionStore creates the appropriate session store based on configuration
func setupSessionStore(config *Config, contextID string) (sessions.SessionStore, error) {
	if needsFileStore(config, contextID) {
		return sessions.NewFileSessionStore("") // Uses default ~/.pollytool/contexts
	}
	return sessions.NewSessionStore(memoryStoreTTL), nil
}

// handleListContexts lists all available contexts
func handleListContexts(store sessions.SessionStore) error {
	if fileStore, ok := store.(*sessions.FileSessionStore); ok {
		// Get named contexts
		namedContexts := fileStore.GetContextInfo()
		lastContext := fileStore.GetLastContext()

		// Get all context IDs
		contextIDs, err := fileStore.ListContexts()
		if err != nil {
			return fmt.Errorf("failed to list contexts: %w", err)
		}

		// Print named contexts first
		for name, info := range namedContexts {
			marker := ""
			if info.Name == lastContext {
				marker = " *"
			}
			timeSince := time.Since(info.LastUsed)
			timeStr := formatDuration(timeSince)

			// Build model info string
			modelInfo := ""
			if info.Model != "" {
				modelInfo = fmt.Sprintf(" [%s]", info.Model)
			}

			fmt.Printf("%s%s - last used: %s%s\n", name, modelInfo, timeStr, marker)
		}

		// Print contexts without metadata in the index
		for _, name := range contextIDs {
			// Skip if already shown (has metadata in index)
			if _, hasMetadata := namedContexts[name]; hasMetadata {
				continue
			}
			marker := ""
			if name == lastContext {
				marker = " *"
			}
			shortName := name
			if len(shortName) > 20 {
				shortName = shortName[:8] + "..."
			}
			fmt.Printf("%s%s\n", shortName, marker)
		}

		if len(contextIDs) == 0 && len(namedContexts) == 0 {
			fmt.Println("No contexts found")
		}
	} else {
		fmt.Println("No persistent contexts (using memory store)")
	}
	return nil
}

// formatDuration formats a duration in a human-readable way
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return "just now"
	} else if d < time.Hour {
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	} else if d < 24*time.Hour {
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	} else {
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
}

// handleDeleteContext deletes the specified context
func handleDeleteContext(store sessions.SessionStore, contextID string) error {
	// Check if it's a file store - only file-based contexts can be deleted
	fileStore, ok := store.(*sessions.FileSessionStore)
	if !ok {
		return fmt.Errorf("cannot delete context: using memory store")
	}

	// Check if context exists
	if !fileStore.ContextExists(contextID) {
		return fmt.Errorf("context '%s' not found", contextID)
	}

	// Delete the context
	fileStore.Delete(contextID)
	fmt.Printf("Context '%s' deleted\n", contextID)
	return nil
}

// handleAddToContext adds stdin content to a context without making an API call
func handleAddToContext(store sessions.SessionStore, config *Config, contextID string) error {
	if contextID == "" {
		// Try to use last context if available
		if fileStore, ok := store.(*sessions.FileSessionStore); ok {
			lastContext := fileStore.GetLastContext()
			if lastContext != "" {
				contextDisplay := lastContext
				if info := fileStore.GetContextByNameOrID(lastContext); info != nil && info.Name != "" {
					contextDisplay = info.Name
				}
				prompt := fmt.Sprintf("No context specified. Use last context '%s'?", contextDisplay)
				if promptYesNo(prompt) {
					contextID = lastContext
				} else {
					return fmt.Errorf("--add requires a context ID (use --context or POLLYTOOL_CONTEXT)")
				}
			} else {
				return fmt.Errorf("--add requires a context ID (use --context or POLLYTOOL_CONTEXT)")
			}
		} else {
			return fmt.Errorf("--add requires a context ID (use --context or POLLYTOOL_CONTEXT)")
		}
	}

	if !hasStdinData() {
		return fmt.Errorf("--add requires input from stdin")
	}

	content, err := readFromStdin()
	if err != nil {
		return err
	}

	session := store.Get(contextID)
	defer closeFileSession(session)

	session.AddMessage(messages.ChatMessage{
		Role:    messages.MessageRoleUser,
		Content: content,
	})

	if !config.Quiet {
		fmt.Fprintf(os.Stderr, "Added to context %s\n", contextID)
	}
	return nil
}

// getOrCreateSession gets an existing session or creates a new one
func getOrCreateSession(store sessions.SessionStore, contextID string, needFileStore bool) sessions.Session {
	if contextID == "" && !needFileStore {
		contextID = "default" // Memory store context
	}
	return store.Get(contextID)
}
