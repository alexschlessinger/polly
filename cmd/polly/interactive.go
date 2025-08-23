package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/chzyer/readline"
)

// runInteractiveMode runs the CLI in interactive mode with readline support
func runInteractiveMode(ctx context.Context, config *Config, session sessions.Session, multipass llm.LLM, toolRegistry *tools.ToolRegistry, contextID string) error {
	// Note: session is already initialized by caller, no need to close it here

	// Initialize colors based on terminal background
	initColors()

	// Create the file attachment listener
	listener := &fileAttachListener{
		session:        session,
		config:         config,
		processedPaths: make(map[string]bool),
	}

	// Configure readline
	rl, err := readline.NewEx(&readline.Config{
		Prompt:          "> ",
		HistoryFile:     getHistoryFilePath(contextID),
		AutoComplete:    createAutoCompleter(),
		InterruptPrompt: "^C",
		EOFPrompt:       "exit",

		HistorySearchFold:   true,
		FuncFilterInputRune: filterInput,
		Listener:            listener,
	})
	if err != nil {
		return err
	}
	defer rl.Close()

	// Print welcome message
	printWelcomeMessage(config, contextID)

	// Show recent history if resuming an existing conversation
	hasHistory := showRecentHistory(session)
	if hasHistory {
		fmt.Println()
		fmt.Println(dimStyle.Styled("─── Resuming context ───"))
	}

	// Track the current completion cancellation
	var currentCompletionCancel context.CancelFunc

	// Set up signal handling for interactive mode
	// This catches SIGINT during completion and cancels it instead of exiting
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt)
	go func() {
		for range sigChan {
			// Cancel current completion if running
			if currentCompletionCancel != nil {
				currentCompletionCancel()
				currentCompletionCancel = nil
			}
		}
	}()
	// Stop catching signals when we exit
	defer signal.Stop(sigChan)

	// Ensure we cancel any running completion on exit
	defer func() {
		if currentCompletionCancel != nil {
			currentCompletionCancel()
		}
	}()

	// Main interactive loop
	for {
		// Check for context cancellation
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		// Reset the listener's processed paths for each new input line
		listener.processedPaths = make(map[string]bool)

		// Read input - simple, just get whatever they type/paste
		input, err := rl.Readline()
		if err == readline.ErrInterrupt {
			// Ctrl-C during readline input - just show message
			fmt.Println(dimStyle.Styled("Use /exit or Ctrl-D to quit"))
			continue
		} else if err == io.EOF {
			// Cancel any running completion before exiting
			if currentCompletionCancel != nil {
				currentCompletionCancel()
			}
			return nil
		} else if err != nil {
			return err
		}

		// Skip empty input
		input = strings.TrimSpace(input)
		if input == "" {
			continue
		}

		// Handle special commands - only process known commands
		if strings.HasPrefix(input, "/") && isKnownCommand(input) {
			if handled := handleInteractiveCommand(input, config, session, rl); handled {
				continue
			}
		}

		// At this point, any files have already been attached by the listener
		// The input is just the message text (files have been removed)
		// Simply create a text message
		userMsg := messages.ChatMessage{
			Role:    messages.MessageRoleUser,
			Content: input,
		}

		// Add user message and execute
		session.AddMessage(userMsg)

		// Create a cancellable context for this completion
		completionCtx, cancel := context.WithCancel(ctx)
		currentCompletionCancel = cancel

		// Create interactive status handler
		interactiveStatus := NewInteractiveStatus()

		// Execute completion with cancellable context and interactive status
		executeCompletion(completionCtx, config, multipass, session, toolRegistry, nil, interactiveStatus)

		// Clear the cancel function after completion
		currentCompletionCancel = nil

	}
}

// isKnownCommand checks if the input starts with a known command
func isKnownCommand(input string) bool {
	parts := strings.Fields(input)
	if len(parts) == 0 {
		return false
	}

	command := strings.ToLower(parts[0])
	knownCommands := []string{
		"/exit", "/quit", "/q",
		"/clear", "/cls",
		"/reset",
		"/model", "/m",
		"/temp", "/temperature",
		"/history", "/h",
		"/save",
		"/file", "/f",
		"/help", "/?",
		"/context", "/c",
		"/system", "/sys",
		"/debug",
	}

	for _, known := range knownCommands {
		if command == known {
			return true
		}
	}
	return false
}

