package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
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

var RL *readline.Instance

func safePrintln(text string) {
	RL.Clean()
	fmt.Println(text)
	RL.Refresh()
}

func safePrintf(format string, args ...interface{}) {
	RL.Clean()
	fmt.Printf(format, args...)
	RL.Refresh()
}

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
	RL = rl
	defer rl.Close()

	// Print welcome message
	printWelcomeMessage(config, session, contextID)

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
	_, exists := interactiveCommands[command]
	return exists
}

// commandHandler is a function that handles an interactive command
type commandHandler func(parts []string, config *Config, session sessions.Session, rl *readline.Instance) bool

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
	"/mcp":         handleMCP,
	"/think":       handleThinking,
	"/thinking":    handleThinking,
	"/maxtokens":   handleMaxTokens,
	"/tokens":      handleMaxTokens,
	"/tooltimeout": handleToolTimeout,
}

// handleInteractiveCommand processes special interactive commands
func handleInteractiveCommand(input string, config *Config, session sessions.Session, rl *readline.Instance) bool {
	parts := strings.Fields(input)
	if len(parts) == 0 {
		return false
	}

	command := strings.ToLower(parts[0])
	if handler, ok := interactiveCommands[command]; ok {
		return handler(parts, config, session, rl)
	}

	// Don't show "unknown command" for paths that start with /
	// Let them fall through to file path handling
	return false
}

// Command handlers
func handleExit(parts []string, config *Config, session sessions.Session, rl *readline.Instance) bool {
	cleanupAndExit(0)
	return true
}

func handleClear(parts []string, config *Config, session sessions.Session, rl *readline.Instance) bool {
	clearScreen()
	return true
}

func handleReset(parts []string, config *Config, session sessions.Session, rl *readline.Instance) bool {
	session.Clear()
	fmt.Println(successStyle.Styled("Conversation reset."))
	return true
}

func handleModel(parts []string, config *Config, session sessions.Session, rl *readline.Instance) bool {
	if len(parts) < 2 {
		fmt.Printf("Current model: %s\n", highlightStyle.Styled(config.Model))
		fmt.Println(dimStyle.Styled("Usage: /model <model-name>"))
		fmt.Println(dimStyle.Styled("Example: /model openai/gpt-4o"))
	} else {
		config.Model = parts[1]
		if contextInfo := session.GetMetadata(); contextInfo != nil {
			contextInfo.Model = config.Model
			session.SetMetadata(contextInfo)
		}
		fmt.Println(successStyle.Styled(fmt.Sprintf("Switched to model: %s", config.Model)))
	}
	return true
}

func handleTemperature(parts []string, config *Config, session sessions.Session, rl *readline.Instance) bool {
	if len(parts) < 2 {
		fmt.Printf("Current temperature: %s\n", highlightStyle.Styled(fmt.Sprintf("%.2f", config.Temperature)))
		fmt.Println(dimStyle.Styled("Usage: /temp <0.0-2.0>"))
	} else {
		if temp, err := parseFloat(parts[1]); err == nil {
			if temp < 0.0 || temp > 2.0 {
				fmt.Println(errorStyle.Styled(fmt.Sprintf("temperature must be between 0.0 and 2.0, got %.1f", temp)))
				return true
			}
			config.Temperature = temp
			if contextInfo := session.GetMetadata(); contextInfo != nil {
				contextInfo.Temperature = temp
				session.SetMetadata(contextInfo)
			}
			fmt.Println(successStyle.Styled(fmt.Sprintf("Temperature set to: %.2f", config.Temperature)))
		} else {
			fmt.Println(errorStyle.Styled("Invalid temperature value"))
		}
	}
	return true
}

func handleHistory(parts []string, config *Config, session sessions.Session, rl *readline.Instance) bool {
	showHistory(session)
	return true
}

func handleSave(parts []string, config *Config, session sessions.Session, rl *readline.Instance) bool {
	if len(parts) < 2 {
		fmt.Println(dimStyle.Styled("Usage: /save <filename>"))
	} else {
		saveConversation(session, parts[1])
	}
	return true
}

func handleFile(parts []string, config *Config, session sessions.Session, rl *readline.Instance) bool {
	if len(parts) < 2 {
		fmt.Println(dimStyle.Styled("Usage: /file <path>"))
		return true
	}

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
	return true
}

