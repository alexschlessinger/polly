package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/muesli/termenv"
	"github.com/urfave/cli/v3"
)

func main() {
	// Set up panic recovery to ensure cleanup
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "Fatal error: %v\n", r)
			cleanupAndExit(2)
		}
	}()

	app := &cli.Command{
		Name:   "polly",
		Usage:  "Chat with LLMs using various providers",
		Flags:  defineFlags(),
		Before: validateFlags,
		Action: runCommand,
		OnUsageError: func(ctx context.Context, cmd *cli.Command, err error, isSubcommand bool) error {
			// Just return the error without showing usage
			return err
		},
	}

	if err := app.Run(context.Background(), os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		cleanupAndExit(1)
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
			Usage:   "Context name for conversation continuity (uses POLLYTOOL_CONTEXT env var if not set)",
		},
		&cli.BoolFlag{
			Name:    "last",
			Aliases: []string{"L"},
			Usage:   "Use the last active context",
		},
		&cli.StringFlag{
			Name:  "reset",
			Usage: "Reset the specified context (clear conversation history, keep settings)",
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
		&cli.StringFlag{
			Name:  "create",
			Usage: "Create a new context with specified name and configuration",
		},
		&cli.StringFlag{
			Name:  "show",
			Usage: "Show configuration for the specified context",
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

func validateFlags(ctx context.Context, cmd *cli.Command) (context.Context, error) {
	// Count how many standalone operations are requested
	standaloneOps := 0
	if cmd.String("reset") != "" {
		standaloneOps++
	}
	if cmd.Bool("purge") {
		standaloneOps++
	}
	if cmd.String("create") != "" {
		standaloneOps++
	}
	if cmd.String("show") != "" {
		standaloneOps++
	}
	if cmd.Bool("list") {
		standaloneOps++
	}
	if cmd.String("delete") != "" {
		standaloneOps++
	}
	if cmd.Bool("add") {
		standaloneOps++
	}

	if standaloneOps > 1 {
		return ctx, fmt.Errorf("only one operation flag can be used at a time")
	}

	// --purge must be completely alone (except quiet/debug)
	if cmd.Bool("purge") {
		// Check for any other non-output flags
		if cmd.String("context") != "" || cmd.Bool("last") ||
			cmd.String("prompt") != "" || len(cmd.StringSlice("file")) > 0 ||
			cmd.String("model") != defaultModel ||
			cmd.Float64("temp") != defaultTemperature ||
			cmd.Int("maxtokens") != defaultMaxTokens ||
			len(cmd.StringSlice("tool")) > 0 || len(cmd.StringSlice("mcp")) > 0 {
			return ctx, fmt.Errorf("--purge must be used alone (only --quiet or --debug allowed)")
		}
	}

	// --reset doesn't take prompts or files
	if cmd.String("reset") != "" {
		if cmd.String("prompt") != "" || len(cmd.StringSlice("file")) > 0 {
			return ctx, fmt.Errorf("--reset does not take prompts or files")
		}
	}

	// --create doesn't take a prompt
	if cmd.String("create") != "" {
		if cmd.String("prompt") != "" {
			return ctx, fmt.Errorf("--create does not take a prompt (use model/settings flags to configure)")
		}
	}

	// --show doesn't take prompt or files
	if cmd.String("show") != "" {
		if cmd.String("prompt") != "" || len(cmd.StringSlice("file")) > 0 {
			return ctx, fmt.Errorf("--show does not take prompts or files")
		}
	}

	// --list doesn't need any other flags
	if cmd.Bool("list") {
		if cmd.String("prompt") != "" || len(cmd.StringSlice("file")) > 0 {
			return ctx, fmt.Errorf("--list does not take prompts or files")
		}
	}

	// --delete doesn't take prompts/files
	if cmd.String("delete") != "" {
		if cmd.String("prompt") != "" || len(cmd.StringSlice("file")) > 0 {
			return ctx, fmt.Errorf("--delete does not take prompts or files")
		}
	}

	// Note: --add can take -p, --file, or stdin (validated in handleAddToContext)

	return ctx, nil
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

	// Handle --reset flag
	if config.ResetContext != "" {
		return handleResetContext(sessionStore, config, config.ResetContext)
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
	if config.CreateContext != "" {
		return handleCreateContext(sessionStore, config, config.CreateContext)
	}
	if config.ShowContext != "" {
		return handleShowContext(sessionStore, config.ShowContext)
	}

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

// initializeSession sets up everything needed for a conversation session
func initializeSession(config *Config, sessionStore sessions.SessionStore, contextID string) (string, sessions.Session, llm.LLM, *tools.ToolRegistry, error) {
	// Initialize conversation using helper function
	var err error
	contextID, _, _, err = initializeConversation(config, sessionStore, contextID)
	if err != nil {
		return "", nil, nil, nil, err
	}

	// Load API keys
	apiKeys := loadAPIKeys()

	// Create LLM provider
	multipass := llm.NewMultiPass(apiKeys)

	// Get or create session
	needFileStore := needsFileStore(config, contextID)
	session := getOrCreateSession(sessionStore, contextID, needFileStore)

	// Update context info with current settings using helper function
	updateContextInfo(sessionStore, contextID, config)

	// Load tools
	toolRegistry, err := loadTools(config)
	if err != nil {
		closeFileSession(session)
		return "", nil, nil, nil, err
	}

	return contextID, session, multipass, toolRegistry, nil
}

func runConversation(ctx context.Context, config *Config, sessionStore sessions.SessionStore, contextID string) error {
	// Initialize session
	contextID, session, multipass, toolRegistry, err := initializeSession(config, sessionStore, contextID)
	if err != nil {
		return err
	}
	defer closeFileSession(session)

	// Get prompt early to determine if we're going to interactive mode
	prompt, err := getPrompt(config)
	if err != nil {
		return err
	}

	// If no prompt provided and no stdin, switch to interactive mode
	// Don't set up signal handling for interactive mode - let readline handle it
	if prompt == "" {
		return runInteractiveMode(ctx, config, session, multipass, toolRegistry, contextID)
	}

	// Only set up signal handling for non-interactive mode
	ctx, cancel := setupSignalHandling(ctx)
	defer cancel()

	// Load schema if specified
	var schema *llm.Schema
	if config.SchemaPath != "" {
		var err error
		schema, err = loadSchemaFile(config.SchemaPath)
		if err != nil {
			return fmt.Errorf("failed to load schema: %w", err)
		}
	}

	// Build user message with files if provided
	userMsg, err := buildMessageWithFiles(prompt, config.Files)
	if err != nil {
		return fmt.Errorf("error processing files: %w", err)
	}

	// Add user message and execute
	session.AddMessage(userMsg)

	// Create status line if appropriate
	statusLine := createStatusLine(config)
	if statusLine != nil {
		statusLine.Start()
		defer statusLine.Stop()
	}

	executeCompletion(ctx, config, multipass, session, toolRegistry, schema, statusLine)
	return nil
}

func getPrompt(config *Config) (string, error) {
	if config.Prompt != "" {
		return config.Prompt, nil
	}

	if hasStdinData() {
		return readFromStdin()
	}

	// No -p flag and no pipe input - signal to use interactive mode
	return "", nil
}

func executeCompletion(
	ctx context.Context,
	config *Config,
	provider llm.LLM,
	session sessions.Session,
	registry *tools.ToolRegistry,
	schema *llm.Schema,
	statusLine StatusHandler,
) {
	// Create request using helper function
	req := createCompletionRequest(config, session, registry, schema)

	// Create stream processor
	processor := messages.NewStreamProcessor()

	// Status line is now managed by the caller

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
		executeCompletion(ctx, config, provider, session, registry, schema, statusLine)
	}
}

func processEventStream(
	ctx context.Context,
	config *Config,
	session sessions.Session,
	registry *tools.ToolRegistry,
	statusLine StatusHandler,
	schema *llm.Schema,
	eventChan <-chan *messages.StreamEvent,
) messages.ChatMessage {
	var fullResponse messages.ChatMessage
	var responseText strings.Builder
	var firstByteReceived bool
	var reasoningLength int
	var messageCommitted bool

	for event := range eventChan {
		// Check if context was cancelled (but only before committing messages)
		if !messageCommitted {
			select {
			case <-ctx.Done():
				// Context cancelled, return empty response
				if statusLine != nil {
					statusLine.Clear()
				}
				return messages.ChatMessage{}
			default:
			}
		}
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

			// Print content as it arrives (unless using schema/structured output)
			if config.SchemaPath == "" && event.Content != "" {
				fmt.Print(event.Content)
			}

			// Update streaming progress for status line
			if statusLine != nil && config.SchemaPath == "" {
				statusLine.UpdateStreamingProgress(responseText.Len())
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
			messageCommitted = true // Mark that we've committed a message

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
					fmt.Print(fullResponse.Content)
				} else if responseText.Len() > 0 && config.SchemaPath == "" {
					// Content was streamed, add a newline before tool output
					fmt.Println()
				}

				// For interactive mode, we need special handling of tool calls
				_, isInteractive := statusLine.(*InteractiveStatus)
				if isInteractive {
					// Interactive mode shows tool calls with spinner and completion status
					termOutput := termenv.NewOutput(os.Stdout)
					var successStyle, errorStyle termenv.Style
					
					// Adapt colors based on terminal background
					if termenv.HasDarkBackground() {
						successStyle = termOutput.String().Foreground(termOutput.Color("65"))  // Muted green for dark
						errorStyle = termOutput.String().Foreground(termOutput.Color("124"))   // Muted red for dark
					} else {
						successStyle = termOutput.String().Foreground(termOutput.Color("28"))  // Dark green for light
						errorStyle = termOutput.String().Foreground(termOutput.Color("160"))   // Dark red for light
					}
					
					for _, toolCall := range fullResponse.ToolCalls {
						// Execute with spinner showing
						success := executeToolCall(ctx, toolCall, registry, session, config, statusLine)
						
						// Clear spinner and show completion message
						statusLine.Clear()
						if success {
							fmt.Printf("%s Completed: %s\n", successStyle.Styled("✓"), toolCall.Name)
						} else {
							fmt.Printf("%s Failed: %s\n", errorStyle.Styled("✗"), toolCall.Name)
						}
					}
				} else {
					// Regular mode uses status updates
					processToolCalls(ctx, fullResponse.ToolCalls, registry, session, config, statusLine)
				}
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

// createCompletionRequest builds an LLM completion request from config
func createCompletionRequest(config *Config, session sessions.Session, registry *tools.ToolRegistry, schema *llm.Schema) *llm.CompletionRequest {
	return &llm.CompletionRequest{
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
}

// initializeConversation handles all the setup needed before starting a conversation
func initializeConversation(config *Config, sessionStore sessions.SessionStore, contextID string) (string, *sessions.ContextInfo, bool, error) {
	var needReset bool
	var originalContextInfo *sessions.ContextInfo

	// Resolve --last flag if specified
	if config.UseLastContext {
		if fileStore, ok := sessionStore.(*sessions.FileSessionStore); ok {
			contextID = fileStore.GetLastContext()
			if contextID == "" {
				return "", nil, false, fmt.Errorf("no last context found")
			}
		}
	}

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
			if config.MaxTokens == defaultMaxTokens && contextInfo.MaxTokens != 0 {
				config.MaxTokens = contextInfo.MaxTokens
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
				return "", nil, false, fmt.Errorf("failed to reset context: %w", err)
			}
			// Context name remains the same after reset
			contextID = contextName
		}
	}

	return contextID, originalContextInfo, needReset, nil
}

// updateContextInfo updates the context info with current settings
func updateContextInfo(sessionStore sessions.SessionStore, contextID string, config *Config) {
	if fileStore, ok := sessionStore.(*sessions.FileSessionStore); ok {
		if fileStore.GetContextByNameOrID(contextID) != nil {
			// This context has metadata, update its settings
			if contextInfo := fileStore.GetContextByNameOrID(contextID); contextInfo != nil {
				contextInfo.Model = config.Model
				contextInfo.Temperature = config.Temperature
				contextInfo.MaxTokens = config.MaxTokens
				contextInfo.SystemPrompt = config.SystemPrompt
				contextInfo.ToolPaths = config.ToolPaths
				contextInfo.MCPServers = config.MCPServers
				fileStore.SaveContextInfo(contextInfo)
			}
		}
	}
}