// handleInteractiveCommand processes special interactive commands
func handleInteractiveCommand(input string, config *Config, session sessions.Session, rl *readline.Instance) bool {
	parts := strings.Fields(input)
	if len(parts) == 0 {
		return false
	}

	command := strings.ToLower(parts[0])
	switch command {
	case "/exit", "/quit", "/q":
		cleanupAndExit(0)

	case "/clear", "/cls":
		clearScreen()
		return true

case "/reset":
        // Clear the session history for any session implementation
        session.Clear()
        fmt.Println(successStyle.Styled("Conversation reset."))
        return true

    case "/model", "/m":
        if len(parts) < 2 {
            fmt.Printf("Current model: %s\n", highlightStyle.Styled(config.Model))
            fmt.Println(dimStyle.Styled("Usage: /model <model-name>"))
            fmt.Println(dimStyle.Styled("Example: /model openai/gpt-4o"))
        } else {
            config.Model = parts[1]
            if contextInfo := session.GetContextInfo(); contextInfo != nil {
                contextInfo.Model = config.Model
                session.SetContextInfo(contextInfo)
            }
            fmt.Println(successStyle.Styled(fmt.Sprintf("Switched to model: %s", config.Model)))
        }
        return true

    case "/temp", "/temperature":
        if len(parts) < 2 {
            fmt.Printf("Current temperature: %s\n", highlightStyle.Styled(fmt.Sprintf("%.2f", config.Temperature)))
            fmt.Println(dimStyle.Styled("Usage: /temp <0.0-2.0>"))
        } else {
            if temp, err := parseFloat(parts[1]); err == nil {
                if temp < 0.0 || temp > 2.0 {
                    fmt.Println(errorStyle.Styled("Temperature must be between 0.0 and 2.0"))
                    return true
                }
                config.Temperature = temp
                if contextInfo := session.GetContextInfo(); contextInfo != nil {
                    contextInfo.Temperature = temp
                    session.SetContextInfo(contextInfo)
                }
                fmt.Println(successStyle.Styled(fmt.Sprintf("Temperature set to: %.2f", config.Temperature)))
            } else {
                fmt.Println(errorStyle.Styled("Invalid temperature value"))
            }
        }
        return true

	case "/history", "/h":
		showHistory(session)
		return true

	case "/save":
		if len(parts) < 2 {
			fmt.Println(dimStyle.Styled("Usage: /save <filename>"))
		} else {
			saveConversation(session, parts[1])
		}
		return true

	case "/file", "/f":
		if len(parts) < 2 {
			fmt.Println(dimStyle.Styled("Usage: /file <path>"))
		} else {
			// Get the file path (join in case it has spaces)
			filePath := strings.Join(parts[1:], " ")

			// Expand home directory if needed
			if strings.HasPrefix(filePath, "~/") {
				home, _ := os.UserHomeDir()
				filePath = filepath.Join(home, filePath[2:])
			}

			// Validate the file exists
			if _, err := os.Stat(filePath); err != nil {
				fmt.Println(errorStyle.Styled(fmt.Sprintf("File not found: %s", filePath)))
				return true
			}

			// Build and add file message to session
			userMsg, err := buildMessageWithFiles("", []string{filePath})
			if err != nil {
				fmt.Println(errorStyle.Styled(fmt.Sprintf("Error processing file: %v", err)))
				return true
			}

			// Add to session
			session.AddMessage(userMsg)

			// Show confirmation
			fileInfo := getFileInfo(filePath)
			fmt.Println(dimStyle.Styled(fmt.Sprintf("📎 Attached: %s", fileInfo)))
		}
		return true

	case "/help", "/?":
		printInteractiveHelp()
		return true

	case "/context", "/c":
		if len(parts) < 2 {
			fmt.Printf("Current context: %s\n", highlightStyle.Styled(getContextDisplayName(session)))
		} else {
			fmt.Println(dimStyle.Styled("To switch context, exit and restart with -c flag"))
		}
		return true

    case "/system", "/sys":
        if len(parts) < 2 {
            fmt.Printf("Current system prompt: %s\n", highlightStyle.Styled(config.SystemPrompt))
        } else {
            // Join all parts after the command as the new system prompt
            newPrompt := strings.Join(parts[1:], " ")
            // Update config and session metadata
            config.SystemPrompt = newPrompt
            if contextInfo := session.GetContextInfo(); contextInfo != nil {
                contextInfo.SystemPrompt = newPrompt
                session.SetContextInfo(contextInfo)
            }
            // Reset conversation history to apply new system prompt
            session.Clear()
            fmt.Println(successStyle.Styled("System prompt updated and conversation reset."))
        }
        return true

	case "/debug":
		config.Debug = !config.Debug
		fmt.Println(successStyle.Styled(fmt.Sprintf("Debug mode: %v", config.Debug)))
		return true
	}

	// Don't show "unknown command" for paths that start with /
	// Let them fall through to file path handling
	return false
}