func handleHelp(parts []string, config *Config, session sessions.Session, rl *readline.Instance) bool {
	printInteractiveHelp()
	return true
}

func handleContext(parts []string, config *Config, session sessions.Session, rl *readline.Instance) bool {
	if len(parts) < 2 {
		fmt.Printf("Current context: %s\n", highlightStyle.Styled(getContextDisplayName(session)))
	} else {
		fmt.Println(dimStyle.Styled("To switch context, exit and restart with -c flag"))
	}
	return true
}

func handleSystem(parts []string, config *Config, session sessions.Session, rl *readline.Instance) bool {
	if len(parts) < 2 {
		fmt.Printf("Current system prompt: %s\n", highlightStyle.Styled(config.SystemPrompt))
	} else {
		// Join all parts after the command as the new system prompt
		newPrompt := strings.Join(parts[1:], " ")
		// Update config and session metadata
		config.SystemPrompt = newPrompt
		if contextInfo := session.GetMetadata(); contextInfo != nil {
			contextInfo.SystemPrompt = newPrompt
			session.SetMetadata(contextInfo)
		}
		// Reset conversation history to apply new system prompt
		session.Clear()
		fmt.Println(successStyle.Styled("System prompt updated and conversation reset."))
	}
	return true
}

func handleDebug(parts []string, config *Config, session sessions.Session, rl *readline.Instance) bool {
	config.Debug = !config.Debug
	fmt.Println(successStyle.Styled(fmt.Sprintf("Debug mode: %v", config.Debug)))
	return true
}

func handleDescription(parts []string, config *Config, session sessions.Session, rl *readline.Instance) bool {
	contextInfo := session.GetMetadata()
	if contextInfo == nil {
		fmt.Println(errorStyle.Styled("No context available. Create or switch to a context first."))
		return true
	}

	if len(parts) < 2 {
		if contextInfo.Description != "" {
			fmt.Printf("Current description: %s\n", highlightStyle.Styled(contextInfo.Description))
		} else {
			fmt.Println(dimStyle.Styled("No description set"))
		}
		fmt.Println(dimStyle.Styled("Usage: /description <text>"))
		fmt.Println(dimStyle.Styled("       /description clear  (to remove)"))
	} else {
		if parts[1] == "clear" {
			contextInfo.Description = ""
			session.SetMetadata(contextInfo)
			fmt.Println(successStyle.Styled("Description cleared"))
		} else {
			// Join all parts after the command as the description
			contextInfo.Description = strings.Join(parts[1:], " ")
			session.SetMetadata(contextInfo)
			fmt.Println(successStyle.Styled(fmt.Sprintf("Description set: %s", contextInfo.Description)))
		}
	}
	return true
}

func handleMaxHistory(parts []string, config *Config, session sessions.Session, rl *readline.Instance) bool {
	contextInfo := session.GetMetadata()
	if contextInfo == nil {
		fmt.Println(errorStyle.Styled("No context available. Create or switch to a context first."))
		return true
	}

	if len(parts) < 2 {
		if contextInfo.MaxHistory > 0 {
			fmt.Printf("Current max history: %s\n", highlightStyle.Styled(fmt.Sprintf("%d messages", contextInfo.MaxHistory)))
		} else {
			fmt.Println(dimStyle.Styled("No max history limit set (unlimited)"))
		}
		fmt.Println(dimStyle.Styled("Usage: /maxhistory <number>"))
		fmt.Println(dimStyle.Styled("       /maxhistory 0  (for unlimited)"))
	} else {
		if val, err := parseInt(parts[1]); err == nil {
			if val < 0 {
				fmt.Println(errorStyle.Styled("Max history must be 0 (unlimited) or positive"))
				return true
			}
			contextInfo.MaxHistory = val
			session.SetMetadata(contextInfo)
			if val == 0 {
				fmt.Println(successStyle.Styled("Max history set to unlimited"))
			} else {
				fmt.Println(successStyle.Styled(fmt.Sprintf("Max history set to: %d messages", val)))
			}
		} else {
			fmt.Println(errorStyle.Styled("Invalid number"))
		}
	}
	return true
}

