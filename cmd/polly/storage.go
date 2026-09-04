package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/urfave/cli/v3"
)

var defaultNativeToolNames = []string{
	"bash",
	"read_file",
	"list_dir",
	"search_files",
	"write_file",
	"edit_file",
}

// needsFileStore determines whether the unified store should use disk mode.
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

// setupSessionStore opens the unified SQLite store. forceFile selects disk
// mode even when no flag demands persistence (used for auto-named REPL
// contexts).
func setupSessionStore(config *Config, contextID string, forceFile bool) (sessions.SessionStore, error) {
	// New sessions must start with the fully resolved invocation settings.
	// Existing sessions restore every persisted value, including meaningful
	// zeros, so a partial default would make a freshly named context look like
	// an existing context whose model and limits were intentionally cleared.
	defaultInfo := metadataFromConfig(config)

	storeConfig := sessions.StoreConfig{
		Mode:            sessions.ModeMemory,
		DefaultMetadata: defaultInfo,
		AutoSessionTTL:  7 * 24 * time.Hour,
	}
	if forceFile || needsFileStore(config, contextID) {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve home directory: %w", err)
		}
		storeConfig.Mode = sessions.ModeDisk
		storeConfig.Path = filepath.Join(homeDir, ".pollytool", "polly.db")
	}
	return sessions.OpenStore(storeConfig)
}

func metadataFromConfig(config *Config) *sessions.Metadata {
	metadata := &sessions.Metadata{}
	for _, spec := range settingSpecs {
		if spec.toMeta != nil {
			spec.toMeta(&config.Launch, metadata)
		}
	}
	if len(config.Tools) == 0 {
		metadata.ActiveTools = make([]tools.ToolLoaderInfo, 0, len(defaultNativeToolNames))
		for _, name := range defaultNativeToolNames {
			metadata.ActiveTools = append(metadata.ActiveTools, tools.ToolLoaderInfo{
				Name: name, Type: "native", Source: "builtin",
			})
		}
	}
	return metadata
}