// getHistoryFilePath returns the path for readline history
func getHistoryFilePath(contextID string) string {
	homeDir, _ := os.UserHomeDir()
	pollyDir := filepath.Join(homeDir, ".pollytool")
	os.MkdirAll(pollyDir, 0755)

	if contextID != "" {
		return filepath.Join(pollyDir, fmt.Sprintf(".history_%s", contextID))
	}
	return filepath.Join(pollyDir, ".history")
}

// createAutoCompleter creates readline auto-completer
func createAutoCompleter() *readline.PrefixCompleter {
	return readline.NewPrefixCompleter(
		readline.PcItem("/exit"),
		readline.PcItem("/quit"),
		readline.PcItem("/clear"),
		readline.PcItem("/reset"),
		readline.PcItem("/model",
			readline.PcItem("openai/gpt-4.1"),
			readline.PcItem("openai/gpt-4.1-mini"),
			readline.PcItem("anthropic/claude-3-5-haiku-latest"),
			readline.PcItem("anthropic/claude-sonnet-4-20250514"),
			readline.PcItem("anthropic/claude-opus-4-1-20250805"),
			readline.PcItem("gemini/gemini-2.5-flash"),
			readline.PcItem("gemini/gemini-2.5-pro"),
			readline.PcItem("ollama/gpt-oss:latest"),
		),
		readline.PcItem("/temp"),
		readline.PcItem("/history"),
		readline.PcItem("/save"),
		readline.PcItem("/help"),
		readline.PcItem("/context"),
		readline.PcItem("/system"),
		readline.PcItem("/debug"),
		readline.PcItem("/file"),
		readline.PcItem("/f"),
	)
}

// filterInput filters input runes for readline
func filterInput(r rune) (rune, bool) {
	switch r {
	case readline.CharCtrlZ:
		return r, false
	}
	return r, true
}

// printWelcomeMessage prints the interactive mode welcome message
func printWelcomeMessage(config *Config, contextID string) {
	// Show all configuration
	fmt.Printf("Model: %s\n", highlightStyle.Styled(config.Model))
	if contextID != "" {
		fmt.Printf("Context: %s\n", highlightStyle.Styled(contextID))
	}
	fmt.Printf("Temperature: %s\n", highlightStyle.Styled(fmt.Sprintf("%.1f", config.Temperature)))
	fmt.Printf("Max Tokens: %s\n", highlightStyle.Styled(fmt.Sprintf("%d", config.MaxTokens)))

	// Show tools if configured
	if len(config.ToolPaths) > 0 {
		fmt.Print("Tools: ")
		tools := []string{}
		tools = append(tools, config.ToolPaths...)
		fmt.Println(highlightStyle.Styled(strings.Join(tools, ", ")))
	}
	if len(config.MCPServers) > 0 {
		fmt.Print("MCP Servers: ")
		fmt.Println(highlightStyle.Styled(strings.Join(config.MCPServers, ", ")))
	}

	fmt.Println()
}

// printInteractiveHelp prints help for interactive commands
func printInteractiveHelp() {
	commands := []struct {
		cmd  string
		desc string
	}{
		{"/exit, /quit", "Exit interactive mode"},
		{"/clear", "Clear the screen"},
		{"/reset", "Reset conversation history"},
		{"/model <name>", "Switch to a different model"},
		{"/temp <0.0-2.0>", "Set temperature"},
		{"/history", "Show conversation history"},
		{"/save <file>", "Save conversation to file"},
		{"/file <path>", "Attach a file to the conversation"},
		{"/context", "Show current context"},
		{"/system <prompt>", "Update system prompt"},
		{"/debug", "Toggle debug mode"},
		{"/help", "Show this help message"},
	}

	for _, c := range commands {
		fmt.Printf("  %-18s %s\n", highlightStyle.Styled(c.cmd), c.desc)
	}

}

// clearScreen clears the terminal screen
func clearScreen() {
	output.ClearScreen()
	output.MoveCursor(1, 1)
}

