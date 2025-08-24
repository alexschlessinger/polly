package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

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

func runCommand(ctx context.Context, cmd *cli.Command) error {
	config := parseConfig(cmd)

	// Set up logging
	if !config.Debug {
		log.SetOutput(io.Discard)
	}

	// Get context ID from config
	contextID := config.ContextID

	// Context validation will now happen in SessionStore.Get() method

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
		contextID = sessionStore.GetLast()
		if contextID == "" {
			return fmt.Errorf("no last context found")
		}
	}

	// Handle --reset flag
	if config.ResetContext != "" {
		return handleResetContext(sessionStore, config, config.ResetContext)
	}

	// Handle context management operations
	if config.ListContexts {
		return handleListContexts(sessionStore)
	}
	if config.DeleteContext != "" {
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

	// Check if context exists and prompt if not
	if contextID != "" {
		contextID = checkAndPromptForMissingContext(sessionStore, contextID)
		if contextID == "" {
			return nil // User cancelled
		}
	}

	// Prepare for conversation
	return runConversation(ctx, config, sessionStore, contextID, cmd)
}

// initializeSession sets up everything needed for a conversation session
func initializeSession(config *Config, sessionStore sessions.SessionStore, contextID string, cmd *cli.Command) (string, sessions.Session, llm.LLM, *tools.ToolRegistry, error) {
	// Initialize conversation using helper function
	var err error
	contextID, _, err = initializeConversation(config, sessionStore, contextID, cmd)
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
	updateContextInfo(session, config, cmd)

	// Load tools
	toolRegistry, err := loadTools(config)
	if err != nil {
		session.Close()
		return "", nil, nil, nil, err
	}

	return contextID, session, multipass, toolRegistry, nil
}