// handleListContexts lists all available contexts
func handleListContexts(ctx context.Context, store sessions.SessionStore) error {
	contexts, err := store.GetAllMetadata(ctx)
	if err != nil {
		return fmt.Errorf("list context metadata: %w", err)
	}
	lastContext, err := store.GetLast(ctx)
	if err != nil {
		return fmt.Errorf("find last context: %w", err)
	}

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
		if info.Parent != "" {
			modelInfo += " (spawned by " + info.Parent + ")"
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
func handleDeleteContext(ctx context.Context, store sessions.SessionStore, contextID string) error {
	// Check if context exists
	exists, err := store.Exists(ctx, contextID)
	if err != nil {
		return fmt.Errorf("check context %q: %w", contextID, err)
	}
	if !exists {
		return fmt.Errorf("context '%s' not found", contextID)
	}

	// Prompt for confirmation (default to no for destructive operation)
	if !confirmDestructive(fmt.Sprintf("Delete context '%s' permanently?", contextID), "Delete cancelled") {
		return nil
	}

	return deleteContext(ctx, store, contextID)
}

// confirmDeletion prompts the user to confirm deletion
// confirmDestructive asks prompt with a default of no, printing cancelled
// when the user declines.
func confirmDestructive(prompt, cancelled string) bool {
	if !promptYesNo(prompt, false) {
		fmt.Println(cancelled)
		return false
	}
	return true
}

// deleteContext performs the actual deletion
func deleteContext(ctx context.Context, store sessions.SessionStore, contextID string) error {
	if err := store.Delete(ctx, contextID); err != nil {
		return fmt.Errorf("delete context %q: %w", contextID, err)
	}

	fmt.Printf("Context '%s' deleted\n", contextID)
	return nil
}

// handleAddToContext adds stdin content or file content to a context without making an API call
func handleAddToContext(ctx context.Context, store sessions.SessionStore, config *Config, contextID string) (retErr error) {
	if contextID == "" {
		// Try to use last context if available
		lastContext, err := store.GetLast(ctx)
		if err != nil {
			return fmt.Errorf("find last context: %w", err)
		}
		if lastContext != "" {
			contextDisplay := lastContext
			metadata, err := store.GetAllMetadata(ctx)
			if err != nil {
				return fmt.Errorf("list context metadata: %w", err)
			}
			if info := metadata[lastContext]; info != nil && info.Name != "" {
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

	session, err := store.Acquire(ctx, contextID, sessions.AcquireOptions{})
	if err != nil {
		return fmt.Errorf("failed to get session for context %s: %w", contextID, err)
	}
	defer func() {
		if err := session.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close context %s: %w", contextID, err))
		}
	}()

	// Collect the messages, then persist them all in a single write below.
	var msgs []messages.ChatMessage

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
			msgs = append(msgs, messages.ChatMessage{
				Role:    messages.MessageRoleUser,
				Content: content,
			})
		}

		// Add each file as a separate message. A text file keeps its filename
		// boundary in the provider-visible text; the part's FileName lets the
		// resumed REPL compact the body to "[attached: name]".
		for _, part := range parts {
			if part.Type == "text" && part.FileName != "" {
				part.Text = fmt.Sprintf("=== %s ===\n%s", part.FileName, part.Text)
			}
			msgs = append(msgs, messages.ChatMessage{
				Role:  messages.MessageRoleUser,
				Parts: []messages.ContentPart{part},
			})
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

		msgs = append(msgs, messages.ChatMessage{
			Role:    messages.MessageRoleUser,
			Content: content,
		})
	}
	for i := range msgs {
		if msgs[i].Metadata == nil {
			msgs[i].Metadata = make(map[string]any)
		}
		msgs[i].Metadata[messages.MetadataKeyContextImport] = true
	}
	artifactStore := session.ArtifactStore()
	for i := range msgs {
		msgs[i], err = externalizeMessageImages(ctx, msgs[i], artifactStore)
		if err != nil {
			return fmt.Errorf("persist imported artifacts: %w", err)
		}
	}

	if err := session.AddMessages(ctx, msgs); err != nil {
		return fmt.Errorf("failed to add to context %s: %w", contextID, err)
	}

	if !config.Quiet {
		fmt.Fprintf(os.Stderr, "Added to context %s\n", contextID)
	}
	return nil
}

// getOrCreateSession gets an existing session or creates a new one
func getOrCreateSession(ctx context.Context, store sessions.SessionStore, contextID string, needFileStore, auto bool) (sessions.Session, error) {
	if contextID == "" && !needFileStore {
		contextID = "default" // Memory store context
	}
	session, err := store.Acquire(ctx, contextID, sessions.AcquireOptions{Auto: auto})
	if err != nil {
		return nil, fmt.Errorf("get session for context %q: %w", contextID, err)
	}
	return session, nil
}

// handleCreateContext creates a new context with the specified configuration
func handleCreateContext(ctx context.Context, store sessions.SessionStore, config *Config, contextID string) (retErr error) {
	if contextID == "" {
		return fmt.Errorf("--create requires a context name (use -c or POLLYTOOL_CONTEXT)")
	}

	// Check if context already exists
	exists, err := store.Exists(ctx, contextID)
	if err != nil {
		return fmt.Errorf("check context %q: %w", contextID, err)
	}
	if exists {
		return fmt.Errorf("context '%s' already exists", contextID)
	}

	// Create context info with all resolved settings.
	info := metadataFromConfig(config)
	info.Name = contextID
	info.Created = time.Now()
	info.LastUsed = time.Now()

	// Create session and set its context info
	session, err := store.Acquire(ctx, contextID, sessions.AcquireOptions{})
	if err != nil {
		return fmt.Errorf("failed to create context: %w", err)
	}
	defer func() {
		if err := session.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close context %s: %w", contextID, err))
		}
	}()

	if err := session.SetMetadata(ctx, info); err != nil {
		return fmt.Errorf("failed to create context '%s': %w", contextID, err)
	}

	return showContext(ctx, store, contextID)
}

// handleShowContext shows the configuration for a context
func handleShowContext(ctx context.Context, store sessions.SessionStore, contextID string) error {
	if contextID == "" {
		return fmt.Errorf("--show requires a context name")
	}

	return showContext(ctx, store, contextID)
}

func showContext(ctx context.Context, store sessions.SessionStore, contextID string) error {
	metadata, err := store.GetAllMetadata(ctx)
	if err != nil {
		return fmt.Errorf("list context metadata: %w", err)
	}
	info := metadata[contextID]
	if info == nil {
		return fmt.Errorf("context '%s' not found", contextID)
	}

	// Display detailed configuration
	fmt.Printf("Context: %s\n", info.Name)

	// Timestamps
	fmt.Printf("  Created: %s\n", info.Created.Format("2006-01-02 15:04:05"))
	fmt.Printf("  Last Used: %s (%s)\n",
		info.LastUsed.Format("2006-01-02 15:04:05"),
		formatDuration(time.Since(info.LastUsed)))

	// Model configuration
	fmt.Printf("  Model: %s\n", info.Model)
	fmt.Printf("  Temperature: %.2f\n", info.Temperature)
	fmt.Printf("  Max Tokens: %d\n", info.MaxTokens)
	fmt.Printf("  Thinking: %s\n", info.ThinkingEffort)

	// Conversation settings
	fmt.Printf("  Max Context: %d tokens\n", info.MaxHistoryTokens)
	fmt.Printf("  TTL: %s\n", info.TTL)

	// Prompts and description
	fmt.Printf("  Description: %s\n", info.Description)
	if info.Parent != "" {
		fmt.Printf("  Spawned By: %s\n", info.Parent)
	}
	if info.SystemPrompt != "" {
		fmt.Printf("  System Prompt: %s\n", info.SystemPrompt)
	} else {
		fmt.Printf("  System Prompt: (none)\n")
	}

	// Tool configuration
	if len(info.ActiveTools) > 0 {
		fmt.Println("  Active Tools:")
		for _, loader := range info.ActiveTools {
			fmt.Printf("    - %s [%s] from %s\n", loader.Name, loader.Type, loader.Source)
		}
	} else {
		fmt.Printf("  Active Tools: []\n")
	}
	if len(info.ActiveSkills) > 0 {
		fmt.Println("  Active Skills:")
		for _, skill := range info.ActiveSkills {
			fmt.Printf("    - %s\n", skill)
		}
	} else {
		fmt.Printf("  Active Skills: []\n")
	}
	fmt.Printf("  Tool Timeout: %s\n", info.ToolTimeout)

	return nil
}

// handleResetContext resets a context (clears conversation, keeps settings)
func handleResetContext(ctx context.Context, store sessions.SessionStore, config *Config, cmd *cli.Command, contextID string) (retErr error) {
	if contextID == "" {
		return fmt.Errorf("--reset requires a context name")
	}

	// Check if context exists
	exists, err := store.Exists(ctx, contextID)
	if err != nil {
		return fmt.Errorf("check context %q: %w", contextID, err)
	}
	if !exists {
		return fmt.Errorf("context '%s' does not exist", contextID)
	}

	// Prompt for confirmation
	if !confirmDestructive(fmt.Sprintf("Reset context '%s' (clear conversation history)?", contextID), "Reset cancelled") {
		return nil
	}

	// Apply every explicit override and clear the conversation in one
	// transaction. This keeps the rebuilt system message, settings, transcript,
	// and artifact ownership consistent even if the reset fails partway through.
	session, err := store.Acquire(ctx, contextID, sessions.AcquireOptions{})
	if err != nil {
		return fmt.Errorf("failed to get session: %w", err)
	}
	defer func() {
		if err := session.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close context %s: %w", contextID, err))
		}
	}()

	// Apply only explicitly-set command-line flags: a plain --reset must not
	// replace stored settings with CLI defaults, and explicit zeros
	// (--maxcontext 0 = unlimited) must stick.
	md, err := session.GetMetadata(ctx)
	if err != nil {
		return fmt.Errorf("read context metadata: %w", err)
	}
	if md == nil {
		md = &sessions.Metadata{Name: contextID}
	}
	applyFlagSettings(md, &config.Launch, cmd)

	if err := session.Reset(ctx, md); err != nil {
		return fmt.Errorf("failed to reset context: %w", err)
	}

	fmt.Printf("Reset context '%s' (cleared conversation, kept settings)\n", contextID)
	return nil
}

// confirmReset prompts the user to confirm reset
// handlePurgeAll deletes all sessions.
func handlePurgeAll(ctx context.Context, store sessions.SessionStore) error {
	// Get count of contexts for the confirmation message
	contextIDs, err := store.List(ctx)
	if err != nil {
		return fmt.Errorf("failed to list contexts: %w", err)
	}

	if len(contextIDs) == 0 {
		fmt.Println("No contexts to purge")
		return nil
	}

	// Prompt for confirmation
	if !confirmDestructive(fmt.Sprintf("This will permanently delete %d context(s) and all associated data. Are you sure?", len(contextIDs)), "Purge cancelled") {
		return nil
	}

	return purgeContexts(ctx, store, contextIDs)
}

// confirmPurge prompts the user to confirm purge
// purgeContexts performs the actual purge operation
func purgeContexts(ctx context.Context, store sessions.SessionStore, contextIDs []string) error {
	deletedCount := 0
	var deleteErrors []error
	for _, contextID := range contextIDs {
		if err := store.Delete(ctx, contextID); err != nil {
			deleteErrors = append(deleteErrors, fmt.Errorf("delete context %q: %w", contextID, err))
			continue
		}
		deletedCount++
	}

	fmt.Printf("Purged %d context(s)\n", deletedCount)
	return errors.Join(deleteErrors...)
}

// resetContextWithSystemPrompt clears the conversation history while
// preserving the context settings, with systemPrompt as the new persona.
func resetContextWithSystemPrompt(ctx context.Context, sessionStore sessions.SessionStore, name, systemPrompt string) (retErr error) {
	// Get the session (creates if doesn't exist)
	session, err := sessionStore.Acquire(ctx, name, sessions.AcquireOptions{})
	if err != nil {
		return fmt.Errorf("failed to get session for context %s: %w", name, err)
	}
	// Ensure we release the file lock
	defer func() {
		if err := session.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close context %s: %w", name, err))
		}
	}()
	metadata, err := session.GetMetadata(ctx)
	if err != nil {
		return fmt.Errorf("failed to read context %s metadata: %w", name, err)
	}
	if metadata == nil {
		metadata = &sessions.Metadata{}
	}
	metadata.SystemPrompt = systemPrompt
	if err := session.Reset(ctx, metadata); err != nil {
		return fmt.Errorf("failed to reset context %s with system prompt: %w", name, err)
	}
	return nil
}

// checkAndPromptForMissingContext checks if a context exists and creates it if missing
// Returns the context name to use (existing or newly created)
func checkAndPromptForMissingContext(ctx context.Context, sessionStore sessions.SessionStore, contextName string) (string, error) {
	if contextName == "" {
		return contextName, nil // No context specified
	}

	// Check if context exists
	exists, err := sessionStore.Exists(ctx, contextName)
	if err != nil {
		return "", fmt.Errorf("check context %q: %w", contextName, err)
	}
	if exists {
		return contextName, nil // Context exists, use it
	}

	// Context doesn't exist, create it
	contextDisplay := contextName
	// Show shortened version for long names
	if len(contextName) > 20 {
		contextDisplay = contextName[:8] + "..."
	}

	// Get the session to create it (this will initialize the context)
	if session, err := sessionStore.Acquire(ctx, contextName, sessions.AcquireOptions{}); err != nil {
		return "", fmt.Errorf("create context %q: %w", contextName, err)
	} else {
		if err := session.Close(); err != nil {
			return "", fmt.Errorf("close new context %q: %w", contextName, err)
		}
	}
	fmt.Fprintf(os.Stderr, "Created new context '%s'\n", contextDisplay)

	return contextName, nil
}