func handleTTL(parts []string, config *Config, session sessions.Session, rl *readline.Instance) bool {
	contextInfo := session.GetMetadata()
	if contextInfo == nil {
		fmt.Println(errorStyle.Styled("No context available. Create or switch to a context first."))
		return true
	}

	if len(parts) < 2 {
		if contextInfo.TTL > 0 {
			fmt.Printf("Current TTL: %s\n", highlightStyle.Styled(contextInfo.TTL.String()))
		} else {
			fmt.Println(dimStyle.Styled("No TTL set (never expires)"))
		}
		fmt.Println(dimStyle.Styled("Usage: /ttl <duration>"))
		fmt.Println(dimStyle.Styled("Examples: /ttl 24h, /ttl 7d, /ttl 30m"))
		fmt.Println(dimStyle.Styled("          /ttl 0  (never expires)"))
	} else {
		if parts[1] == "0" {
			contextInfo.TTL = 0
			session.SetMetadata(contextInfo)
			fmt.Println(successStyle.Styled("TTL cleared (context never expires)"))
		} else {
			if duration, err := time.ParseDuration(parts[1]); err == nil {
				contextInfo.TTL = duration
				session.SetMetadata(contextInfo)
				fmt.Println(successStyle.Styled(fmt.Sprintf("TTL set to: %s", duration)))
			} else {
				// Try parsing with days suffix
				if strings.HasSuffix(parts[1], "d") {
					daysStr := strings.TrimSuffix(parts[1], "d")
					if days, err := parseInt(daysStr); err == nil {
						duration := time.Duration(days) * 24 * time.Hour
						contextInfo.TTL = duration
						session.SetMetadata(contextInfo)
						fmt.Println(successStyle.Styled(fmt.Sprintf("TTL set to: %s", duration)))
					} else {
						fmt.Println(errorStyle.Styled("Invalid duration format"))
					}
				} else {
					fmt.Println(errorStyle.Styled("Invalid duration format"))
				}
			}
		}
	}
	return true
}

func handleTools(parts []string, config *Config, session sessions.Session, rl *readline.Instance) bool {
	contextInfo := session.GetMetadata()
	if contextInfo == nil {
		fmt.Println(errorStyle.Styled("No context available. Create or switch to a context first."))
		return true
	}

	if len(parts) < 2 {
		if len(contextInfo.ToolPaths) > 0 {
			fmt.Println("Current tool paths:")
			for _, path := range contextInfo.ToolPaths {
				fmt.Printf("  - %s\n", highlightStyle.Styled(path))
			}
		} else {
			fmt.Println(dimStyle.Styled("No tool paths configured"))
		}
		fmt.Println(dimStyle.Styled("Usage: /tools add <path>"))
		fmt.Println(dimStyle.Styled("       /tools remove <path>"))
		fmt.Println(dimStyle.Styled("       /tools clear"))
	} else {
		switch parts[1] {
		case "add":
			if len(parts) < 3 {
				fmt.Println(errorStyle.Styled("Usage: /tools add <path>"))
			} else {
				path := strings.Join(parts[2:], " ")
				contextInfo.ToolPaths = append(contextInfo.ToolPaths, path)
				config.ToolPaths = contextInfo.ToolPaths
				session.SetMetadata(contextInfo)
				fmt.Println(successStyle.Styled(fmt.Sprintf("Added tool path: %s", path)))
				fmt.Println(dimStyle.Styled("Note: Restart session for changes to take effect"))
			}
		case "remove":
			if len(parts) < 3 {
				fmt.Println(errorStyle.Styled("Usage: /tools remove <path>"))
			} else {
				path := strings.Join(parts[2:], " ")
				newPaths := []string{}
				found := false
				for _, p := range contextInfo.ToolPaths {
					if p != path {
						newPaths = append(newPaths, p)
					} else {
						found = true
					}
				}
				if found {
					contextInfo.ToolPaths = newPaths
					config.ToolPaths = contextInfo.ToolPaths
					session.SetMetadata(contextInfo)
					fmt.Println(successStyle.Styled(fmt.Sprintf("Removed tool path: %s", path)))
					fmt.Println(dimStyle.Styled("Note: Restart session for changes to take effect"))
				} else {
					fmt.Println(errorStyle.Styled("Tool path not found"))
				}
			}
		case "clear":
			contextInfo.ToolPaths = []string{}
			config.ToolPaths = contextInfo.ToolPaths
			session.SetMetadata(contextInfo)
			fmt.Println(successStyle.Styled("All tool paths cleared"))
		default:
			fmt.Println(errorStyle.Styled("Unknown subcommand. Use: add, remove, or clear"))
		}
	}
	return true
}

