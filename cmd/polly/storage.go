package main

import (
	"fmt"
	"os"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
)

// needsFileStore determines if we need a file-based session store
func needsFileStore(config *Config, contextID string) bool {
	return contextID != "" ||
		config.ResetContext != "" ||
		config.UseLastContext ||
		config.ListContexts ||
		config.DeleteContext != "" ||
		config.AddToContext ||
		config.PurgeAll ||
		config.CreateContext != "" ||
		config.ShowContext != ""
}

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
	contexts := store.GetAllContextInfo()
	lastContext := store.GetLastContext()

	if len(contexts) == 0 {
		fmt.Println("No contexts found")
		return nil
	}

	// Print all contexts with their metadata
	for name, info := range contexts {
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
	// Check if context exists
	if !store.Exists(contextID) {
		return fmt.Errorf("context '%s' not found", contextID)
	}

	// Prompt for confirmation (default to no for destructive operation)
	if !confirmDeletion(contextID) {
		return nil
	}

	return deleteContext(store, contextID)
}

// confirmDeletion prompts the user to confirm deletion
func confirmDeletion(contextID string) bool {
	prompt := fmt.Sprintf("Delete context '%s' permanently?", contextID)
	if !promptYesNo(prompt, false) {
		fmt.Println("Delete cancelled")
		return false
	}
	return true
}

// deleteContext performs the actual deletion
func deleteContext(store sessions.SessionStore, contextID string) error {
	store.Delete(contextID)

	// Reflect actual result: if still exists, it was likely in use and skipped
	if store.Exists(contextID) {
		fmt.Fprintf(os.Stderr, "Context '%s' is currently in use; deletion skipped\n", contextID)
		return nil
	}

	fmt.Printf("Context '%s' deleted\n", contextID)
	return nil
}

