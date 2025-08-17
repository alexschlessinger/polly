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

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
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
		config.ResetContext ||
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
	if name == "" || name == "true" {
		return true // No specific context, proceed
	}

	// Check if context already exists
	if !fileStore.ContextExists(name) {
		fmt.Fprintf(os.Stderr, "Error: context '%s' does not exist\n", name)
		return false // Context doesn't exist, cannot reset
	}

	// Context exists, prompt for reset
	prompt := fmt.Sprintf("Reset context '%s' (clear conversation history)?", name)
	if !promptYesNo(prompt) {
		fmt.Fprintf(os.Stderr, "Reset cancelled\n")
		return false
	}

	// Reset the context (preserve settings, clear history)
	if err := resetContext(fileStore, name); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to reset context: %v\n", err)
		return false
	}

	fmt.Fprintf(os.Stderr, "Reset context '%s'\n", name)
	return true
}

// resetContext clears the conversation history but preserves the context settings
func resetContext(fileStore *sessions.FileSessionStore, name string) error {
	// Get existing context info
	contextInfo := fileStore.GetContextByNameOrID(name)
	if contextInfo == nil {
		// No stored settings, just delete the file
		sessionPath := filepath.Join(fileStore.GetBaseDir(), name+".json")
		os.Remove(sessionPath)
		return nil
	}

	// Update last used time
	contextInfo.LastUsed = time.Now()

	// Save updated context info
	if err := fileStore.SaveContextInfo(contextInfo); err != nil {
		return err
	}

	// Delete conversation file (using name directly since it's already validated)
	sessionPath := filepath.Join(fileStore.GetBaseDir(), name+".json")
	os.Remove(sessionPath)

	return nil
}

// validateContextName checks if a context name is valid
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

// checkAndPromptForMissingContext checks if a context exists and creates it if missing
// Returns the context name to use (existing or newly created)
func checkAndPromptForMissingContext(fileStore *sessions.FileSessionStore, contextName string) string {
	if contextName == "" {
		return contextName // No context specified
	}

	// Check if context exists
	if fileStore.ContextExists(contextName) {
		return contextName // Context exists, use it
	}

	// Context doesn't exist, create it
	contextDisplay := contextName
	// Show shortened version for long names
	if len(contextName) > 20 {
		contextDisplay = contextName[:8] + "..."
	}

	// Save metadata for the new context
	fileStore.SaveContextName(contextName, "")
	fmt.Fprintf(os.Stderr, "Created new context '%s'\n", contextDisplay)

	return contextName
}