// formatConversation formats conversation history in a consistent way
func formatConversation(history []messages.ChatMessage) string {
	if len(history) == 0 {
		return ""
	}

	var builder strings.Builder
	for _, msg := range history {
		// Style the role header based on the role type
		var roleHeader string
		switch msg.Role {
		case messages.MessageRoleUser:
			roleHeader = userStyle.Styled(fmt.Sprintf("═══ %s ═══", string(msg.Role)))
		case messages.MessageRoleAssistant:
			roleHeader = assistantStyle.Styled(fmt.Sprintf("═══ %s ═══", string(msg.Role)))
		case messages.MessageRoleSystem:
			roleHeader = systemStyle.Styled(fmt.Sprintf("═══ %s ═══", string(msg.Role)))
		default:
			roleHeader = fmt.Sprintf("═══ %s ═══", string(msg.Role))
		}

		fmt.Fprintf(&builder, "%s\n", roleHeader)

		// Handle file attachments when content is empty
		if msg.Content == "" && len(msg.Parts) > 0 {
			var attachments []string
			for _, part := range msg.Parts {
				if part.FileName != "" {
					attachments = append(attachments, fmt.Sprintf("📎 %s", part.FileName))
				} else if part.Type == "image_base64" || part.Type == "image_url" {
					attachments = append(attachments, "📎 [image]")
				} else if part.Text != "" {
					// If there's text in parts, show it
					fmt.Fprintf(&builder, "%s\n", part.Text)
				}
			}
			if len(attachments) > 0 {
				fmt.Fprintf(&builder, "%s\n", strings.Join(attachments, "\n"))
			}
		} else {
			// Regular content
			fmt.Fprintf(&builder, "%s\n", msg.Content)
		}
		fmt.Fprintf(&builder, "\n")
	}
	return builder.String()
}

// showRecentHistory displays recent conversation history (up to 25 lines)
func showRecentHistory(session sessions.Session) bool {
	history := session.GetHistory()
	if len(history) == 0 {
		return false
	}

	// Filter out system messages
	var filteredHistory []messages.ChatMessage
	for _, msg := range history {
		if msg.Role != messages.MessageRoleSystem {
			filteredHistory = append(filteredHistory, msg)
		}
	}

	if len(filteredHistory) == 0 {
		return false
	}

	// Format all messages
	formatted := formatConversation(filteredHistory)

	// Keep only last 25 lines
	lines := strings.Split(formatted, "\n")
	if len(lines) > 25 {
		fmt.Println(dimStyle.Styled("..."))
		lines = lines[len(lines)-25:]
		formatted = strings.Join(lines, "\n")
	}

	fmt.Print(formatted)
	return true
}

// showHistory displays conversation history
func showHistory(session sessions.Session) {
	history := session.GetHistory()
	if len(history) == 0 {
		fmt.Println(dimStyle.Styled("No conversation history."))
		return
	}

	formatted := formatConversation(history)
	fmt.Print(formatted)
}

// saveConversation saves the conversation to a file
func saveConversation(session sessions.Session, filename string) {
	history := session.GetHistory()
	if len(history) == 0 {
		fmt.Println(dimStyle.Styled("No conversation to save."))
		return
	}

	formatted := formatConversation(history)
	err := os.WriteFile(filename, []byte(formatted), 0644)
	if err != nil {
		fmt.Println(errorStyle.Styled(fmt.Sprintf("Error saving file: %v", err)))
		return
	}

	fmt.Println(successStyle.Styled(fmt.Sprintf("Conversation saved to %s", filename)))
}

// getContextDisplayName returns a display name for the current context
func getContextDisplayName(session sessions.Session) string {
	if fileSession, ok := session.(*sessions.FileSession); ok {
		if fileSession.ID != "" {
			return fileSession.ID
		}
	}
	return "default"
}

// parseFloat safely parses a string to float64
func parseFloat(s string) (float64, error) {
	var f float64
	_, err := fmt.Sscanf(s, "%f", &f)
	return f, err
}

// fileAttachListener handles real-time file detection and attachment
type fileAttachListener struct {
	session        sessions.Session
	config         *Config
	processedPaths map[string]bool // Track which paths we've already processed
}

// OnChange is called when the input line changes
func (l *fileAttachListener) OnChange(line []rune, pos int, key rune) (newLine []rune, newPos int, ok bool) {
	input := string(line)

	// Skip processing if this looks like a command
	if strings.HasPrefix(strings.TrimSpace(input), "/") {
		return line, pos, true
	}

	// Extract file paths from current input
	paths, remaining := extractFilePaths(input)

	// Check for new paths we haven't processed yet
	var newPaths []string
	for _, path := range paths {
		if !l.processedPaths[path] {
			newPaths = append(newPaths, path)
			l.processedPaths[path] = true
		}
	}

	// If we found new valid paths, attach them immediately
	if len(newPaths) > 0 {
		// Build a message with just the new files
		userMsg, err := buildMessageWithFiles("", newPaths)
		if err == nil {
			// Add files to session immediately
			l.session.AddMessage(userMsg)

            // Print attachment confirmations
            for _, path := range newPaths {
                fileInfo := getFileInfo(path)
                fmt.Printf("\n%s\n", dimStyle.Styled(fmt.Sprintf("📎 Attached: %s", fileInfo)))
            }
        }

		// Return the remaining text without file paths
		newLine = []rune(remaining)
		if pos > len(newLine) {
			newPos = len(newLine)
		} else {
			newPos = pos
		}
		return newLine, newPos, true
	}

	return line, pos, true
}

