package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/chzyer/readline"
)

// readlineUI wraps the readline instance and ensures output plays nicely with the prompt.
type readlineUI struct {
	rl *readline.Instance
}

func newReadlineUI(rl *readline.Instance) *readlineUI {
	return &readlineUI{rl: rl}
}

func (ui *readlineUI) Println(text string) {
	if ui == nil || ui.rl == nil {
		fmt.Println(text)
		return
	}
	ui.rl.Clean()
	fmt.Println(text)
	ui.rl.Refresh()
}

func (ui *readlineUI) Printf(format string, args ...interface{}) {
	if ui == nil || ui.rl == nil {
		fmt.Printf(format, args...)
		return
	}
	ui.rl.Clean()
	fmt.Printf(format, args...)
	ui.rl.Refresh()
}

// runInteractiveMode runs the CLI in interactive mode with readline support

func runInteractiveMode(ctx context.Context, config *Config, session sessions.Session, multipass llm.LLM, toolRegistry *tools.ToolRegistry, contextID string) error {
	// Note: session is already initialized by caller, no need to close it here

	// Initialize colors based on terminal background
	initColors()

	listener := &fileAttachListener{
		session:        session,
		processedPaths: make(map[string]bool),
	}

	rl, err := readline.NewEx(&readline.Config{
		Prompt:          "> ",
		HistoryFile:     getHistoryFilePath(contextID),
		AutoComplete:    createAutoCompleter(toolRegistry),
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

	ui := newReadlineUI(rl)
	listener.ui = ui

	refreshInteractiveUI(config, session, toolRegistry, ui)

	// Print welcome message
	printWelcomeMessage(config, session, contextID, toolRegistry)

	// Show recent history if resuming an existing conversation
	hasHistory := showRecentHistory(session)
	if hasHistory {
		fmt.Println()
		fmt.Println(userStyle.Styled("─── Resuming context ───"))
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
			if handled := handleInteractiveCommand(input, config, session, &toolRegistry, ui); handled {
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
	_, exists := interactiveCommands[command]
	return exists
}

// commandHandler is a function that handles an interactive command
type commandHandler func(parts []string, config *Config, session sessions.Session, registry **tools.ToolRegistry, ui *readlineUI) bool

// interactiveCommands maps command names to their handlers
var interactiveCommands = map[string]commandHandler{
	"/exit":        handleExit,
	"/quit":        handleExit,
	"/q":           handleExit,
	"/clear":       handleClear,
	"/cls":         handleClear,
	"/reset":       handleReset,
	"/model":       handleModel,
	"/m":           handleModel,
	"/temp":        handleTemperature,
	"/temperature": handleTemperature,
	"/history":     handleHistory,
	"/h":           handleHistory,
	"/save":        handleSave,
	"/file":        handleFile,
	"/f":           handleFile,
	"/help":        handleHelp,
	"/?":           handleHelp,
	"/context":     handleContext,
	"/c":           handleContext,
	"/system":      handleSystem,
	"/sys":         handleSystem,
	"/debug":       handleDebug,
	"/description": handleDescription,
	"/desc":        handleDescription,
	"/maxhistory":  handleMaxHistory,
	"/ttl":         handleTTL,
	"/tools":       handleTools,
	"/think":       handleThinking,
	"/thinking":    handleThinking,
	"/maxtokens":   handleMaxTokens,
	"/tokens":      handleMaxTokens,
	"/tooltimeout": handleToolTimeout,
}

// handleInteractiveCommand processes special interactive commands
func handleInteractiveCommand(input string, config *Config, session sessions.Session, registry **tools.ToolRegistry, ui *readlineUI) bool {
	parts := strings.Fields(input)
	if len(parts) == 0 {
		return false
	}

	command := strings.ToLower(parts[0])
	if handler, ok := interactiveCommands[command]; ok {
		return handler(parts, config, session, registry, ui)
	}

	// Don't show "unknown command" for paths that start with /
	// Let them fall through to file path handling
	return false
}

// Command handlers
func handleExit(_ []string, _ *Config, _ sessions.Session, _ **tools.ToolRegistry, _ *readlineUI) bool {
	cleanupAndExit(0)
	return true
}

func handleClear(_ []string, _ *Config, _ sessions.Session, _ **tools.ToolRegistry, _ *readlineUI) bool {
	clearScreen()
	return true
}

func handleReset(_ []string, _ *Config, session sessions.Session, _ **tools.ToolRegistry, ui *readlineUI) bool {
	session.Clear()
	ui.Println(successStyle.Styled("Conversation reset."))
	return true
}

func handleModel(parts []string, config *Config, session sessions.Session, registry **tools.ToolRegistry, ui *readlineUI) bool {
	if len(parts) < 2 {
		ui.Printf("Current model: %s\n", highlightStyle.Styled(config.Model))
		ui.Println(dimStyle.Styled("Usage: /model <model-name>"))
		ui.Println(dimStyle.Styled("Example: /model openai/gpt-4o"))
	} else {
		config.Model = parts[1]
		if contextInfo := session.GetMetadata(); contextInfo != nil {
			contextInfo.Model = config.Model
			session.SetMetadata(contextInfo)
		}
		ui.Println(successStyle.Styled(fmt.Sprintf("Switched to model: %s", config.Model)))
		refreshInteractiveUI(config, session, *registry, ui)
	}
	return true
}

func handleTemperature(parts []string, config *Config, session sessions.Session, _ **tools.ToolRegistry, ui *readlineUI) bool {
	if len(parts) < 2 {
		ui.Printf("Current temperature: %s\n", highlightStyle.Styled(fmt.Sprintf("%.2f", config.Temperature)))
		ui.Println(dimStyle.Styled("Usage: /temp <0.0-2.0>"))
	} else {
		if temp, err := parseFloat(parts[1]); err == nil {
			if temp < 0.0 || temp > 2.0 {
				ui.Println(errorStyle.Styled(fmt.Sprintf("temperature must be between 0.0 and 2.0, got %.1f", temp)))
				return true
			}
			config.Temperature = temp
			if contextInfo := session.GetMetadata(); contextInfo != nil {
				contextInfo.Temperature = temp
				session.SetMetadata(contextInfo)
			}
			ui.Println(successStyle.Styled(fmt.Sprintf("Temperature set to: %.2f", config.Temperature)))
		} else {
			ui.Println(errorStyle.Styled("Invalid temperature value"))
		}
	}
	return true
}

func handleHistory(_ []string, _ *Config, session sessions.Session, _ **tools.ToolRegistry, ui *readlineUI) bool {
	showHistory(session, ui)
	return true
}

func handleSave(parts []string, _ *Config, session sessions.Session, _ **tools.ToolRegistry, ui *readlineUI) bool {
	if len(parts) < 2 {
		ui.Println(dimStyle.Styled("Usage: /save <filename>"))
		return true
	}

	saveConversation(session, parts[1], ui)
	return true
}

func handleFile(parts []string, _ *Config, session sessions.Session, _ **tools.ToolRegistry, ui *readlineUI) bool {
	if len(parts) < 2 {
		ui.Println(dimStyle.Styled("Usage: /file <path>"))
		return true
	}

	filePath := strings.Join(parts[1:], " ")
	if strings.HasPrefix(filePath, "~/") {
		home, _ := os.UserHomeDir()
		filePath = filepath.Join(home, filePath[2:])
	}

	if _, err := os.Stat(filePath); err != nil {
		ui.Println(errorStyle.Styled(fmt.Sprintf("File not found: %s", filePath)))
		return true
	}

	userMsg, err := buildMessageWithFiles("", []string{filePath})
	if err != nil {
		ui.Println(errorStyle.Styled(fmt.Sprintf("Error processing file: %v", err)))
		return true
	}

	session.AddMessage(userMsg)
	fileInfo := getFileInfo(filePath)
	ui.Println(dimStyle.Styled(fmt.Sprintf("📎 Attached: %s", fileInfo)))
	return true
}

func handleHelp(_ []string, _ *Config, _ sessions.Session, _ **tools.ToolRegistry, ui *readlineUI) bool {
	printInteractiveHelp(ui)
	return true
}

func handleContext(parts []string, _ *Config, session sessions.Session, _ **tools.ToolRegistry, ui *readlineUI) bool {
	if len(parts) < 2 {
		ui.Printf("Current context: %s\n", highlightStyle.Styled(getContextDisplayName(session)))
		return true
	}

	ui.Println(dimStyle.Styled("To switch context, exit and restart with -c flag"))
	return true
}

func handleSystem(parts []string, config *Config, session sessions.Session, _ **tools.ToolRegistry, ui *readlineUI) bool {
	if len(parts) < 2 {
		ui.Printf("Current system prompt: %s\n", highlightStyle.Styled(config.SystemPrompt))
		return true
	}

	newPrompt := strings.Join(parts[1:], " ")
	config.SystemPrompt = newPrompt
	if contextInfo := session.GetMetadata(); contextInfo != nil {
		contextInfo.SystemPrompt = newPrompt
		session.SetMetadata(contextInfo)
	}
	session.Clear()
	ui.Println(successStyle.Styled("System prompt updated and conversation reset."))
	return true
}

func handleDebug(_ []string, config *Config, _ sessions.Session, _ **tools.ToolRegistry, ui *readlineUI) bool {
	config.Debug = !config.Debug
	ui.Println(successStyle.Styled(fmt.Sprintf("Debug mode: %v", config.Debug)))
	return true
}

func handleDescription(parts []string, _ *Config, session sessions.Session, _ **tools.ToolRegistry, ui *readlineUI) bool {
	contextInfo := session.GetMetadata()
	if contextInfo == nil {
		ui.Println(errorStyle.Styled("No context available. Create or switch to a context first."))
		return true
	}

	if len(parts) < 2 {
		if contextInfo.Description != "" {
			ui.Printf("Current description: %s\n", highlightStyle.Styled(contextInfo.Description))
		} else {
			ui.Println(dimStyle.Styled("No description set"))
		}
		ui.Println(dimStyle.Styled("Usage: /description <text>"))
		ui.Println(dimStyle.Styled("       /description clear  (to remove)"))
		return true
	}

	if parts[1] == "clear" {
		contextInfo.Description = ""
		session.SetMetadata(contextInfo)
		ui.Println(successStyle.Styled("Description cleared"))
		return true
	}

	contextInfo.Description = strings.Join(parts[1:], " ")
	session.SetMetadata(contextInfo)
	ui.Println(successStyle.Styled(fmt.Sprintf("Description set: %s", contextInfo.Description)))
	return true
}

func handleMaxHistory(parts []string, _ *Config, session sessions.Session, _ **tools.ToolRegistry, ui *readlineUI) bool {
	contextInfo := session.GetMetadata()
	if contextInfo == nil {
		ui.Println(errorStyle.Styled("No context available. Create or switch to a context first."))
		return true
	}

	if len(parts) < 2 {
		if contextInfo.MaxHistory > 0 {
			ui.Printf("Current max history: %s\n", highlightStyle.Styled(fmt.Sprintf("%d messages", contextInfo.MaxHistory)))
		} else {
			ui.Println(dimStyle.Styled("No max history limit set (unlimited)"))
		}
		ui.Println(dimStyle.Styled("Usage: /maxhistory <number>"))
		ui.Println(dimStyle.Styled("       /maxhistory 0  (for unlimited)"))
		return true
	}

	val, err := parseInt(parts[1])
	if err != nil {
		ui.Println(errorStyle.Styled("Invalid number"))
		return true
	}
	if val < 0 {
		ui.Println(errorStyle.Styled("Max history must be 0 (unlimited) or positive"))
		return true
	}

	contextInfo.MaxHistory = val
	session.SetMetadata(contextInfo)
	if val == 0 {
		ui.Println(successStyle.Styled("Max history set to unlimited"))
	} else {
		ui.Println(successStyle.Styled(fmt.Sprintf("Max history set to: %d messages", val)))
	}
	return true
}

func handleTTL(parts []string, _ *Config, session sessions.Session, _ **tools.ToolRegistry, ui *readlineUI) bool {
	contextInfo := session.GetMetadata()
	if contextInfo == nil {
		ui.Println(errorStyle.Styled("No context available. Create or switch to a context first."))
		return true
	}

	if len(parts) < 2 {
		if contextInfo.TTL > 0 {
			ui.Printf("Current TTL: %s\n", highlightStyle.Styled(contextInfo.TTL.String()))
		} else {
			ui.Println(dimStyle.Styled("No TTL set (never expires)"))
		}
		ui.Println(dimStyle.Styled("Usage: /ttl <duration>"))
		ui.Println(dimStyle.Styled("Examples: /ttl 24h, /ttl 7d, /ttl 30m"))
		ui.Println(dimStyle.Styled("          /ttl 0  (never expires)"))
		return true
	}

	if parts[1] == "0" {
		contextInfo.TTL = 0
		session.SetMetadata(contextInfo)
		ui.Println(successStyle.Styled("TTL cleared (context never expires)"))
		return true
	}

	if duration, err := time.ParseDuration(parts[1]); err == nil {
		contextInfo.TTL = duration
		session.SetMetadata(contextInfo)
		ui.Println(successStyle.Styled(fmt.Sprintf("TTL set to: %s", duration)))
		return true
	}

	if strings.HasSuffix(parts[1], "d") {
		daysStr := strings.TrimSuffix(parts[1], "d")
		days, err := parseInt(daysStr)
		if err != nil {
			ui.Println(errorStyle.Styled("Invalid duration format"))
			return true
		}
		duration := time.Duration(days) * 24 * time.Hour
		contextInfo.TTL = duration
		session.SetMetadata(contextInfo)
		ui.Println(successStyle.Styled(fmt.Sprintf("TTL set to: %s", duration)))
		return true
	}

	ui.Println(errorStyle.Styled("Invalid duration format"))
	return true
}

func handleTools(parts []string, config *Config, session sessions.Session, registry **tools.ToolRegistry, ui *readlineUI) bool {
	contextInfo := session.GetMetadata()
	if contextInfo == nil {
		ui.Println(errorStyle.Styled("No context available. Create or switch to a context first."))
		return true
	}

	current := *registry
	needsRegistry := len(parts) >= 2 && parts[1] != "list"
	if needsRegistry && current == nil {
		ui.Println(errorStyle.Styled("Tool registry not available. Please restart the session."))
		return true
	}

	if len(parts) < 2 || parts[1] == "list" {
		if current == nil || len(current.All()) == 0 {
			ui.Println(userStyle.Styled("No tools currently loaded"))
			refreshInteractiveUI(config, session, current, ui)
			return true
		}

		ui.Println("Currently loaded tools:")
		for _, tool := range current.All() {
			displayType := tool.GetType()
			switch displayType {
			case "shell":
				displayType = "Shell"
			case "mcp":
				displayType = "MCP"
			case "native":
				displayType = "Native"
			}
			ui.Printf("  - %s [%s]\n", highlightStyle.Styled(tool.GetName()), dimStyle.Styled(displayType))
		}
		refreshInteractiveUI(config, session, current, ui)
		return true
	}

	switch parts[1] {
	case "add":
		if len(parts) < 3 {
			ui.Println(errorStyle.Styled("Usage: /tools add <path|server>"))
			return true
		}
		if current == nil {
			current = tools.NewToolRegistry(nil)
			*registry = current
		}

		pathOrServer := strings.Join(parts[2:], " ")
		toolsBefore := make(map[string]bool)
		for _, tool := range current.All() {
			toolsBefore[tool.GetName()] = true
		}

		if _, err := current.LoadToolAuto(pathOrServer); err != nil {
			ui.Println(errorStyle.Styled(fmt.Sprintf("Failed to load tool: %v", err)))
			return true
		}

		var newTools []string
		for _, tool := range current.All() {
			name := tool.GetName()
			if !toolsBefore[name] {
				newTools = append(newTools, name)
			}
		}

		contextInfo.ActiveTools = current.GetActiveToolLoaders()
		session.SetMetadata(contextInfo)

		if len(newTools) > 0 {
			ui.Println(successStyle.Styled(fmt.Sprintf("Loaded tools: %s", strings.Join(newTools, ", "))))
			refreshInteractiveUI(config, session, current, ui)
		} else {
			ui.Println(dimStyle.Styled("No new tools were loaded"))
		}

		session.AddMessage(messages.ChatMessage{
			Role:    messages.MessageRoleAssistant,
			Content: "My available tools have been updated.",
		})

	case "remove":
		if len(parts) < 3 {
			ui.Println(errorStyle.Styled("Usage: /tools remove <name or pattern>"))
			return true
		}

		pattern := strings.Join(parts[2:], " ")
		if strings.Contains(pattern, "*") {
			var removed []string
			for _, tool := range current.All() {
				name := tool.GetName()
				matched, _ := path.Match(pattern, name)
				if matched {
					current.Remove(name)
					removed = append(removed, name)
				}
			}

			if len(removed) == 0 {
				ui.Println(errorStyle.Styled(fmt.Sprintf("No tools matched pattern: %s", pattern)))
				return true
			}

			var newLoaders []tools.ToolLoaderInfo
			for _, loader := range contextInfo.ActiveTools {
				keep := true
				for _, removedName := range removed {
					if loader.Name == removedName {
						keep = false
						break
					}
				}
				if keep {
					newLoaders = append(newLoaders, loader)
				}
			}
			contextInfo.ActiveTools = newLoaders
			session.SetMetadata(contextInfo)

			ui.Println(successStyle.Styled(fmt.Sprintf("Removed %d tools: %s", len(removed), strings.Join(removed, ", "))))
			refreshInteractiveUI(config, session, current, ui)

			session.AddMessage(messages.ChatMessage{
				Role:    messages.MessageRoleAssistant,
				Content: "My available tools have been updated.",
			})
			return true
		}

		if _, exists := current.Get(pattern); !exists {
			ui.Println(errorStyle.Styled(fmt.Sprintf("Tool not found: %s", pattern)))
			return true
		}

		current.Remove(pattern)
		var newLoaders []tools.ToolLoaderInfo
		for _, loader := range contextInfo.ActiveTools {
			if loader.Name != pattern {
				newLoaders = append(newLoaders, loader)
			}
		}
		contextInfo.ActiveTools = newLoaders
		session.SetMetadata(contextInfo)

		ui.Println(successStyle.Styled(fmt.Sprintf("Removed tool: %s", pattern)))
		refreshInteractiveUI(config, session, current, ui)

		session.AddMessage(messages.ChatMessage{
			Role:    messages.MessageRoleAssistant,
			Content: "My available tools have been updated.",
		})

	case "reload":
		ui.Println("Reloading all tools...")
		if current != nil {
			if err := current.Close(); err != nil {
				log.Printf("Error closing registry: %v", err)
			}
		}

		newRegistry, err := loadTools(contextInfo.ActiveTools)
		if err != nil {
			ui.Println(errorStyle.Styled(fmt.Sprintf("Failed to reload tools: %v", err)))
			return true
		}

		*registry = newRegistry
		current = newRegistry
		ui.Println(successStyle.Styled("All tools reloaded"))
		refreshInteractiveUI(config, session, current, ui)

		session.AddMessage(messages.ChatMessage{
			Role:    messages.MessageRoleAssistant,
			Content: "My available tools have been updated.",
		})

	default:
		ui.Println(errorStyle.Styled(fmt.Sprintf("Unknown subcommand: %s", parts[1])))
		ui.Println(userStyle.Styled("Usage: /tools [list]              - List all loaded tools"))
		ui.Println(userStyle.Styled("       /tools add <path|server>   - Add a tool"))
		ui.Println(userStyle.Styled("       /tools remove <name|pattern> - Remove tool(s)"))
		ui.Println(userStyle.Styled("       /tools reload              - Reload all tools"))
		return true
	}

	return true
}

func handleThinking(parts []string, config *Config, session sessions.Session, registry **tools.ToolRegistry, ui *readlineUI) bool {
	contextInfo := session.GetMetadata()
	if contextInfo == nil {
		ui.Println(errorStyle.Styled("No context available. Create or switch to a context first."))
		return true
	}

	if len(parts) < 2 {
		if contextInfo.ThinkingEffort != "off" && contextInfo.ThinkingEffort != "" {
			ui.Printf("Current thinking effort: %s\n", highlightStyle.Styled(contextInfo.ThinkingEffort))
		} else if config.ThinkingEffort != "off" {
			ui.Printf("Current thinking effort: %s\n", highlightStyle.Styled(config.ThinkingEffort))
		} else {
			ui.Printf("Current thinking effort: %s\n", highlightStyle.Styled("off"))
		}
		ui.Println(dimStyle.Styled("Usage: /think <level>"))
		ui.Println(dimStyle.Styled("Levels: off, low, medium, high"))
		ui.Println(dimStyle.Styled("       /think off  (to disable)"))
		return true
	}

	effort := strings.ToLower(parts[1])
	validEfforts := map[string]bool{"off": true, "low": true, "medium": true, "high": true}
	if !validEfforts[effort] {
		ui.Println(errorStyle.Styled("Invalid thinking effort. Use: off, low, medium, or high"))
		return true
	}

	contextInfo.ThinkingEffort = effort
	config.ThinkingEffort = effort
	session.SetMetadata(contextInfo)

	if effort == "off" {
		ui.Println(successStyle.Styled("Thinking disabled"))
	} else {
		ui.Println(successStyle.Styled(fmt.Sprintf("Thinking effort set to: %s", effort)))
	}
	refreshInteractiveUI(config, session, *registry, ui)
	return true
}

func handleMaxTokens(parts []string, config *Config, session sessions.Session, registry **tools.ToolRegistry, ui *readlineUI) bool {
	if len(parts) < 2 {
		ui.Printf("Current max tokens: %s\n", highlightStyle.Styled(fmt.Sprintf("%d", config.MaxTokens)))
		ui.Println(dimStyle.Styled("Usage: /maxtokens <number>"))
		ui.Println(dimStyle.Styled("Example: /maxtokens 4096"))
		return true
	}

	tokens, err := parseInt(parts[1])
	if err != nil || tokens <= 0 {
		ui.Println(errorStyle.Styled("Max tokens must be a positive number"))
		return true
	}

	config.MaxTokens = tokens
	if contextInfo := session.GetMetadata(); contextInfo != nil {
		contextInfo.MaxTokens = tokens
		session.SetMetadata(contextInfo)
	}
	ui.Println(successStyle.Styled(fmt.Sprintf("Max tokens set to: %d", tokens)))
	refreshInteractiveUI(config, session, *registry, ui)
	return true
}

func handleToolTimeout(parts []string, config *Config, session sessions.Session, registry **tools.ToolRegistry, ui *readlineUI) bool {
	if len(parts) < 2 {
		if config.ToolTimeout > 0 {
			ui.Printf("Current tool timeout: %s\n", highlightStyle.Styled(config.ToolTimeout.String()))
		} else {
			ui.Println(dimStyle.Styled("No tool timeout set (using default)"))
		}
		ui.Println(dimStyle.Styled("Usage: /tooltimeout <duration>"))
		ui.Println(dimStyle.Styled("Examples: /tooltimeout 30s, /tooltimeout 2m, /tooltimeout 0 (no timeout)"))
		return true
	}

	if parts[1] == "0" {
		config.ToolTimeout = 0
		if contextInfo := session.GetMetadata(); contextInfo != nil {
			contextInfo.ToolTimeout = 0
			session.SetMetadata(contextInfo)
		}
		ui.Println(successStyle.Styled("Tool timeout disabled"))
		refreshInteractiveUI(config, session, *registry, ui)
		return true
	}

	duration, err := time.ParseDuration(parts[1])
	if err != nil || duration < 0 {
		ui.Println(errorStyle.Styled("Invalid duration format"))
		return true
	}

	config.ToolTimeout = duration
	if contextInfo := session.GetMetadata(); contextInfo != nil {
		contextInfo.ToolTimeout = duration
		session.SetMetadata(contextInfo)
	}
	ui.Println(successStyle.Styled(fmt.Sprintf("Tool timeout set to: %s", duration)))
	refreshInteractiveUI(config, session, *registry, ui)
	return true
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
func createAutoCompleter(registry *tools.ToolRegistry) *readline.PrefixCompleter {
	var toolItems []readline.PrefixCompleterInterface
	if registry != nil {
		for _, t := range registry.All() {
			toolItems = append(toolItems, readline.PcItem(t.GetName()))
		}
	}

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
		readline.PcItem("/maxtokens",
			readline.PcItem("1024"),
			readline.PcItem("2048"),
			readline.PcItem("4096"),
			readline.PcItem("8192"),
			readline.PcItem("16384"),
		),
		readline.PcItem("/tokens",
			readline.PcItem("1024"),
			readline.PcItem("2048"),
			readline.PcItem("4096"),
			readline.PcItem("8192"),
			readline.PcItem("16384"),
		),
		readline.PcItem("/history"),
		readline.PcItem("/save"),
		readline.PcItem("/help"),
		readline.PcItem("/context"),
		readline.PcItem("/system"),
		readline.PcItem("/description",
			readline.PcItem("clear"),
		),
		readline.PcItem("/desc",
			readline.PcItem("clear"),
		),
		readline.PcItem("/maxhistory"),
		readline.PcItem("/ttl",
			readline.PcItem("24h"),
			readline.PcItem("7d"),
			readline.PcItem("30d"),
			readline.PcItem("0"),
		),
		readline.PcItem("/think",
			readline.PcItem("off"),
			readline.PcItem("low"),
			readline.PcItem("medium"),
			readline.PcItem("high"),
		),
		readline.PcItem("/thinking",
			readline.PcItem("off"),
			readline.PcItem("low"),
			readline.PcItem("medium"),
			readline.PcItem("high"),
		),
		readline.PcItem("/tooltimeout",
			readline.PcItem("30s"),
			readline.PcItem("1m"),
			readline.PcItem("2m"),
			readline.PcItem("5m"),
			readline.PcItem("0"),
		),
		readline.PcItem("/tools",
			readline.PcItem("list"),
			readline.PcItem("add"),
			readline.PcItem("remove", toolItems...),
			readline.PcItem("reload"),
			readline.PcItem("mcp",
				readline.PcItem("list"),
				readline.PcItem("remove"),
			),
		),
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
func printWelcomeMessage(config *Config, session sessions.Session, contextID string, toolRegistry *tools.ToolRegistry) {
	// Show all configuration
	fmt.Printf("Model: %s\n", highlightStyle.Styled(config.Model))
	if contextID != "" {
		fmt.Printf("Context: %s\n", highlightStyle.Styled(contextID))
	}
	fmt.Printf("Temperature: %s\n", highlightStyle.Styled(fmt.Sprintf("%.1f", config.Temperature)))
	fmt.Printf("Max Tokens: %s\n", highlightStyle.Styled(fmt.Sprintf("%d", config.MaxTokens)))

	// Get context info from session for additional details
	if contextInfo := session.GetMetadata(); contextInfo != nil {
		// Show system prompt if set
		if contextInfo.SystemPrompt != "" {
			// Truncate long system prompts for display
			prompt := contextInfo.SystemPrompt
			fmt.Printf("System Prompt: %s\n", highlightStyle.Styled(prompt))
		} else if config.SystemPrompt != "" {
			// Fall back to config system prompt
			prompt := config.SystemPrompt
			if len(prompt) > 60 {
				prompt = prompt[:57] + "..."
			}
			fmt.Printf("System Prompt: %s\n", highlightStyle.Styled(prompt))
		}

		// Show max history if set
		if contextInfo.MaxHistory > 0 {
			fmt.Printf("Max History: %s\n", highlightStyle.Styled(fmt.Sprintf("%d messages", contextInfo.MaxHistory)))
		}

		// Show description if set
		if contextInfo.Description != "" {
			desc := contextInfo.Description
			if len(desc) > 60 {
				desc = desc[:57] + "..."
			}
			fmt.Printf("Description: %s\n", highlightStyle.Styled(desc))
		}

		// Show TTL if set
		if contextInfo.TTL > 0 {
			fmt.Printf("TTL: %s\n", highlightStyle.Styled(contextInfo.TTL.String()))
		}

		// Show thinking effort if set
		if contextInfo.ThinkingEffort != "off" && contextInfo.ThinkingEffort != "" {
			fmt.Printf("Thinking: %s\n", highlightStyle.Styled(contextInfo.ThinkingEffort))
		}
	} else {
		// No context info, show from config if available
		if config.SystemPrompt != "" {
			prompt := config.SystemPrompt
			if len(prompt) > 60 {
				prompt = prompt[:57] + "..."
			}
			fmt.Printf("System Prompt: %s\n", highlightStyle.Styled(prompt))
		}
		if config.ThinkingEffort != "off" {
			fmt.Printf("Thinking: %s\n", highlightStyle.Styled(config.ThinkingEffort))
		}
	}

	// Show all loaded tools
	if toolRegistry != nil {
		// Get all tools directly for display
		allTools := toolRegistry.All()
		if len(allTools) > 0 {
			fmt.Print("Tools: ")
			toolNames := []string{}
			for _, tool := range allTools {
				// Determine tool type display
				toolType := ""
				switch tool.GetType() {
				case "shell":
					toolType = " [Shell]"
				case "mcp":
					toolType = " [MCP]"
				case "native":
					toolType = " [Native]"
				}
				toolNames = append(toolNames, tool.GetName()+toolType)
			}
			fmt.Println(highlightStyle.Styled(strings.Join(toolNames, ", ")))
		}
	}

	fmt.Println()
}

// refreshInteractiveUI updates the prompt and autocomplete based on current state
func refreshInteractiveUI(config *Config, session sessions.Session, registry *tools.ToolRegistry, ui *readlineUI) {
	if ui == nil || ui.rl == nil {
		return
	}
	ui.rl.SetPrompt(makePrompt(config, session, registry))
	ui.rl.Config.AutoComplete = createAutoCompleter(registry)
	ui.rl.Refresh()
}

// makePrompt builds a dynamic, styled prompt
func makePrompt(config *Config, session sessions.Session, registry *tools.ToolRegistry) string {
	ctxName := getContextDisplayName(session)

	// Abbreviate model by removing provider prefix (provider/model -> model)
	displayModel := config.Model
	if idx := strings.Index(displayModel, "/"); idx != -1 && idx+1 < len(displayModel) {
		displayModel = displayModel[idx+1:]
	}
	if len(displayModel) > 24 {
		displayModel = displayModel[:21] + "..."
	}

	toolCount := 0
	if registry != nil {
		toolCount = len(registry.All())
	}

	// Build prompt pieces; omit context when it is "default"
	base := userStyle.Styled("polly")
	parts := []string{base}
	if ctxName != "default" {
		parts = append(parts, highlightStyle.Styled(ctxName))
	}
	parts = append(parts, highlightStyle.Styled(displayModel))
	parts = append(parts, fmt.Sprintf("%d tools", toolCount))

	return strings.Join(parts, " · ") + " > "
}

// printInteractiveHelp prints help for interactive commands
func printInteractiveHelp(ui *readlineUI) {
	commands := []struct {
		cmd  string
		desc string
	}{
		{"/exit, /quit", "Exit interactive mode"},
		{"/clear", "Clear the screen"},
		{"/reset", "Reset conversation history"},
		{"/model <name>", "Switch to a different model"},
		{"/temp <0.0-2.0>", "Set temperature"},
		{"/maxtokens <n>", "Set max tokens for response"},
		{"/history", "Show conversation history"},
		{"/save <file>", "Save conversation to file"},
		{"/file <path>", "Attach a file to the conversation"},
		{"/context", "Show current context"},
		{"/system <prompt>", "Update system prompt"},
		{"/description <text>", "Set context description"},
		{"/maxhistory <n>", "Set max history limit (0=unlimited)"},
		{"/ttl <duration>", "Set context TTL (e.g., 24h, 7d)"},
		{"/think <level>", "Set thinking effort (off/low/medium/high)"},
		{"/tooltimeout <duration>", "Set tool execution timeout"},
		{"/tools", "Manage all tools (list/add/remove/reload/mcp)"},
		{"/debug", "Toggle debug mode"},
		{"/help", "Show this help message"},
	}

	for _, c := range commands {
		ui.Printf("  %-20s %s\n", highlightStyle.Styled(c.cmd), c.desc)
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
			// Regular content with simple fenced code styling
			lines := strings.Split(msg.Content, "\n")
			inFence := false
			for _, line := range lines {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "```") {
					// Toggle fence; print fence lines dimmed
					inFence = !inFence
					fmt.Fprintf(&builder, "%s\n", dimStyle.Styled(line))
					continue
				}
				if inFence {
					// Faint gutter for code lines
					fmt.Fprintf(&builder, "%s%s\n", dimStyle.Styled("│ "), dimStyle.Styled(line))
				} else {
					fmt.Fprintf(&builder, "%s\n", line)
				}
			}
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

func showHistory(session sessions.Session, ui *readlineUI) {
	history := session.GetHistory()
	if len(history) == 0 {
		ui.Println(dimStyle.Styled("No conversation history."))
		return
	}

	formatted := formatConversation(history)
	ui.Printf("%s", formatted)
}

// saveConversation saves the conversation to a file
func saveConversation(session sessions.Session, filename string, ui *readlineUI) {
	history := session.GetHistory()
	if len(history) == 0 {
		ui.Println(dimStyle.Styled("No conversation to save."))
		return
	}

	formatted := formatConversation(history)
	err := os.WriteFile(filename, []byte(formatted), 0644)
	if err != nil {
		ui.Println(errorStyle.Styled(fmt.Sprintf("Error saving file: %v", err)))
		return
	}

	ui.Println(successStyle.Styled(fmt.Sprintf("Conversation saved to %s", filename)))
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

func parseInt(s string) (int, error) {
	return strconv.Atoi(s)
}

// fileAttachListener handles real-time file detection and attachment
type fileAttachListener struct {
	session        sessions.Session
	processedPaths map[string]bool // Track which paths we've already processed
	ui             *readlineUI
}

func (l *fileAttachListener) println(text string) {
	if l.ui != nil {
		l.ui.Println(text)
		return
	}
	fmt.Println(text)
}

// OnChange is called when the input line changes
func (l *fileAttachListener) OnChange(line []rune, pos int, key rune) (newLine []rune, newPos int, ok bool) {
	input := string(line)

	// Skip processing for commands
	if strings.HasPrefix(strings.TrimSpace(input), "/") {
		return line, pos, true
	}

	// Extract and process file paths
	paths, remaining := extractFilePaths(input)
	newPaths := l.filterNewPaths(paths)

	if len(newPaths) == 0 {
		return line, pos, true
	}

	// Attach new files
	if err := l.attachFiles(newPaths); err == nil {
		l.showAttachmentConfirmations(newPaths)
	}

	// Return remaining text without file paths
	newLine = []rune(remaining)
	newPos = min(pos, len(newLine))
	return newLine, newPos, true
}

// filterNewPaths returns paths that haven't been processed yet
func (l *fileAttachListener) filterNewPaths(paths []string) []string {
	var newPaths []string
	for _, path := range paths {
		if !l.processedPaths[path] {
			newPaths = append(newPaths, path)
			l.processedPaths[path] = true
		}
	}
	return newPaths
}

// attachFiles adds files to the session
func (l *fileAttachListener) attachFiles(paths []string) error {
	userMsg, err := buildMessageWithFiles("", paths)
	if err != nil {
		return err
	}
	l.session.AddMessage(userMsg)
	return nil
}

// showAttachmentConfirmations displays confirmation messages for attached files
func (l *fileAttachListener) showAttachmentConfirmations(paths []string) {
	if len(paths) <= 2 {
		for _, path := range paths {
			fileInfo := getFileInfo(path)
			l.println(dimStyle.Styled(fmt.Sprintf("📎 Attached: %s", fileInfo)))
		}
		return
	}

	// Summarize many attachments on a single line
	infos := make([]string, 0, len(paths))
	for i, p := range paths {
		if i < 2 {
			infos = append(infos, getFileInfo(p))
		} else {
			break
		}
	}
	more := len(paths) - 2
	summary := fmt.Sprintf("📎 %d files attached (%s, … and %d more)", len(paths), strings.Join(infos, ", "), more)
	l.println(dimStyle.Styled(summary))
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