func handleMCP(parts []string, config *Config, session sessions.Session, rl *readline.Instance) bool {
	contextInfo := session.GetMetadata()
	if contextInfo == nil {
		fmt.Println(errorStyle.Styled("No context available. Create or switch to a context first."))
		return true
	}

	if len(parts) < 2 {
		if len(contextInfo.MCPServers) > 0 {
			fmt.Println("Current MCP servers:")
			for _, server := range contextInfo.MCPServers {
				fmt.Printf("  - %s\n", highlightStyle.Styled(server))
			}
		} else {
			fmt.Println(dimStyle.Styled("No MCP servers configured"))
		}
		fmt.Println(dimStyle.Styled("Usage: /mcp add <server>"))
		fmt.Println(dimStyle.Styled("       /mcp remove <server>"))
		fmt.Println(dimStyle.Styled("       /mcp clear"))
	} else {
		switch parts[1] {
		case "add":
			if len(parts) < 3 {
				fmt.Println(errorStyle.Styled("Usage: /mcp add <server>"))
			} else {
				server := strings.Join(parts[2:], " ")
				contextInfo.MCPServers = append(contextInfo.MCPServers, server)
				config.MCPServers = contextInfo.MCPServers
				session.SetMetadata(contextInfo)
				fmt.Println(successStyle.Styled(fmt.Sprintf("Added MCP server: %s", server)))
				fmt.Println(dimStyle.Styled("Note: Restart session for MCP changes to take effect"))
			}
		case "remove":
			if len(parts) < 3 {
				fmt.Println(errorStyle.Styled("Usage: /mcp remove <server>"))
			} else {
				server := strings.Join(parts[2:], " ")
				newServers := []string{}
				found := false
				for _, s := range contextInfo.MCPServers {
					if s != server {
						newServers = append(newServers, s)
					} else {
						found = true
					}
				}
				if found {
					contextInfo.MCPServers = newServers
					config.MCPServers = contextInfo.MCPServers
					session.SetMetadata(contextInfo)
					fmt.Println(successStyle.Styled(fmt.Sprintf("Removed MCP server: %s", server)))
					fmt.Println(dimStyle.Styled("Note: Restart session for MCP changes to take effect"))
				} else {
					fmt.Println(errorStyle.Styled("MCP server not found"))
				}
			}
		case "clear":
			contextInfo.MCPServers = []string{}
			config.MCPServers = contextInfo.MCPServers
			session.SetMetadata(contextInfo)
			fmt.Println(successStyle.Styled("All MCP servers cleared"))
			fmt.Println(dimStyle.Styled("Note: Restart session for MCP changes to take effect"))
		default:
			fmt.Println(errorStyle.Styled("Unknown subcommand. Use: add, remove, or clear"))
		}
	}
	return true
}

func handleThinking(parts []string, config *Config, session sessions.Session, rl *readline.Instance) bool {
	contextInfo := session.GetMetadata()
	if contextInfo == nil {
		fmt.Println(errorStyle.Styled("No context available. Create or switch to a context first."))
		return true
	}

	if len(parts) < 2 {
		if contextInfo.ThinkingEffort != "off" && contextInfo.ThinkingEffort != "" {
			fmt.Printf("Current thinking effort: %s\n", highlightStyle.Styled(contextInfo.ThinkingEffort))
		} else if config.ThinkingEffort != "off" {
			fmt.Printf("Current thinking effort: %s\n", highlightStyle.Styled(config.ThinkingEffort))
		} else {
			fmt.Printf("Current thinking effort: %s\n", highlightStyle.Styled("off"))
		}
		fmt.Println(dimStyle.Styled("Usage: /think <level>"))
		fmt.Println(dimStyle.Styled("Levels: off, low, medium, high"))
		fmt.Println(dimStyle.Styled("       /think off  (to disable)"))
	} else {
		effort := strings.ToLower(parts[1])
		// Validate the thinking effort level
		validEfforts := map[string]bool{"off": true, "low": true, "medium": true, "high": true}
		if !validEfforts[effort] {
			fmt.Println(errorStyle.Styled("Invalid thinking effort. Use: off, low, medium, or high"))
			return true
		}

		contextInfo.ThinkingEffort = effort
		config.ThinkingEffort = effort
		session.SetMetadata(contextInfo)

		if effort == "off" {
			fmt.Println(successStyle.Styled("Thinking disabled"))
		} else {
			fmt.Println(successStyle.Styled(fmt.Sprintf("Thinking effort set to: %s", effort)))
		}
	}
	return true
}