// extractFilePaths extracts file paths from user input that may contain drag-dropped files
// Handles various formats: 'path', "path", path\ with\ spaces, /absolute/path, C:\Windows\path
func extractFilePaths(input string) ([]string, string) {
	var paths []string
	remaining := input

	// Regular expressions for different path patterns
	// Single-quoted paths
	singleQuotedPattern := regexp.MustCompile(`'([^']+)'`)
	// Double-quoted paths
	doubleQuotedPattern := regexp.MustCompile(`"([^"]+)"`)
	// Escaped spaces (path\ with\ spaces)
	escapedSpacePattern := regexp.MustCompile(`([^\s]+(?:\\ [^\s]*)+)`)

	// Extract single-quoted paths
	matches := singleQuotedPattern.FindAllStringSubmatch(input, -1)
	for _, match := range matches {
		if len(match) > 1 {
			path := match[1]
			if isValidPath(path) {
				paths = append(paths, path)
				remaining = strings.Replace(remaining, match[0], "", 1)
			}
		}
	}

	// Extract double-quoted paths
	matches = doubleQuotedPattern.FindAllStringSubmatch(remaining, -1)
	for _, match := range matches {
		if len(match) > 1 {
			path := match[1]
			if isValidPath(path) {
				paths = append(paths, path)
				remaining = strings.Replace(remaining, match[0], "", 1)
			}
		}
	}

	// Extract escaped space paths
	matches = escapedSpacePattern.FindAllStringSubmatch(remaining, -1)
	for _, match := range matches {
		if len(match) > 0 {
			// Unescape the spaces
			path := strings.ReplaceAll(match[0], "\\ ", " ")
			if isValidPath(path) {
				paths = append(paths, path)
				remaining = strings.Replace(remaining, match[0], "", 1)
			}
		}
	}

	// Check for unquoted absolute paths (Unix/Mac)
	// or Windows paths (C:\, D:\, etc.)
	words := strings.Fields(remaining)
	var nonPathWords []string
	for _, word := range words {
		// Check if it looks like a path
		if isPathLike(word) && isValidPath(word) {
			paths = append(paths, word)
		} else {
			nonPathWords = append(nonPathWords, word)
		}
	}

	// If we found paths in the unquoted words, update remaining
	if len(paths) > 0 && len(nonPathWords) < len(words) {
		remaining = strings.Join(nonPathWords, " ")
	}

	// Clean up remaining text
	remaining = strings.TrimSpace(remaining)

	return paths, remaining
}

// isPathLike checks if a string looks like it could be a file path
func isPathLike(s string) bool {
	// Unix/Mac absolute paths
	if strings.HasPrefix(s, "/") {
		return true
	}
	// Home directory paths
	if strings.HasPrefix(s, "~/") {
		return true
	}
	// Relative paths
	if strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../") {
		return true
	}
	// Windows paths (C:\, D:\, etc.)
	if len(s) > 2 && s[1] == ':' && (s[2] == '\\' || s[2] == '/') {
		return true
	}
	return false
}

// isValidPath checks if a path exists and is accessible
func isValidPath(path string) bool {
	// Expand home directory if needed
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return false
		}
		path = filepath.Join(home, path[2:])
	}

	// Check if the path exists
	_, err := os.Stat(path)
	return err == nil
}

// getFileInfo returns a formatted string with filename and size
func getFileInfo(path string) string {
	// Expand home directory if needed
	originalPath := path
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, path[2:])
	}

	info, err := os.Stat(path)
	if err != nil {
		return fmt.Sprintf("%s <unknown>", filepath.Base(originalPath))
	}

	// Get the base filename
	filename := filepath.Base(path)

	// Format size
	size := info.Size()
	var sizeStr string
	switch {
	case size < 1024:
		sizeStr = fmt.Sprintf("%dB", size)
	case size < 1024*1024:
		sizeStr = fmt.Sprintf("%.1fKB", float64(size)/1024)
	case size < 1024*1024*1024:
		sizeStr = fmt.Sprintf("%.1fMB", float64(size)/(1024*1024))
	default:
		sizeStr = fmt.Sprintf("%.1fGB", float64(size)/(1024*1024*1024))
	}

	return fmt.Sprintf("%s <%s>", filename, sizeStr)
}
