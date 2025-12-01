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
	"github.com/urfave/cli/v3"
)

func main() {
	command := getCommand()
	if err := command.Run(context.Background(), os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		cleanupAndExit(1)
	}
}

type commandRunner struct {
	ctx          context.Context
	cmd          *cli.Command
	config       *Config
	sessionStore sessions.SessionStore
	contextID    string
}

func newCommandRunner(ctx context.Context, cmd *cli.Command) (*commandRunner, error) {
	config := parseConfig(cmd)

	if !config.Debug {
		log.SetOutput(io.Discard)
	}

	contextID := config.ContextID
	if config.UseLastContext {
		contextID = ""
	}

	sessionStore, err := setupSessionStore(config, contextID)
	if err != nil {
		return nil, fmt.Errorf("failed to create context store: %w", err)
	}

	if config.UseLastContext {
		contextID = sessionStore.GetLast()
		if contextID == "" {
			return nil, fmt.Errorf("no last context found")
		}
	}

	return &commandRunner{
		ctx:          ctx,
		cmd:          cmd,
		config:       config,
		sessionStore: sessionStore,
		contextID:    contextID,
	}, nil
}

func (r *commandRunner) Run() error {
	handled, err := r.handleManagementFlags()
	if err != nil {
		return err
	}
	if handled {
		return nil
	}

	if r.contextID != "" {
		contextID := checkAndPromptForMissingContext(r.sessionStore, r.contextID)
		if contextID == "" {
			return nil
		}
		r.contextID = contextID
	}

	return runConversation(r.ctx, r.config, r.sessionStore, r.contextID, r.cmd)
}

func (r *commandRunner) handleManagementFlags() (bool, error) {
	cfg := r.config
	store := r.sessionStore

	if cfg.ResetContext != "" {
		return true, handleResetContext(store, cfg, cfg.ResetContext)
	}
	if cfg.ListContexts {
		return true, handleListContexts(store)
	}
	if cfg.DeleteContext != "" {
		return true, handleDeleteContext(store, cfg.DeleteContext)
	}
	if cfg.AddToContext {
		return true, handleAddToContext(store, cfg, r.contextID)
	}
	if cfg.PurgeAll {
		return true, handlePurgeAll(store)
	}
	if cfg.CreateContext != "" {
		return true, handleCreateContext(store, cfg, cfg.CreateContext)
	}
	if cfg.ShowContext != "" {
		return true, handleShowContext(store, cfg.ShowContext)
	}

	return false, nil
}

func runCommand(ctx context.Context, cmd *cli.Command) error {
	runner, err := newCommandRunner(ctx, cmd)
	if err != nil {
		return err
	}

	return runner.Run()
}

// initializeSession sets up everything needed for a conversation session
func initializeSession(config *Config, sessionStore sessions.SessionStore, contextID string, cmd *cli.Command) (string, sessions.Session, *llm.Agent, *tools.ToolRegistry, error) {
	// Initialize conversation using helper function
	var err error
	contextID, _, err = initializeConversation(config, sessionStore, contextID, cmd)
	if err != nil {
		return "", nil, nil, nil, err
	}

	// Load API keys
	apiKeys := loadAPIKeys()

	// Create LLM provider
	llmClient := llm.NewMultiPass(apiKeys)

	// Get or create session
	needFileStore := needsFileStore(config, contextID)
	session := getOrCreateSession(sessionStore, contextID, needFileStore)

	// Handle command-line tools if provided - they replace session tools
	metadata := session.GetMetadata()
	var toolRegistry *tools.ToolRegistry

	if len(config.Tools) > 0 {
		// Load command-line tools directly into the registry we'll use
		toolRegistry = tools.NewToolRegistry(nil)
		for _, source := range config.Tools {
			_, err := toolRegistry.LoadToolAuto(source)
			if err != nil {
				session.Close()
				return "", nil, nil, nil, fmt.Errorf("failed to load tool %s: %w", source, err)
			}
		}
		// Store the metadata for persistence
		metadata.ActiveTools = toolRegistry.GetActiveToolLoaders()
		session.SetMetadata(metadata)
	} else {
		// Load tools from session metadata
		toolRegistry, err = loadTools(metadata.ActiveTools)
		if err != nil {
			session.Close()
			return "", nil, nil, nil, err
		}
	}

	// Update context info with current settings using helper function
	updateContextInfo(session, config, cmd)

	// Create the agent with the tool registry
	agent := llm.NewAgent(llmClient, toolRegistry, llm.AgentConfig{
		MaxIterations: 10,
		ToolTimeout:   config.ToolTimeout,
	})

	return contextID, session, agent, toolRegistry, nil
}

