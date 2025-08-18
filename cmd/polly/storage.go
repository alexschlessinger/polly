package main

import (
	"fmt"
	"os"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

// setupSessionStore creates the appropriate session store based on configuration
func setupSessionStore(config *Config, contextID string) (sessions.SessionStore, error) {
	sessionConfig := &sessions.SessionConfig{
		TTL:          memoryStoreTTL,
		SystemPrompt: config.SystemPrompt,
		MaxHistory:   0, // Unlimited for polly CLI
	}
	
	if needsFileStore(config, contextID) {
		return sessions.NewFileSessionStore("", sessionConfig) // Uses default ~/.pollytool/contexts
	}
	return sessions.NewSyncMapSessionStore(sessionConfig), nil
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

// handleAddToContext adds stdin content or file content to a context without making an API call
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

	session := store.Get(contextID)
	defer closeFileSession(session)

	// Check if files are provided via --file flag
	if len(config.Files) > 0 {
		// Process files to get their content
		parts, err := processFiles(config.Files)
		if err != nil {
			return fmt.Errorf("error processing files: %w", err)
		}

		// Check if stdin data is also provided
		if hasStdinData() {
			content, err := readFromStdin()
			if err != nil {
				return err
			}
			// Add stdin content as a separate message
			session.AddMessage(messages.ChatMessage{
				Role:    messages.MessageRoleUser,
				Content: content,
			})
		}

		// Add each file as a separate message
		for _, part := range parts {
			switch part.Type {
			case "text":
				// Create a message for each file
				var content string
				if part.FileName != "" {
					content = fmt.Sprintf("=== %s ===\n%s", part.FileName, part.Text)
				} else {
					content = part.Text
				}
				session.AddMessage(messages.ChatMessage{
					Role:    messages.MessageRoleUser,
					Content: content,
				})
			case "image_base64":
				// Create a message with image content using Parts field
				msg := messages.ChatMessage{
					Role: messages.MessageRoleUser,
					Parts: []messages.ContentPart{part},
				}
				session.AddMessage(msg)
			}
		}
	} else {
		// Original behavior: require stdin when no files
		if !hasStdinData() {
			return fmt.Errorf("--add requires input from stdin or files via --file")
		}

		content, err := readFromStdin()
		if err != nil {
			return err
		}

		session.AddMessage(messages.ChatMessage{
			Role:    messages.MessageRoleUser,
			Content: content,
		})
	}

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
