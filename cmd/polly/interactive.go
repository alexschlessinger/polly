package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
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

	// Configure readline
	rl, err := readline.NewEx(&readline.Config{
		Prompt:          "> ",
		HistoryFile:     getHistoryFilePath(contextID),
		AutoComplete:    createAutoCompleter(),
		InterruptPrompt: "^C",
		EOFPrompt:       "exit",

		HistorySearchFold:   true,
		FuncFilterInputRune: filterInput,
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

		// Handle special commands
		if strings.HasPrefix(input, "/") {
			if handled := handleInteractiveCommand(input, config, session, rl); handled {
				continue
			}
		}

		// Process the prompt as a regular message
		userMsg := messages.ChatMessage{
			Role:    messages.MessageRoleUser,
			Content: input,
		}

		// Add files if specified via command in the prompt (e.g., "/file path/to/file")
		if strings.Contains(input, "/file ") {
			// Parse and attach files
			files := parseFileReferences(input)
			if len(files) > 0 {
				var err error
				userMsg, err = buildMessageWithFiles(cleanPromptFromFileRefs(input), files)
				if err != nil {
					fmt.Println(errorStyle.Styled(fmt.Sprintf("Error processing files: %v", err)))
					continue
				}
			}
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
		// Clear the session history
		if fileSession, ok := session.(*sessions.FileSession); ok {
			fileSession.Clear()
			fmt.Println(successStyle.Styled("Conversation reset."))
		}
		return true

	case "/model", "/m":
		if len(parts) < 2 {
			fmt.Printf("Current model: %s\n", highlightStyle.Styled(config.Model))
			fmt.Println(dimStyle.Styled("Usage: /model <model-name>"))
			fmt.Println(dimStyle.Styled("Example: /model openai/gpt-4o"))
		} else {
			config.Model = parts[1]
			fmt.Println(successStyle.Styled(fmt.Sprintf("Switched to model: %s", config.Model)))
		}
		return true

	case "/temp", "/temperature":
		if len(parts) < 2 {
			fmt.Printf("Current temperature: %s\n", highlightStyle.Styled(fmt.Sprintf("%.2f", config.Temperature)))
			fmt.Println(dimStyle.Styled("Usage: /temp <0.0-2.0>"))
		} else {
			if temp, err := parseFloat(parts[1]); err == nil {
				config.Temperature = temp
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
			config.SystemPrompt = newPrompt
			fmt.Println(successStyle.Styled("System prompt updated."), dimStyle.Styled("Note: This will apply to new messages only."))
		}
		return true

	case "/debug":
		config.Debug = !config.Debug
		fmt.Println(successStyle.Styled(fmt.Sprintf("Debug mode: %v", config.Debug)))
		return true
	}

	// Unknown command
	if strings.HasPrefix(input, "/") {
		fmt.Println(errorStyle.Styled(fmt.Sprintf("Unknown command: %s", parts[0])), dimStyle.Styled("(use /help for available commands)"))
		return true
	}

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
	fmt.Println()
	fmt.Println(boldStyle.Styled("Interactive Mode Commands:"))
	fmt.Println(dimStyle.Styled("─────────────────────────"))

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
		{"/context", "Show current context"},
		{"/system <prompt>", "Update system prompt"},
		{"/debug", "Toggle debug mode"},
		{"/help", "Show this help message"},
	}

	for _, c := range commands {
		fmt.Printf("  %-18s %s\n", highlightStyle.Styled(c.cmd), c.desc)
	}

	fmt.Println()
	fmt.Println(boldStyle.Styled("Examples:"))
	fmt.Println(dimStyle.Styled("─────────"))
	fmt.Println(dimStyle.Styled("  /model openai/gpt-4o-mini"))
	fmt.Println(dimStyle.Styled("  /temp 0.7"))
	fmt.Println(dimStyle.Styled("  /save chat.txt"))
	fmt.Println()
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
		fmt.Fprintf(&builder, "%s\n\n", msg.Content)
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

// parseFileReferences extracts file paths from /file commands in the prompt
func parseFileReferences(input string) []string {
	var files []string
	lines := strings.SplitSeq(input, "\n")
	for line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "/file ") {
			parts := strings.Fields(line)
			if len(parts) > 1 {
				files = append(files, parts[1:]...)
			}
		}
	}
	return files
}

// cleanPromptFromFileRefs removes /file commands from the prompt
func cleanPromptFromFileRefs(input string) string {
	lines := strings.Split(input, "\n")
	var cleaned []string
	for _, line := range lines {
		if !strings.HasPrefix(strings.TrimSpace(line), "/file ") {
			cleaned = append(cleaned, line)
		}
	}
	return strings.Join(cleaned, "\n")
}

// parseFloat safely parses a string to float64
func parseFloat(s string) (float64, error) {
	var f float64
	_, err := fmt.Sscanf(s, "%f", &f)
	return f, err
}
