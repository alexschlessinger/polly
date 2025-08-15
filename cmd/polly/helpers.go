package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/pkdindustries/pollytool/messages"
	"github.com/pkdindustries/pollytool/sessions"
)

// readFromStdin reads all lines from stdin and joins them with newlines
func readFromStdin() (string, error) {
	scanner := bufio.NewScanner(os.Stdin)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("error reading stdin: %w", err)
	}
	return strings.Join(lines, "\n"), nil
}

// hasStdinData checks if stdin has data available
func hasStdinData() bool {
	stat, _ := os.Stdin.Stat()
	return (stat.Mode() & os.ModeCharDevice) == 0
}

// setupSignalHandling sets up signal handling for graceful shutdown
func setupSignalHandling(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		cancel()
	}()
	return ctx, cancel
}

// ensureSystemPrompt adds a system prompt to the session if it's empty
func ensureSystemPrompt(session sessions.Session, systemPrompt string) {
	if len(session.GetHistory()) == 0 {
		if systemPrompt == "" {
			systemPrompt = defaultSystemPrompt
		}
		session.AddMessage(messages.ChatMessage{
			Role:    messages.MessageRoleSystem,
			Content: systemPrompt,
		})
	}
}

// closeFileSession safely closes a file session if applicable
func closeFileSession(session sessions.Session) {
	if fileSession, ok := session.(*sessions.FileSession); ok {
		fileSession.Close()
	}
}

// loadAPIKeys loads API keys from environment variables
func loadAPIKeys() map[string]string {
	return map[string]string{
		"ollama":    os.Getenv("POLLYTOOL_OLLAMAKEY"),
		"openai":    os.Getenv("POLLYTOOL_OPENAIKEY"),
		"anthropic": os.Getenv("POLLYTOOL_ANTHROPICKEY"),
		"gemini":    os.Getenv("POLLYTOOL_GEMINIKEY"),
	}
}

// getContextID determines the context ID from config or environment
func getContextID(config *Config) string {
	if config.ContextID != "" {
		return config.ContextID
	}
	return os.Getenv("POLLYTOOL_CONTEXT")
}

// needsFileStore determines if we need a file-based session store
func needsFileStore(config *Config, contextID string) bool {
	return contextID != "" ||
		config.ResetContext != "" ||
		config.UseLastContext ||
		config.ListContexts ||
		config.DeleteContext != "" ||
		config.AddToContext
}

// promptYesNo prompts the user for a yes/no response (defaults to yes)
func promptYesNo(prompt string) bool {
	fmt.Fprintf(os.Stderr, "%s (Y/n): ", prompt)
	reader := bufio.NewReader(os.Stdin)
	response, err := reader.ReadString('\n')
	if err != nil {
		return false
	}
	response = strings.TrimSpace(strings.ToLower(response))
	// Default to yes if user just presses enter
	if response == "" {
		return true
	}
	return response == "y" || response == "yes"
}

// checkAndPromptForReset checks if a context exists and prompts to reset it
// Returns true if we should proceed with resetting the context
func checkAndPromptForReset(fileStore *sessions.FileSessionStore, name string) bool {
	if name == "" || name == "true" || !strings.HasPrefix(name, "@") {
		return true // Not a named context, proceed
	}

	// Check if context name already exists
	existingID := fileStore.ResolveContext(name)
	if existingID == "" {
		return true // Doesn't exist, will create new
	}

	// Context exists, prompt for reset
	prompt := fmt.Sprintf("Reset context '%s' (clear conversation history)?", name)
	if !promptYesNo(prompt) {
		fmt.Fprintf(os.Stderr, "Reset cancelled\n")
		return false
	}

	// Reset the context (preserve settings, clear history)
	if err := resetContext(fileStore, name, existingID); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to reset context: %v\n", err)
		return false
	}
	
	fmt.Fprintf(os.Stderr, "Reset context '%s'\n", name)
	return true
}

// resetContext clears the conversation history but preserves the context settings
func resetContext(fileStore *sessions.FileSessionStore, name string, oldID string) error {
	// Get existing context info
	contextInfo := fileStore.GetContextByNameOrID(name)
	if contextInfo == nil {
		// No stored settings, just delete the file
		sessionPath := filepath.Join(fileStore.GetBaseDir(), oldID+".json")
		os.Remove(sessionPath)
		return nil
	}

	// Generate new ID for the reset context
	newID := sessions.GenerateSessionID()
	
	// Update the context info with new ID but preserve all settings
	contextInfo.ID = newID
	contextInfo.LastUsed = time.Now()
	
	// Save updated context info
	if err := fileStore.SaveContextInfo(contextInfo); err != nil {
		return err
	}
	
	// Delete old conversation file
	sessionPath := filepath.Join(fileStore.GetBaseDir(), oldID+".json")
	os.Remove(sessionPath)
	
	return nil
}

// checkAndPromptForMissingContext checks if a context exists and creates it if missing
// Returns the context ID to use (existing or newly created)
func checkAndPromptForMissingContext(fileStore *sessions.FileSessionStore, contextID string) string {
	if contextID == "" {
		return contextID // No context specified
	}

	// Check if context exists
	if fileStore.ContextExists(contextID) {
		return contextID // Context exists, use it
	}

	// Context doesn't exist, create it
	contextDisplay := contextID
	if !strings.HasPrefix(contextID, "@") {
		// For IDs, show shortened version for readability
		if len(contextID) > 8 {
			contextDisplay = contextID[:8] + "..."
		}
	}

	// If it's a named context, save the name mapping
	if strings.HasPrefix(contextID, "@") {
		newID := sessions.GenerateSessionID()
		fileStore.SaveContextName(contextID, newID)
		fmt.Fprintf(os.Stderr, "Created new context '%s'\n", contextID)
		return newID
	}

	// For regular IDs, just return as-is (will be created when accessed)
	fmt.Fprintf(os.Stderr, "Created new context '%s'\n", contextDisplay)
	return contextID
}
