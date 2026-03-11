package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/alexschlessinger/pollytool/internal/log"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/skills"
	"github.com/alexschlessinger/pollytool/tools"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
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

var newSandbox = sandbox.New

func newCommandRunner(ctx context.Context, cmd *cli.Command) (*commandRunner, error) {
	config := parseConfig(cmd)

	log.InitLogger(config.Debug)

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
	if cfg.ListSkills {
		return true, handleListSkills(cfg)
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
func initializeSession(config *Config, sessionStore sessions.SessionStore, contextID string, cmd *cli.Command) (string, sessions.Session, *llm.Agent, *tools.ToolRegistry, *skills.Catalog, *tools.SkillRuntime, *skillCatalogResult, error) {
	// Initialize conversation using helper function
	var err error
	contextID, _, err = initializeConversation(config, sessionStore, contextID, cmd)
	if err != nil {
		return "", nil, nil, nil, nil, nil, nil, err
	}

	// Load API keys
	apiKeys := loadAPIKeys()

	// Create LLM provider
	llmClient := llm.NewMultiPass(apiKeys)

	// Get or create session early so we can read persisted skill sources.
	needFileStore := needsFileStore(config, contextID)
	session := getOrCreateSession(sessionStore, contextID, needFileStore)
	metadata := session.GetMetadata()

	// Discover skills before building the runtime tool registry.
	// Pass persisted SkillSources so --skill is restored on session resume.
	skillResult, err := loadSkillCatalog(config, metadata.SkillSources)
	if err != nil {
		session.Close()
		return "", nil, nil, nil, nil, nil, nil, err
	}
	skillCatalog := skillResult.catalog

	// Persist skill sources for future session restores.
	if len(skillResult.sources) > 0 {
		metadata.SkillSources = skillResult.sources
		session.SetMetadata(metadata)
	}

	registryOpts, err := sandboxRegistryOptions(config)
	if err != nil {
		session.Close()
		return "", nil, nil, nil, nil, nil, nil, err
	}

	// Handle command-line tools if provided - they replace session tools
	var toolRegistry *tools.ToolRegistry

	if len(config.Tools) > 0 {
		// Load command-line tools directly into the registry we'll use
		toolRegistry = tools.NewToolRegistry(nil, registryOpts...)
		for _, source := range config.Tools {
			_, err := toolRegistry.LoadToolAuto(source)
			if err != nil {
				session.Close()
				return "", nil, nil, nil, nil, nil, nil, fmt.Errorf("failed to load tool %s: %w", source, err)
			}
		}
		// Store the metadata for persistence
		metadata.ActiveTools = toolRegistry.GetActiveToolLoaders()
		session.SetMetadata(metadata)
	} else {
		// Load tools from session metadata
		toolRegistry, err = loadTools(metadata.ActiveTools, registryOpts...)
		if err != nil {
			session.Close()
			return "", nil, nil, nil, nil, nil, nil, err
		}
	}
	skillRuntime, err := newSkillRuntime(skillCatalog, toolRegistry)
	if err != nil {
		session.Close()
		return "", nil, nil, nil, nil, nil, nil, err
	}
	if err := restoreActiveSkills(metadata, skillRuntime); err != nil {
		if toolRegistry != nil {
			_ = toolRegistry.Close()
		}
		session.Close()
		return "", nil, nil, nil, nil, nil, nil, err
	}
	if err := autoActivateSkills(skillResult.autoActivate, skillRuntime); err != nil {
		if toolRegistry != nil {
			_ = toolRegistry.Close()
		}
		session.Close()
		return "", nil, nil, nil, nil, nil, nil, err
	}

	// Update context info with current settings using helper function
	updateContextInfo(session, config, cmd)

	// Create the agent with the tool registry
	agent := llm.NewAgent(llmClient, toolRegistry, llm.AgentConfig{
		MaxIterations: config.MaxIterations,
		ToolTimeout:   config.ToolTimeout,
	})

	return contextID, session, agent, toolRegistry, skillCatalog, skillRuntime, skillResult, nil
}

func sandboxRegistryOptions(config *Config) ([]tools.RegistryOption, error) {
	if config.NoSandbox {
		return nil, nil
	}

	baseCfg := sandbox.DefaultConfig()

	// Validate that the backend works before proceeding.
	if _, err := newSandbox(baseCfg); err != nil {
		return nil, fmt.Errorf("sandbox requested but unavailable: %w", err)
	}

	return []tools.RegistryOption{tools.WithSandboxFactory(newSandbox, baseCfg)}, nil
}

func runConversation(ctx context.Context, config *Config, sessionStore sessions.SessionStore, contextID string, cmd *cli.Command) error {
	// Initialize session
	contextID, session, agent, toolRegistry, skillCatalog, skillRuntime, skillResult, err := initializeSession(config, sessionStore, contextID, cmd)
	if err != nil {
		return err
	}
	defer session.Close()
	if toolRegistry != nil {
		defer toolRegistry.Close()
	}

	// Get prompt early to determine if we're going to interactive mode
	prompt, err := getPrompt(config)
	if err != nil {
		return err
	}

	// If no prompt provided and no stdin, return error as interactive mode is disabled
	if prompt == "" {
		return fmt.Errorf("no prompt provided. Please provide a prompt via -p flag or stdin")
	}

	// Set up signal handling
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
	req := createCompletionRequest(config, session, toolRegistry, skillCatalog, schema)

	// Track whether we need a newline before the next content block
	// (i.e., tool calls happened since the last content output)
	needsNewline := false
	contentPrinted := false

	// Set up tool approval if --confirm is active
	var approver *toolApprover
	if config.Confirm && isTerminal() {
		approver = &toolApprover{}
	}

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
				if needsNewline {
					fmt.Println()
					needsNewline = false
				}
				fmt.Print(content)
				contentPrinted = true
			}
		},
		OnToolStart: func(calls []messages.ChatMessageToolCall) {
			needsNewline = true
			if toolDisplayEnabled(config) {
				if contentPrinted {
					fmt.Fprintln(os.Stderr)
					contentPrinted = false
				}
				for _, tc := range calls {
					printToolStart(tc)
				}
			}
			if statusLine != nil && len(calls) > 0 && approver == nil {
				statusLine.ShowToolCall(calls[0].Name)
			}
		},
		ApproveToolCalls: func() func([]messages.ChatMessageToolCall) []bool {
			if approver != nil {
				return func(calls []messages.ChatMessageToolCall) []bool {
					if statusLine != nil {
						statusLine.Clear()
					}
					return approver.approveToolCalls(calls)
				}
			}
			return nil
		}(),
		OnToolEnd: func(tc messages.ChatMessageToolCall, result string, duration time.Duration, err error) {
			if statusLine != nil {
				statusLine.Clear()
			}
			if toolDisplayEnabled(config) {
				printToolEnd(tc, duration, err)
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
	if err := persistActiveSkills(session, skillRuntime, skillResult.sources); err != nil {
		return fmt.Errorf("failed to persist active skills: %w", err)
	}

	if err != nil {
		return err
	}

	// Warn if response was truncated due to token limit
	if resp.Message != nil && resp.Message.StopReason == messages.StopReasonMaxTokens {
		fmt.Fprintf(os.Stderr, "\nWarning: response truncated (hit %d token limit, use --maxtokens to increase)\n", config.MaxTokens)
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
func createCompletionRequest(config *Config, session sessions.Session, registry *tools.ToolRegistry, skillCatalog *skills.Catalog, schema *llm.Schema) *llm.CompletionRequest {
	// Parse thinking effort - already validated at config parsing time
	thinkingEffort, _ := llm.ParseThinkingEffort(config.ThinkingEffort)

	return &llm.CompletionRequest{
		BaseURL:        config.BaseURL,
		Timeout:        config.Timeout,
		Temperature:    float32(config.Temperature),
		Model:          config.Model,
		MaxTokens:      config.MaxTokens,
		Messages:       session.GetHistory(),
		Skills:         skillCatalog,
		Tools:          registry.All(),
		ResponseSchema: schema,
		ThinkingEffort: thinkingEffort,
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
			if !cmd.IsSet("maxcontext") && contextInfo.MaxHistoryTokens != 0 {
				config.Settings.MaxHistoryTokens = contextInfo.MaxHistoryTokens
			}
			// Only use stored system prompt if flag wasn't explicitly set
			if !cmd.IsSet("system") && contextInfo.SystemPrompt != "" {
				config.Settings.SystemPrompt = contextInfo.SystemPrompt
			}
			// Tools are now handled directly with session metadata in initializeSession
			// Apply stored thinking effort if not provided via command line
			if !cmd.IsSet("thinkingeffort") && contextInfo.ThinkingEffort != "off" && contextInfo.ThinkingEffort != "" {
				config.Settings.ThinkingEffort = contextInfo.ThinkingEffort
			}
			// Apply stored tool timeout if not provided via command line
			if !cmd.IsSet("tooltimeout") && contextInfo.ToolTimeout > 0 {
				config.Settings.ToolTimeout = contextInfo.ToolTimeout
			}
			if !cmd.IsSet("maxiterations") && contextInfo.MaxIterations > 0 {
				config.MaxIterations = contextInfo.MaxIterations
			}
			if !cmd.IsSet("skilldir") && len(contextInfo.SkillDirs) > 0 {
				config.Settings.SkillDirs = contextInfo.SkillDirs
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
		Name:          session.GetName(),
		LastUsed:      time.Now(),
		Model:         config.Settings.Model,
		Temperature:   config.Settings.Temperature,
		MaxTokens:     config.Settings.MaxTokens,
		MaxIterations: config.MaxIterations,
		ToolTimeout:   config.Settings.ToolTimeout,
		SkillDirs:     config.Settings.SkillDirs,
	}

	// Only update these if explicitly set via command line
	if cmd.IsSet("maxcontext") {
		update.MaxHistoryTokens = config.Settings.MaxHistoryTokens
	}
	if cmd.IsSet("system") {
		update.SystemPrompt = config.Settings.SystemPrompt
	}
	// Tools are already handled in initializeSession, no need to update here
	if cmd.IsSet("thinkingeffort") {
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
