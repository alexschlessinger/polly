package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/urfave/cli/v3"
)

func main() {
	app := &cli.Command{
		Name:   "polly",
		Usage:  "Chat with LLMs using various providers",
		Flags:  defineFlags(),
		Action: runCommand,
	}

	if err := app.Run(context.Background(), os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func defineFlags() []cli.Flag {
	return []cli.Flag{
		// Model configuration
		&cli.StringFlag{
			Name:    "model",
			Aliases: []string{"m"},
			Usage:   "Model to use (provider/model format)",
			Value:   defaultModel,
		},
		&cli.Float64Flag{
			Name:  "temp",
			Usage: "Temperature for sampling",
			Value: defaultTemperature,
		},
		&cli.IntFlag{
			Name:  "maxtokens",
			Usage: "Maximum tokens to generate",
			Value: defaultMaxTokens,
		},
		&cli.DurationFlag{
			Name:  "timeout",
			Usage: "Request timeout",
			Value: defaultTimeout,
		},
		&cli.BoolFlag{
			Name:  "think",
			Usage: "Enable thinking/reasoning (low effort)",
		},
		&cli.BoolFlag{
			Name:  "think-medium",
			Usage: "Enable thinking/reasoning (medium effort)",
		},
		&cli.BoolFlag{
			Name:  "think-hard",
			Usage: "Enable thinking/reasoning (high effort)",
		},

		// API configuration
		&cli.StringFlag{
			Name:  "baseurl",
			Usage: "Base URL for API (for OpenAI-compatible endpoints or Ollama)",
			Value: defaultBaseURL,
		},

		// Tool configuration
		&cli.StringSliceFlag{
			Name:    "tool",
			Aliases: []string{"t"},
			Usage:   "Shell tool executable path (can be specified multiple times)",
		},
		&cli.StringSliceFlag{
			Name:  "mcp",
			Usage: "MCP server and arguments (can be specified multiple times)",
		},

		// Input configuration
		&cli.StringFlag{
			Name:    "prompt",
			Aliases: []string{"p"},
			Usage:   "Initial prompt (reads from stdin if not provided)",
		},
		&cli.StringFlag{
			Name:    "system",
			Aliases: []string{"s"},
			Usage:   "System prompt",
			Value:   defaultSystemPrompt,
		},
		&cli.StringSliceFlag{
			Name:    "file",
			Aliases: []string{"f"},
			Usage:   "File, image, or URL to include (can be specified multiple times)",
		},
		&cli.StringFlag{
			Name:  "schema",
			Usage: "Path to JSON schema file for structured output",
		},

		// Context management
		&cli.StringFlag{
			Name:    "context",
			Aliases: []string{"c"},
			Usage:   "Context name (@name) or ID for conversation continuity (uses pollyTOOL_CONTEXT env var if not set)",
		},
		&cli.BoolFlag{
			Name:    "last",
			Aliases: []string{"L"},
			Usage:   "Use the last active context",
		},
		&cli.BoolFlag{
			Name:  "reset",
			Usage: "Reset context (clear conversation history, keep settings)",
		},
		&cli.BoolFlag{
			Name:  "list",
			Usage: "List all available context IDs",
		},
		&cli.StringFlag{
			Name:  "delete",
			Usage: "Delete the specified context",
		},
		&cli.BoolFlag{
			Name:  "add",
			Usage: "Add stdin content to context without making an API call",
		},
		&cli.BoolFlag{
			Name:  "purge",
			Usage: "Delete all sessions and index (requires confirmation)",
		},

		// Output configuration
		&cli.BoolFlag{
			Name:  "quiet",
			Usage: "Suppress confirmation messages",
		},
		&cli.BoolFlag{
			Name:    "debug",
			Aliases: []string{"d"},
			Usage:   "Enable debug logging",
		},
	}
}

func runCommand(ctx context.Context, cmd *cli.Command) error {
	config := parseConfig(cmd)

	// Set up logging
	if !config.Debug {
		log.SetOutput(io.Discard)
	}

	// Get context ID from config or environment
	contextID := getContextID(config)

	// Validate context name if provided
	if contextID != "" {
		if err := validateContextName(contextID); err != nil {
			return fmt.Errorf("invalid context name '%s': %w", contextID, err)
		}
	}

	// Handle --last flag
	if config.UseLastContext {
		// Will be resolved after sessionStore is created
		contextID = ""
	}

	// Set up session store
	sessionStore, err := setupSessionStore(config, contextID)
	if err != nil {
		return fmt.Errorf("failed to create context store: %w", err)
	}

	// Resolve --last flag if specified
	if config.UseLastContext {
		if fileStore, ok := sessionStore.(*sessions.FileSessionStore); ok {
			contextID = fileStore.GetLastContext()
			if contextID == "" {
				return fmt.Errorf("no last context found")
			}
		}
	}

	// Track if we just created a new context
	var justCreatedContext bool

	// Handle --reset flag (reset and use)
	if config.ResetContext {
		// Determine which context to reset
		resetContextName := contextID // Use the context from -c or environment

		// If --last was specified, get the last context
		if config.UseLastContext && resetContextName == "" {
			if fileStore, ok := sessionStore.(*sessions.FileSessionStore); ok {
				resetContextName = fileStore.GetLastContext()
				if resetContextName == "" {
					return fmt.Errorf("no last context found to reset")
				}
			}
		}

		// Ensure we have a context to reset
		if resetContextName == "" {
			return fmt.Errorf("--reset requires a context (use -c or --last)")
		}

		// Check if context exists and handle accordingly
		if fileStore, ok := sessionStore.(*sessions.FileSessionStore); ok {
			if !checkAndPromptForReset(fileStore, resetContextName) {
				return nil // User cancelled or context doesn't exist
			}

			// Check if context already exists to preserve settings
			existingInfo := fileStore.GetContextByNameOrID(resetContextName)
			if existingInfo != nil {
				// Preserve existing settings
				existingInfo.LastUsed = time.Now()
				// Override with command-line settings if provided
				if config.Model != defaultModel {
					existingInfo.Model = config.Model
				}
				if config.Temperature != defaultTemperature {
					existingInfo.Temperature = config.Temperature
				}
				// Update system prompt if explicitly set (even if empty)
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

				// Clear the conversation file
				if err := resetContext(fileStore, resetContextName); err != nil {
					return fmt.Errorf("failed to reset context: %w", err)
				}
			} else {
				// This shouldn't happen since checkAndPromptForReset should have caught it
				return fmt.Errorf("context '%s' does not exist", resetContextName)
			}
		}
		contextID = resetContextName
		justCreatedContext = true
		// Continue with normal flow using this reset context
	}

	// Handle context management operations
	if config.ListContexts {
		return handleListContexts(sessionStore)
	}
	if config.DeleteContext != "" {
		// Validate the context name to delete
		if err := validateContextName(config.DeleteContext); err != nil {
			return fmt.Errorf("invalid context name '%s': %w", config.DeleteContext, err)
		}
		return handleDeleteContext(sessionStore, config.DeleteContext)
	}
	if config.AddToContext {
		return handleAddToContext(sessionStore, config, contextID)
	}
	if config.PurgeAll {
		return handlePurgeAll(sessionStore)
	}

	// Set up signal handling
	ctx, cancel := setupSignalHandling(ctx)
	defer cancel()

	// Check if context exists and prompt if not (skip if we just created it)
	if contextID != "" && !justCreatedContext {
		if fileStore, ok := sessionStore.(*sessions.FileSessionStore); ok {
			contextID = checkAndPromptForMissingContext(fileStore, contextID)
			if contextID == "" {
				return nil // User cancelled
			}
		}
	}

	// Prepare for conversation
	return runConversation(ctx, config, sessionStore, contextID)
}

func runConversation(ctx context.Context, config *Config, sessionStore sessions.SessionStore, contextID string) error {
	// Track if we need to reset due to system prompt change
	var needReset bool
	var originalContextInfo *sessions.ContextInfo

	// Load context settings if available
	if fileStore, ok := sessionStore.(*sessions.FileSessionStore); ok {
		if contextInfo := fileStore.GetContextByNameOrID(contextID); contextInfo != nil {
			originalContextInfo = contextInfo

			// Check if system prompt is being changed (only if context has existing conversation)
			if config.SystemPromptWasSet && config.SystemPrompt != contextInfo.SystemPrompt {
				// Check if there's an existing conversation to reset
				sessionPath := filepath.Join(fileStore.GetBaseDir(), contextInfo.Name+".json")
				if _, err := os.Stat(sessionPath); err == nil {
					needReset = true
					fmt.Fprintf(os.Stderr, "System prompt changed, resetting conversation...\n")
				}
			}

			// Use stored settings if not overridden by command line
			if config.Model == defaultModel && contextInfo.Model != "" {
				config.Model = contextInfo.Model
			}
			if config.Temperature == defaultTemperature && contextInfo.Temperature != 0 {
				config.Temperature = contextInfo.Temperature
			}
			// Only use stored system prompt if flag wasn't explicitly set
			if !config.SystemPromptWasSet && contextInfo.SystemPrompt != "" {
				config.SystemPrompt = contextInfo.SystemPrompt
			}
			// Apply stored tools if none provided via command line
			if len(config.ToolPaths) == 0 && len(contextInfo.ToolPaths) > 0 {
				config.ToolPaths = contextInfo.ToolPaths
			}
			if len(config.MCPServers) == 0 && len(contextInfo.MCPServers) > 0 {
				config.MCPServers = contextInfo.MCPServers
			}
		}
	}

	// Perform reset if system prompt changed
	if needReset && originalContextInfo != nil {
		if fileStore, ok := sessionStore.(*sessions.FileSessionStore); ok {
			// Get the context name
			contextName := contextID
			if originalContextInfo.Name != "" {
				contextName = originalContextInfo.Name
			}

			// Reset the context
			if err := resetContext(fileStore, contextName); err != nil {
				return fmt.Errorf("failed to reset context: %w", err)
			}
			// Context name remains the same after reset
			contextID = contextName
		}
	}

	// Get prompt
	prompt, err := getPrompt(config)
	if err != nil {
		return err
	}

	// Load schema if specified
	var schema *llm.Schema
	if config.SchemaPath != "" {
		var err error
		schema, err = loadSchemaFile(config.SchemaPath)
		if err != nil {
			return fmt.Errorf("failed to load schema: %w", err)
		}
	}

	// Load API keys
	apiKeys := loadAPIKeys()

	// Create LLM provider
	multipass := llm.NewMultiPass(apiKeys)

	// Get or create session
	needFileStore := needsFileStore(config, contextID)
	session := getOrCreateSession(sessionStore, contextID, needFileStore)
	defer closeFileSession(session)

	// Update context info with current settings if it has metadata
	if fileStore, ok := sessionStore.(*sessions.FileSessionStore); ok {
		if fileStore.GetContextByNameOrID(contextID) != nil {
			// This context has metadata, update its settings
			if contextInfo := fileStore.GetContextByNameOrID(contextID); contextInfo != nil {
				contextInfo.Model = config.Model
				contextInfo.Temperature = config.Temperature
				contextInfo.SystemPrompt = config.SystemPrompt
				contextInfo.ToolPaths = config.ToolPaths
				contextInfo.MCPServers = config.MCPServers
				fileStore.SaveContextInfo(contextInfo)
			}
		}
	}

	// Load tools
	toolRegistry := loadTools(config)

	// Build user message with files if provided
	userMsg, err := buildMessageWithFiles(prompt, config.Files)
	if err != nil {
		return fmt.Errorf("error processing files: %w", err)
	}

	// Add user message and execute
	session.AddMessage(userMsg)

	executeCompletion(ctx, config, multipass, session, toolRegistry, schema)
	return nil
}

func getPrompt(config *Config) (string, error) {
	if config.Prompt != "" {
		return config.Prompt, nil
	}

	if hasStdinData() {
		return readFromStdin()
	}

	// No -p flag and no pipe input, prompt the user interactively
	fmt.Fprint(os.Stderr, "Enter prompt (Ctrl+D when done):\n")
	return readFromStdin()
}

func executeCompletion(
	ctx context.Context,
	config *Config,
	provider llm.LLM,
	session sessions.Session,
	registry *tools.ToolRegistry,
	schema *llm.Schema,
) {
	executeCompletionWithStatusLine(ctx, config, provider, session, registry, schema, nil)
}

func executeCompletionWithStatusLine(
	ctx context.Context,
	config *Config,
	provider llm.LLM,
	session sessions.Session,
	registry *tools.ToolRegistry,
	schema *llm.Schema,
	statusLine *Status,
) {
	// Create request
	req := &llm.CompletionRequest{
		BaseURL:        config.BaseURL,
		Timeout:        config.Timeout,
		Temperature:    float32(config.Temperature),
		Model:          config.Model,
		MaxTokens:      config.MaxTokens,
		Messages:       session.GetHistory(),
		Tools:          registry.All(),
		ResponseSchema: schema,
		ThinkingEffort: config.ThinkingEffort,
	}

	// Create stream processor
	processor := messages.NewStreamProcessor()

	// Create status line only if not provided (first call)
	var shouldStopStatusLine bool
	if statusLine == nil {
		statusLine = createStatusLine(config)
		if statusLine != nil {
			statusLine.Start()
			shouldStopStatusLine = true
			defer func() {
				if shouldStopStatusLine {
					statusLine.Stop()
				}
			}()
		}
	}

	// Show initial spinner
	if statusLine != nil {
		statusLine.ShowSpinner("waiting")
	}

	// Use event-based streaming
	eventChan := provider.ChatCompletionStream(ctx, req, processor)

	// Process response using the new event stream
	response := processEventStream(ctx, config, session, registry, statusLine, schema, eventChan)

	// If there were tool calls, continue the completion
	if len(response.ToolCalls) > 0 {
		executeCompletionWithStatusLine(ctx, config, provider, session, registry, schema, statusLine)
	}
}

func processEventStream(
	ctx context.Context,
	config *Config,
	session sessions.Session,
	registry *tools.ToolRegistry,
	statusLine *Status,
	schema *llm.Schema,
	eventChan <-chan *messages.StreamEvent,
) messages.ChatMessage {
	var fullResponse messages.ChatMessage
	var responseText strings.Builder
	var firstByteReceived bool
	var reasoningLength int

	for event := range eventChan {
		switch event.Type {
		case messages.EventTypeReasoning:
			// Track reasoning length for status display
			reasoningLength += len(event.Content)
			if statusLine != nil {
				statusLine.UpdateThinkingProgress(reasoningLength)
			}

		case messages.EventTypeContent:
			// Clear status on first content
			if !firstByteReceived && statusLine != nil {
				firstByteReceived = true
				statusLine.ClearForContent()
			}

			// Accumulate content for the session
			responseText.WriteString(event.Content)

			if config.SchemaPath == "" {
				// Print content as it arrives
				if event.Content != "" {
					if statusLine != nil {
						statusLine.Print(event.Content)
					} else {
						fmt.Print(event.Content)
					}
				}

				// Update terminal title with streaming progress
				if statusLine != nil {
					statusLine.UpdateStreamingProgress(responseText.Len())
				}
			}

		case messages.EventTypeToolCall:
			// Individual tool calls could be processed here if needed
			// For now, we'll handle them in the complete event

		case messages.EventTypeComplete:
			fullResponse = *event.Message

			// Use streamed content if available, otherwise use message content
			if responseText.Len() > 0 {
				fullResponse.Content = strings.TrimLeft(responseText.String(), " \t\n\r")
			}

			// Reasoning is already captured in the message from the event

			// Add assistant response to session
			session.AddMessage(fullResponse)

			// Process tool calls if any
			if len(fullResponse.ToolCalls) > 0 {
				if statusLine != nil {
					statusLine.Clear()
				}

				// If we have content, ensure proper formatting before tool execution
				if fullResponse.Content != "" && config.SchemaPath != "" {
					// In JSON mode, we'll output everything at the end
				} else if fullResponse.Content != "" && responseText.Len() == 0 {
					// Content wasn't streamed (responseText is empty), print it now
					if statusLine != nil {
						statusLine.Print(fullResponse.Content)
					} else {
						fmt.Print(fullResponse.Content)
					}
				} else if responseText.Len() > 0 && config.SchemaPath == "" {
					// Content was streamed, add a newline before tool output
					fmt.Println()
				}

				processToolCalls(ctx, fullResponse.ToolCalls, registry, session, config, statusLine)
			}

		case messages.EventTypeError:
			if statusLine != nil {
				statusLine.Clear()
			}
			fullResponse = messages.ChatMessage{
				Role:    messages.MessageRoleAssistant,
				Content: fmt.Sprintf("Error: %v", event.Error),
			}
			session.AddMessage(fullResponse)
		}
	}

	// Output final response
	if len(fullResponse.ToolCalls) == 0 {
		if config.SchemaPath != "" {
			outputStructured(fullResponse.Content, schema)
		} else {
			outputText()
		}
	}

	return fullResponse
}