func handleMaxTokens(parts []string, config *Config, session sessions.Session, rl *readline.Instance) bool {
	if len(parts) < 2 {
		fmt.Printf("Current max tokens: %s\n", highlightStyle.Styled(fmt.Sprintf("%d", config.MaxTokens)))
		fmt.Println(dimStyle.Styled("Usage: /maxtokens <number>"))
		fmt.Println(dimStyle.Styled("Example: /maxtokens 4096"))
	} else {
		if tokens, err := parseInt(parts[1]); err == nil {
			if tokens <= 0 {
				fmt.Println(errorStyle.Styled("Max tokens must be positive"))
				return true
			}
			config.MaxTokens = tokens
			if contextInfo := session.GetMetadata(); contextInfo != nil {
				contextInfo.MaxTokens = tokens
				session.SetMetadata(contextInfo)
			}
			fmt.Println(successStyle.Styled(fmt.Sprintf("Max tokens set to: %d", tokens)))
		} else {
			fmt.Println(errorStyle.Styled("Invalid number"))
		}
	}
	return true
}

func handleToolTimeout(parts []string, config *Config, session sessions.Session, rl *readline.Instance) bool {
	if len(parts) < 2 {
		if config.ToolTimeout > 0 {
			fmt.Printf("Current tool timeout: %s\n", highlightStyle.Styled(config.ToolTimeout.String()))
		} else {
			fmt.Println(dimStyle.Styled("No tool timeout set (using default)"))
		}
		fmt.Println(dimStyle.Styled("Usage: /tooltimeout <duration>"))
		fmt.Println(dimStyle.Styled("Examples: /tooltimeout 30s, /tooltimeout 2m, /tooltimeout 0 (no timeout)"))
	} else {
		if parts[1] == "0" {
			config.ToolTimeout = 0
			if contextInfo := session.GetMetadata(); contextInfo != nil {
				contextInfo.ToolTimeout = 0
				session.SetMetadata(contextInfo)
			}
			fmt.Println(successStyle.Styled("Tool timeout disabled"))
		} else {
			if duration, err := time.ParseDuration(parts[1]); err == nil {
				if duration < 0 {
					fmt.Println(errorStyle.Styled("Tool timeout must be positive or 0"))
					return true
				}
				config.ToolTimeout = duration
				if contextInfo := session.GetMetadata(); contextInfo != nil {
					contextInfo.ToolTimeout = duration
					session.SetMetadata(contextInfo)
				}
				fmt.Println(successStyle.Styled(fmt.Sprintf("Tool timeout set to: %s", duration)))
			} else {
				fmt.Println(errorStyle.Styled("Invalid duration format"))
			}
		}
	}
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
			readline.PcItem("add"),
			readline.PcItem("remove"),
			readline.PcItem("clear"),
		),
		readline.PcItem("/mcp",
			readline.PcItem("add"),
			readline.PcItem("remove"),
			readline.PcItem("clear"),
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
func printWelcomeMessage(config *Config, session sessions.Session, contextID string) {
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
		{"/tools", "Manage tool paths (add/remove/clear)"},
		{"/mcp", "Manage MCP servers (add/remove/clear)"},
		{"/debug", "Toggle debug mode"},
		{"/help", "Show this help message"},
	}

	for _, c := range commands {
		safePrintf("  %-20s %s\n", highlightStyle.Styled(c.cmd), c.desc)
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
		safePrintln(dimStyle.Styled("No conversation history."))
		return
	}

	formatted := formatConversation(history)
	safePrintf("%s", formatted)
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

func parseInt(s string) (int, error) {
	return strconv.Atoi(s)
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
	for _, path := range paths {
		fileInfo := getFileInfo(path)
		fmt.Printf("\n%s\n", dimStyle.Styled(fmt.Sprintf("📎 Attached: %s", fileInfo)))
	}
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