// handleAddToContext adds stdin content or file content to a context without making an API call
func handleAddToContext(store sessions.SessionStore, config *Config, contextID string) error {
	if contextID == "" {
		// Try to use last context if available
		lastContext := store.GetLastContext()
		if lastContext != "" {
			contextDisplay := lastContext
			if info := store.GetAllContextInfo()[lastContext]; info != nil && info.Name != "" {
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
	}

	session, err := store.Get(contextID)
	if err != nil {
		return fmt.Errorf("failed to get session for context %s: %w", contextID, err)
	}
	defer session.Close()

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
					Role:  messages.MessageRoleUser,
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
	session, err := store.Get(contextID)
	if err != nil {
		// Exit cleanly with error message instead of panic
		fmt.Fprintf(os.Stderr, "Error: Failed to get session for context '%s': %v\n", contextID, err)
		cleanupAndExit(1)
	}
	return session
}

// handleCreateContext creates a new context with the specified configuration
func handleCreateContext(store sessions.SessionStore, config *Config, contextID string) error {
	if contextID == "" {
		return fmt.Errorf("--create requires a context name (use -c or POLLYTOOL_CONTEXT)")
	}

	// Check if context already exists
	if store.Exists(contextID) {
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

	// Create session and set its context info
	session, err := store.Get(contextID)
	if err != nil {
		return fmt.Errorf("failed to create context: %w", err)
	}
	defer session.Close()
	
	session.SetContextInfo(info)

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

	info := store.GetAllContextInfo()[contextID]
	if info == nil {
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
	fmt.Printf("  Last Used: %s (%s)\n",
		info.LastUsed.Format("2006-01-02 15:04:05"),
		formatDuration(time.Since(info.LastUsed)))

	return nil
}

// handleResetContext resets a context (clears conversation, keeps settings)
func handleResetContext(store sessions.SessionStore, config *Config, contextID string) error {
	if contextID == "" {
		return fmt.Errorf("--reset requires a context name")
	}

	// Check if context exists
	if !store.Exists(contextID) {
		return fmt.Errorf("context '%s' does not exist", contextID)
	}

	// Prompt for confirmation
	if !confirmReset(contextID) {
		return nil
	}

	// Reset the context using the resetContext helper
	if err := resetContext(store, contextID); err != nil {
		return fmt.Errorf("failed to reset context: %w", err)
	}
	
	// If there are command-line overrides, apply them through the session
	if config.Model != defaultModel || config.Temperature != defaultTemperature ||
		config.MaxTokens != defaultMaxTokens || config.SystemPrompt != defaultSystemPrompt ||
		len(config.ToolPaths) > 0 || len(config.MCPServers) > 0 {
		
		session, err := store.Get(contextID)
		if err != nil {
			return fmt.Errorf("failed to get session: %w", err)
		}
		defer session.Close()
		
		// Build update with overrides
		update := &sessions.ContextUpdate{Name: contextID}
		now := time.Now()
		update.LastUsed = &now
		
		if config.Model != defaultModel {
			update.Model = &config.Model
		}
		if config.Temperature != defaultTemperature {
			update.Temperature = &config.Temperature
		}
		if config.MaxTokens != defaultMaxTokens {
			update.MaxTokens = &config.MaxTokens
		}
		if config.SystemPrompt != defaultSystemPrompt {
			update.SystemPrompt = &config.SystemPrompt
		}
		if len(config.ToolPaths) > 0 {
			update.ToolPaths = &config.ToolPaths
		}
		if len(config.MCPServers) > 0 {
			update.MCPServers = &config.MCPServers
		}
		
		if err := session.UpdateContextInfo(update); err != nil {
			return fmt.Errorf("failed to update context info: %w", err)
		}
	}

	fmt.Printf("Reset context '%s' (cleared conversation, kept settings)\n", contextID)
	return nil
}

// confirmReset prompts the user to confirm reset
func confirmReset(contextID string) bool {
	prompt := fmt.Sprintf("Reset context '%s' (clear conversation history)?", contextID)
	if !promptYesNo(prompt, false) {
		fmt.Println("Reset cancelled")
		return false
	}
	return true
}

// handlePurgeAll deletes all sessions and the index
func handlePurgeAll(store sessions.SessionStore) error {
	// Get count of contexts for the confirmation message
	contextIDs, err := store.List()
	if err != nil {
		return fmt.Errorf("failed to list contexts: %w", err)
	}

	if len(contextIDs) == 0 {
		fmt.Println("No contexts to purge")
		return nil
	}

	// Prompt for confirmation
	if !confirmPurge(len(contextIDs)) {
		return nil
	}

	return purgeContexts(store, contextIDs)
}

// confirmPurge prompts the user to confirm purge
func confirmPurge(count int) bool {
	prompt := fmt.Sprintf("This will permanently delete %d context(s) and all associated data. Are you sure?", count)
	if !promptYesNo(prompt, false) {
		fmt.Println("Purge cancelled")
		return false
	}
	return true
}

// purgeContexts performs the actual purge operation
func purgeContexts(store sessions.SessionStore, contextIDs []string) error {
	deletedCount := 0
	skippedCount := 0
	for _, contextID := range contextIDs {
		store.Delete(contextID)
		if store.Exists(contextID) {
			skippedCount++ // likely in use
			continue
		}
		deletedCount++
	}

	fmt.Printf("Purged %d context(s)\n", deletedCount)
	if skippedCount > 0 {
		fmt.Fprintf(os.Stderr, "Skipped %d in-use context(s)\n", skippedCount)
	}
	return nil
}

// resetContext clears the conversation history but preserves the context settings
func resetContext(sessionStore sessions.SessionStore, name string) error {
    // Get the session (creates if doesn't exist)
    session, err := sessionStore.Get(name)
    if err != nil {
        return fmt.Errorf("failed to get session for context %s: %w", name, err)
    }
    // Ensure we release the file lock
    defer session.Close()

	// Clear the session history
    session.Clear()

    return nil
}


// checkAndPromptForMissingContext checks if a context exists and creates it if missing
// Returns the context name to use (existing or newly created)
func checkAndPromptForMissingContext(sessionStore sessions.SessionStore, contextName string) string {
	if contextName == "" {
		return contextName // No context specified
	}

	// Check if context exists
	if sessionStore.Exists(contextName) {
		return contextName // Context exists, use it
	}

	// Context doesn't exist, create it
	contextDisplay := contextName
	// Show shortened version for long names
	if len(contextName) > 20 {
		contextDisplay = contextName[:8] + "..."
	}

	// Get the session to create it (this will initialize the context)
	if session, err := sessionStore.Get(contextName); err != nil {
		// If we can't create the context, return empty string to signal cancellation
		return ""
	} else {
		session.Close() // Release the lock immediately
	}
	fmt.Fprintf(os.Stderr, "Created new context '%s'\n", contextDisplay)

	return contextName
}