func runConversation(ctx context.Context, config *Config, sessionStore sessions.SessionStore, contextID string, cmd *cli.Command) error {
	// Initialize session
	contextID, session, multipass, toolRegistry, err := initializeSession(config, sessionStore, contextID, cmd)
	if err != nil {
		return err
	}
	defer session.Close()

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
	var statusLine StatusHandler
	if status := createStatusLine(config); status != nil {
		statusLine = status
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
				fullResponse.Content = strings.TrimLeft(responseText.String(), "")
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
						successStyle = termOutput.String().Foreground(termOutput.Color("65")) // Muted green for dark
						errorStyle = termOutput.String().Foreground(termOutput.Color("124"))  // Muted red for dark
					} else {
						successStyle = termOutput.String().Foreground(termOutput.Color("28")) // Dark green for light
						errorStyle = termOutput.String().Foreground(termOutput.Color("160"))  // Dark red for light
					}

					for _, toolCall := range fullResponse.ToolCalls {
						// Execute with spinner showing
						success := executeToolCall(ctx, toolCall, registry, session, statusLine)

						// Clear spinner and show completion message
						statusLine.Clear()
						if success {
							fmt.Printf("%s Completed: %s\n", successStyle.Styled("✓"), toolCall.Name)
						} else {
							fmt.Printf("%s Failed: %s\n", errorStyle.Styled("✗"), toolCall.Name)
						}
					}
				} else {
					for _, toolCall := range fullResponse.ToolCalls {
						_ = executeToolCall(ctx, toolCall, registry, session, statusLine)
					}
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
			fmt.Println()
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
func initializeConversation(config *Config, sessionStore sessions.SessionStore, contextID string, cmd *cli.Command) (string, *sessions.Metadata, error) {
	var needReset bool
	var originalContextInfo *sessions.Metadata

	// Load context settings if available
	if contextID != "" {
		if contextInfo := sessionStore.GetAllMetadata()[contextID]; contextInfo != nil {
			originalContextInfo = contextInfo

			// Check if system prompt is being changed (only if context has existing conversation)
			if cmd.IsSet("system") && cmd.String("system") != contextInfo.SystemPrompt {
				// Check if there's an existing conversation to reset
				if sessionStore.Exists(contextInfo.Name) {
					needReset = true
					fmt.Fprintf(os.Stderr, "System prompt changed, resetting conversation...\n")
				}
			}

			// Use stored settings if not overridden by command line
			if !cmd.IsSet("model") && contextInfo.Model != "" {
				config.Model = contextInfo.Model
			}
			if !cmd.IsSet("temp") && contextInfo.Temperature != 0 {
				config.Temperature = contextInfo.Temperature
			}
			if !cmd.IsSet("maxtokens") && contextInfo.MaxTokens != 0 {
				config.MaxTokens = contextInfo.MaxTokens
			}
			if !cmd.IsSet("maxhistory") && contextInfo.MaxHistory != 0 {
				config.MaxHistory = contextInfo.MaxHistory
			}
			// Only use stored system prompt if flag wasn't explicitly set
			if !cmd.IsSet("system") && contextInfo.SystemPrompt != "" {
				config.SystemPrompt = contextInfo.SystemPrompt
			}
			// Apply stored tools if none provided via command line
			if !cmd.IsSet("tool") && len(contextInfo.ToolPaths) > 0 {
				config.ToolPaths = contextInfo.ToolPaths
			}
			if !cmd.IsSet("mcp") && len(contextInfo.MCPServers) > 0 {
				config.MCPServers = contextInfo.MCPServers
			}
			// Apply stored thinking effort if not provided via command line
			if !cmd.IsSet("think") && !cmd.IsSet("think-medium") && !cmd.IsSet("think-hard") && contextInfo.ThinkingEffort != "off" && contextInfo.ThinkingEffort != "" {
				config.ThinkingEffort = contextInfo.ThinkingEffort
			}
			// Apply stored tool timeout if not provided via command line
			if !cmd.IsSet("tooltimeout") && contextInfo.ToolTimeout > 0 {
				config.ToolTimeout = contextInfo.ToolTimeout
			}
		}
	}

	// Perform reset if system prompt changed
	if needReset && originalContextInfo != nil {
		// Get the context name
		contextName := contextID
		if originalContextInfo.Name != "" {
			contextName = originalContextInfo.Name
		}

		// Reset the context
		if err := resetContext(sessionStore, contextName); err != nil {
			return "", nil, fmt.Errorf("failed to reset context: %w", err)
		}
		// Context name remains the same after reset
		contextID = contextName
	}

	return contextID, originalContextInfo, nil
}

// updateContextInfo updates the context info with current settings
func updateContextInfo(session sessions.Session, config *Config, cmd *cli.Command) {
	// Build update struct - config already has correct values from:
	// 1. urfave defaults, OR
	// 2. User-provided flags, OR
	// 3. Loaded from existing context (via initializeConversation)
	update := &sessions.Metadata{
		Name:        session.GetName(),
		LastUsed:    time.Now(),
		Model:       config.Model,
		Temperature: config.Temperature,
		MaxTokens:   config.MaxTokens,
		ToolTimeout: config.ToolTimeout,
	}

	// Only update these if explicitly set via command line
	if cmd.IsSet("maxhistory") {
		update.MaxHistory = config.MaxHistory
	}
	if cmd.IsSet("system") {
		update.SystemPrompt = config.SystemPrompt
	}
	if len(config.ToolPaths) > 0 {
		update.ToolPaths = config.ToolPaths
	}
	if len(config.MCPServers) > 0 {
		update.MCPServers = config.MCPServers
	}
	if cmd.IsSet("think") || cmd.IsSet("think-medium") || cmd.IsSet("think-hard") {
		update.ThinkingEffort = config.ThinkingEffort
	}

	_ = session.UpdateMetadata(update)
}

// cleanupAndExit performs cleanup and exits with the given code
func cleanupAndExit(code int) {
	// Don't remove any lock files - they could belong to other processes
	// The flock library handles stale locks automatically
	os.Exit(code)
}

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
		// Cleanup before canceling context
		cleanupAndExit(130) // 128 + SIGINT(2) = 130
	}()
	return ctx, cancel
}

// outputStructured formats and outputs structured response
func outputStructured(content string, schema *llm.Schema) {
	// If content is already JSON, pretty-print it
	var data any
	if err := json.Unmarshal([]byte(content), &data); err == nil {
		// Validate against schema if provided
		if schema != nil {
			if err := validateJSONAgainstSchema(data, schema); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: Output doesn't match schema: %v\n", err)
			}
		}

		jsonBytes, _ := json.MarshalIndent(data, "", "  ")
		fmt.Println(string(jsonBytes))
	} else {
		// Fallback to raw output if not valid JSON
		fmt.Println(content)
	}
}

// createStatusLine creates a status line if appropriate
func createStatusLine(config *Config) *Status {
	// Use terminal title for status updates when in a terminal
	// Status line works fine with schema since it outputs to stderr
	if !config.Quiet && isTerminal() {
		return NewStatus()
	}

	// Return nil when status updates are not appropriate
	return nil
}
