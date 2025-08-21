package main

import (
	"fmt"
	"os"
	"path/filepath"
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

	// Prompt for confirmation (default to no for destructive operation)
	prompt := fmt.Sprintf("Delete context '%s' permanently?", contextID)
	if !promptYesNo(prompt, false) {
		fmt.Println("Delete cancelled")
		return nil
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
				if promptYesNo(prompt, true) {
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
	if session == nil {
		return fmt.Errorf("failed to acquire session lock for context %s (may be in use by another process)", contextID)
	}
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
	session := store.Get(contextID)
	if session == nil {
		// Exit cleanly with error message instead of panic
		fmt.Fprintf(os.Stderr, "Error: Failed to acquire session lock for context '%s'\n", contextID)
		fmt.Fprintf(os.Stderr, "The context may be in use by another polly process.\n")
		fmt.Fprintf(os.Stderr, "Please wait for the other process to complete or use a different context.\n")
		cleanupAndExit(1)
	}
	// Track active file session for cleanup
	setActiveFileSession(session)
	return session
}

// handleCreateContext creates a new context with the specified configuration
func handleCreateContext(store sessions.SessionStore, config *Config, contextID string) error {
	if contextID == "" {
		return fmt.Errorf("--create requires a context name (use -c or POLLYTOOL_CONTEXT)")
	}

	fileStore, ok := store.(*sessions.FileSessionStore)
	if !ok {
		return fmt.Errorf("--create requires file-based storage")
	}

	// Check if context already exists
	if fileStore.ContextExists(contextID) {
		return fmt.Errorf("context '%s' already exists", contextID)
	}

	// Create context info with all settings
	info := &sessions.ContextInfo{
		Name:         contextID,
		Model:        config.Model,
		Temperature:  config.Temperature,
		MaxTokens:    config.MaxTokens,
		SystemPrompt: config.SystemPrompt,
		ToolPaths:    config.ToolPaths,
		MCPServers:   config.MCPServers,
		Created:      time.Now(),
		LastUsed:     time.Now(),
	}

	// Save context info
	if err := fileStore.SaveContextInfo(info); err != nil {
		return fmt.Errorf("failed to save context: %w", err)
	}

	fmt.Printf("Created context '%s' with:\n", contextID)
	fmt.Printf("  Model: %s\n", info.Model)
	fmt.Printf("  Temperature: %.2f\n", info.Temperature)
	fmt.Printf("  Max Tokens: %d\n", info.MaxTokens)
	if info.SystemPrompt != "" && info.SystemPrompt != defaultSystemPrompt {
		// Only show if different from default
		fmt.Printf("  System Prompt: %s\n", info.SystemPrompt)
	}
	if len(info.ToolPaths) > 0 {
		fmt.Printf("  Tools: %v\n", info.ToolPaths)
	}
	if len(info.MCPServers) > 0 {
		fmt.Printf("  MCP Servers: %v\n", info.MCPServers)
	}

	return nil
}

// handleShowContext shows the configuration for a context
func handleShowContext(store sessions.SessionStore, contextID string) error {
	if contextID == "" {
		return fmt.Errorf("--show requires a context name")
	}

	fileStore, ok := store.(*sessions.FileSessionStore)
	if !ok {
		return fmt.Errorf("--show requires file-based storage")
	}

	info := fileStore.GetContextByNameOrID(contextID)
	if info == nil {
		// Check if context exists but has no metadata
		if fileStore.ContextExists(contextID) {
			fmt.Printf("Context: %s\n", contextID)
			fmt.Printf("  (no configuration metadata)\n")
			return nil
		}
		return fmt.Errorf("context '%s' not found", contextID)
	}

	// Display detailed configuration
	fmt.Printf("Context: %s\n", info.Name)
	fmt.Printf("  Model: %s\n", info.Model)
	fmt.Printf("  Temperature: %.2f\n", info.Temperature)
	fmt.Printf("  Max Tokens: %d\n", info.MaxTokens)
	
	if info.SystemPrompt != "" {
		fmt.Printf("  System Prompt: %s\n", info.SystemPrompt)
	}
	
	if len(info.ToolPaths) > 0 {
		fmt.Printf("  Tools: %v\n", info.ToolPaths)
	}
	
	if len(info.MCPServers) > 0 {
		fmt.Printf("  MCP Servers: %v\n", info.MCPServers)
	}
	
	fmt.Printf("  Created: %s\n", info.Created.Format("2006-01-02 15:04:05"))
	fmt.Printf("  Last Used: %s (%s ago)\n", 
		info.LastUsed.Format("2006-01-02 15:04:05"),
		formatDuration(time.Since(info.LastUsed)))

	return nil
}

// handleResetContext resets a context (clears conversation, keeps settings)
func handleResetContext(store sessions.SessionStore, config *Config, contextID string) error {
	if contextID == "" {
		return fmt.Errorf("--reset requires a context name")
	}

	fileStore, ok := store.(*sessions.FileSessionStore)
	if !ok {
		return fmt.Errorf("--reset requires file-based storage")
	}

	// Check if context exists
	if !fileStore.ContextExists(contextID) {
		return fmt.Errorf("context '%s' does not exist", contextID)
	}

	// Prompt for confirmation (default to no for destructive operation)
	prompt := fmt.Sprintf("Reset context '%s' (clear conversation history)?", contextID)
	if !promptYesNo(prompt, false) {
		fmt.Println("Reset cancelled")
		return nil
	}

	// Get existing context info to preserve settings
	existingInfo := fileStore.GetContextByNameOrID(contextID)
	if existingInfo != nil {
		// Update settings with any command-line overrides
		existingInfo.LastUsed = time.Now()
		if config.Model != defaultModel {
			existingInfo.Model = config.Model
		}
		if config.Temperature != defaultTemperature {
			existingInfo.Temperature = config.Temperature
		}
		if config.MaxTokens != defaultMaxTokens {
			existingInfo.MaxTokens = config.MaxTokens
		}
		if config.SystemPromptWasSet {
			existingInfo.SystemPrompt = config.SystemPrompt
		}
		if len(config.ToolPaths) > 0 {
			existingInfo.ToolPaths = config.ToolPaths
		}
		if len(config.MCPServers) > 0 {
			existingInfo.MCPServers = config.MCPServers
		}
		if err := fileStore.SaveContextInfo(existingInfo); err != nil {
			return fmt.Errorf("failed to save context info: %w", err)
		}
	}

	// Clear the conversation file
	sessionPath := filepath.Join(fileStore.GetBaseDir(), contextID+".json")
	if err := os.Remove(sessionPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to reset context: %w", err)
	}

	fmt.Printf("Reset context '%s' (cleared conversation, kept settings)\n", contextID)
	return nil
}

// handlePurgeAll deletes all sessions and the index
func handlePurgeAll(store sessions.SessionStore) error {
	// Check if it's a file store - only file-based contexts can be purged
	fileStore, ok := store.(*sessions.FileSessionStore)
	if !ok {
		return fmt.Errorf("cannot purge: using memory store (no persistent data to delete)")
	}

	// Get count of contexts for the confirmation message
	contextIDs, err := fileStore.ListContexts()
	if err != nil {
		return fmt.Errorf("failed to list contexts: %w", err)
	}

	if len(contextIDs) == 0 {
		fmt.Println("No contexts to purge")
		return nil
	}

	// Prompt for confirmation (default to no for destructive operation)
	prompt := fmt.Sprintf("This will permanently delete %d context(s) and all associated data. Are you sure?", len(contextIDs))
	if !promptYesNo(prompt, false) {
		fmt.Println("Purge cancelled")
		return nil
	}

	// Delete all contexts
	deletedCount := 0
	for _, contextID := range contextIDs {
		fileStore.Delete(contextID)
		deletedCount++
	}

	// Clear the index
	if err := fileStore.ClearIndex(); err != nil {
		return fmt.Errorf("failed to clear index: %w", err)
	}

	// Also clean up any lingering index lock file
	homeDir, _ := os.UserHomeDir()
	indexLockPath := filepath.Join(homeDir, ".pollytool", "index.json.lock")
	os.Remove(indexLockPath)

	fmt.Printf("Purged %d context(s) and cleared the index\n", deletedCount)
	return nil
}