func runConversation(ctx context.Context, config *Config, sessionStore sessions.SessionStore, contextID string, cmd *cli.Command) error {
	// Initialize session
	contextID, session, agent, toolRegistry, err := initializeSession(config, sessionStore, contextID, cmd)
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
		return runInteractiveMode(ctx, config, session, agent, toolRegistry, contextID)
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

	// Add user message to session
	session.AddMessage(userMsg)

	// Create status line if appropriate
	var statusLine StatusHandler
	if status := createStatusLine(config); status != nil {
		statusLine = status
		statusLine.Start()
		defer statusLine.Stop()
	}

	// Show initial spinner
	if statusLine != nil {
		statusLine.ShowSpinner("waiting")
	}

	// Create completion request
	req := createCompletionRequest(config, session, toolRegistry, schema)

	// Run completion using the agent
	resp, err := agent.Run(ctx, req, &llm.AgentCallbacks{
		OnReasoning: func(content string) {
			if statusLine != nil {
				// Track total reasoning length for status
				statusLine.UpdateThinkingProgress(len(content))
			}
		},
		OnContent: func(content string) {
			if statusLine != nil {
				statusLine.ClearForContent()
			}
			// Print content unless using schema mode
			if config.SchemaPath == "" {
				fmt.Print(content)
			}
		},
		OnToolStart: func(tc messages.ChatMessageToolCall) {
			if statusLine != nil {
				statusLine.ShowToolCall(tc.Name)
			}
		},
		OnToolEnd: func(tc messages.ChatMessageToolCall, result string, duration time.Duration, err error) {
			if statusLine != nil {
				statusLine.Clear()
			}
		},
		OnError: func(err error) {
			if statusLine != nil {
				statusLine.Clear()
			}
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		},
	})

	// Add all generated messages to session, even if there was an error
	if resp != nil {
		for _, msg := range resp.AllMessages {
			session.AddMessage(msg)
		}
	}

	if err != nil {
		return err
	}

	// Output final result
	if config.SchemaPath != "" {
		outputStructured(resp.Message.Content, schema)
	} else {
		fmt.Println() // Final newline
	}

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
				config.Settings.Model = contextInfo.Model
			}
			if !cmd.IsSet("temp") && contextInfo.Temperature != 0 {
				config.Settings.Temperature = contextInfo.Temperature
			}
			if !cmd.IsSet("maxtokens") && contextInfo.MaxTokens != 0 {
				config.Settings.MaxTokens = contextInfo.MaxTokens
			}
			if !cmd.IsSet("maxhistory") && contextInfo.MaxHistory != 0 {
				config.Settings.MaxHistory = contextInfo.MaxHistory
			}
			// Only use stored system prompt if flag wasn't explicitly set
			if !cmd.IsSet("system") && contextInfo.SystemPrompt != "" {
				config.Settings.SystemPrompt = contextInfo.SystemPrompt
			}
			// Tools are now handled directly with session metadata in initializeSession
			// Apply stored thinking effort if not provided via command line
			if !cmd.IsSet("think") && !cmd.IsSet("think-medium") && !cmd.IsSet("think-hard") && contextInfo.ThinkingEffort != "off" && contextInfo.ThinkingEffort != "" {
				config.Settings.ThinkingEffort = contextInfo.ThinkingEffort
			}
			// Apply stored tool timeout if not provided via command line
			if !cmd.IsSet("tooltimeout") && contextInfo.ToolTimeout > 0 {
				config.Settings.ToolTimeout = contextInfo.ToolTimeout
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
		Model:       config.Settings.Model,
		Temperature: config.Settings.Temperature,
		MaxTokens:   config.Settings.MaxTokens,
		ToolTimeout: config.Settings.ToolTimeout,
	}

	// Only update these if explicitly set via command line
	if cmd.IsSet("maxhistory") {
		update.MaxHistory = config.Settings.MaxHistory
	}
	if cmd.IsSet("system") {
		update.SystemPrompt = config.Settings.SystemPrompt
	}
	// Tools are already handled in initializeSession, no need to update here
	if cmd.IsSet("think") || cmd.IsSet("think-medium") || cmd.IsSet("think-hard") {
		update.ThinkingEffort = config.Settings.ThinkingEffort
	}

	_ = session.UpdateMetadata(update)
}

// cleanupAndExit performs cleanup and exits with the given code
func cleanupAndExit(code int) {

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
