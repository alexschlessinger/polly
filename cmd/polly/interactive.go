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

var RL *readline.Instance

// interactiveContext holds context needed by interactive commands
type interactiveContext struct {
	registry *tools.ToolRegistry
}

var interactiveCtx *interactiveContext

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

	// Set interactive context for commands
	interactiveCtx = &interactiveContext{
		registry: toolRegistry,
	}

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

    // Set initial styled prompt and dynamic autocomplete
    refreshInteractiveUI(config, session, toolRegistry)

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
        refreshInteractiveUI(config, session, interactiveCtx.registry)
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
        safePrintln(dimStyle.Styled("Usage: /file <path>"))
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
        safePrintln(errorStyle.Styled(fmt.Sprintf("File not found: %s", filePath)))
        return true
    }

	// Build and add file message to session
	userMsg, err := buildMessageWithFiles("", []string{filePath})
    if err != nil {
        safePrintln(errorStyle.Styled(fmt.Sprintf("Error processing file: %v", err)))
        return true
    }

	// Add to session
	session.AddMessage(userMsg)

	// Show confirmation
	fileInfo := getFileInfo(filePath)
    safePrintln(dimStyle.Styled(fmt.Sprintf("📎 Attached: %s", fileInfo)))
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
		safePrintln(errorStyle.Styled("No context available. Create or switch to a context first."))
		return true
	}

	// Check if we have a registry for operations that need it
	needsRegistry := len(parts) >= 2 && parts[1] != "list"
	if needsRegistry && (interactiveCtx == nil || interactiveCtx.registry == nil) {
		safePrintln(errorStyle.Styled("Tool registry not available. Please restart the session."))
		return true
	}

    if len(parts) < 2 || parts[1] == "list" {
		// List all tools with their types
		if interactiveCtx != nil && interactiveCtx.registry != nil {
			allTools := interactiveCtx.registry.All()
			if len(allTools) == 0 {
				safePrintln(userStyle.Styled("No tools currently loaded"))
			} else {
				safePrintln("Currently loaded tools:")
				for _, tool := range allTools {
					// Capitalize tool type for display
					toolType := tool.GetType()
					displayType := toolType
					switch toolType {
					case "shell":
						displayType = "Shell"
					case "mcp":
						displayType = "MCP"
					case "native":
						displayType = "Native"
					}
					safePrintf("  - %s [%s]\n", highlightStyle.Styled(tool.GetName()), dimStyle.Styled(displayType))
				}
			}
		} else {
			safePrintln(userStyle.Styled("No tools currently loaded"))
		}
        refreshInteractiveUI(config, session, interactiveCtx.registry)
        return true
    }

	switch parts[1] {
	case "add":
        if len(parts) < 3 {
            safePrintln(errorStyle.Styled("Usage: /tools add <path|server>"))
            return true
        }
		
		pathOrServer := strings.Join(parts[2:], " ")

		// Get current tools before loading
		toolsBefore := make(map[string]bool)
		for _, tool := range interactiveCtx.registry.All() {
			toolsBefore[tool.GetName()] = true
		}

		// Try to auto-detect and load
		_, err := interactiveCtx.registry.LoadToolAuto(pathOrServer)
        if err != nil {
            safePrintln(errorStyle.Styled(fmt.Sprintf("Failed to load tool: %v", err)))
            return true
        }

		// Find newly loaded tools
		var newTools []string
		for _, tool := range interactiveCtx.registry.All() {
			name := tool.GetName()
			if !toolsBefore[name] {
				newTools = append(newTools, name)
			}
		}

		// Update ActiveTools metadata with new loader info
		contextInfo.ActiveTools = interactiveCtx.registry.GetActiveToolLoaders()
		session.SetMetadata(contextInfo)

		// Display what was loaded
		if len(newTools) > 0 {
			safePrintln(successStyle.Styled(fmt.Sprintf("Loaded tools: %s", strings.Join(newTools, ", "))))
			refreshInteractiveUI(config, session, interactiveCtx.registry)
		} else {
			safePrintln(dimStyle.Styled("No new tools were loaded"))
		}

		// Add assistant message to update LLM context
		session.AddMessage(messages.ChatMessage{
			Role:    messages.MessageRoleAssistant,
			Content: "My available tools have been updated.",
		})

	case "remove":
        if len(parts) < 3 {
            safePrintln(errorStyle.Styled("Usage: /tools remove <name or pattern>"))
            return true
        }
		
		pattern := strings.Join(parts[2:], " ")
		
		// Check if it's a wildcard pattern
		if strings.Contains(pattern, "*") {
			// Wildcard removal
			var removed []string
			for _, tool := range interactiveCtx.registry.All() {
				name := tool.GetName()
				matched, _ := path.Match(pattern, name)
				if matched {
					interactiveCtx.registry.Remove(name)
					removed = append(removed, name)
				}
			}
			
            if len(removed) > 0 {
				// Update ActiveTools in metadata - remove all matched tools
				newLoaders := []tools.ToolLoaderInfo{}
				for _, loader := range contextInfo.ActiveTools {
					found := false
					for _, removedName := range removed {
						if loader.Name == removedName {
							found = true
							break
						}
					}
					if !found {
						newLoaders = append(newLoaders, loader)
					}
				}
				contextInfo.ActiveTools = newLoaders
				session.SetMetadata(contextInfo)
				
                safePrintln(successStyle.Styled(fmt.Sprintf("Removed %d tools: %s", len(removed), strings.Join(removed, ", "))))
				refreshInteractiveUI(config, session, interactiveCtx.registry)
				
				// Add assistant message to update LLM context
				session.AddMessage(messages.ChatMessage{
					Role:    messages.MessageRoleAssistant,
					Content: "My available tools have been updated.",
				})
			} else {
                    safePrintln(errorStyle.Styled(fmt.Sprintf("No tools matched pattern: %s", pattern)))
                }
            } else {
                // Exact match removal
                _, exists := interactiveCtx.registry.Get(pattern)
                if !exists {
                    safePrintln(errorStyle.Styled(fmt.Sprintf("Tool not found: %s", pattern)))
                    return true
                }

			// Remove the tool from registry
			interactiveCtx.registry.Remove(pattern)

			// Update ActiveTools in metadata - remove just this specific tool
			newLoaders := []tools.ToolLoaderInfo{}
			for _, loader := range contextInfo.ActiveTools {
				if loader.Name != pattern {
					newLoaders = append(newLoaders, loader)
				}
			}
			contextInfo.ActiveTools = newLoaders
			session.SetMetadata(contextInfo)

                safePrintln(successStyle.Styled(fmt.Sprintf("Removed tool: %s", pattern)))
			refreshInteractiveUI(config, session, interactiveCtx.registry)

			// Add assistant message to update LLM context
			session.AddMessage(messages.ChatMessage{
				Role:    messages.MessageRoleAssistant,
				Content: "My available tools have been updated.",
			})
		}

	case "reload":
		// Clear and reload all tools
		safePrintln("Reloading all tools...")

		// Close existing registry
		if err := interactiveCtx.registry.Close(); err != nil {
			log.Printf("Error closing registry: %v", err)
		}

		// Reload tools from session metadata
		registry, err := loadTools(contextInfo.ActiveTools)
		if err != nil {
			safePrintln(errorStyle.Styled(fmt.Sprintf("Failed to reload tools: %v", err)))
			return true
		}

		interactiveCtx.registry = registry
		safePrintln(successStyle.Styled("All tools reloaded"))

		// Add assistant message to update LLM context
		session.AddMessage(messages.ChatMessage{
			Role:    messages.MessageRoleAssistant,
			Content: "My available tools have been updated.",
		})

	default:
		safePrintln(errorStyle.Styled(fmt.Sprintf("Unknown subcommand: %s", parts[1])))
		safePrintln(userStyle.Styled("Usage: /tools [list]              - List all loaded tools"))
		safePrintln(userStyle.Styled("       /tools add <path|server>   - Add a tool"))
		safePrintln(userStyle.Styled("       /tools remove <name|pattern> - Remove tool(s)"))
		safePrintln(userStyle.Styled("       /tools reload              - Reload all tools"))
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
        refreshInteractiveUI(config, session, interactiveCtx.registry)
    }
    refreshInteractiveUI(config, session, interactiveCtx.registry)
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
    // Build dynamic tool name list for `/tools remove` suggestions
    var toolItems []readline.PrefixCompleterInterface
    if interactiveCtx != nil && interactiveCtx.registry != nil {
        for _, t := range interactiveCtx.registry.All() {
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
func refreshInteractiveUI(config *Config, session sessions.Session, registry *tools.ToolRegistry) {
    if RL == nil {
        return
    }
    RL.SetPrompt(makePrompt(config, session, registry))
    RL.Config.AutoComplete = createAutoCompleter()
    // Force redraw so the new prompt is visible immediately
    RL.Refresh()
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
		{"/tools", "Manage all tools (list/add/remove/reload/mcp)"},
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
    if len(paths) <= 2 {
        for _, path := range paths {
            fileInfo := getFileInfo(path)
            safePrintln(dimStyle.Styled(fmt.Sprintf("📎 Attached: %s", fileInfo)))
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
    safePrintln(dimStyle.Styled(summary))
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
