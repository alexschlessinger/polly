package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/urfave/cli/v3"
)

func getCommand() *cli.Command {
	flags, mutuallyExclusiveGroups := defineFlagsWithGroups()
	command := &cli.Command{
		Name:                   "polly",
		Usage:                  "Chat with LLMs using various providers",
		Flags:                  flags,
		MutuallyExclusiveFlags: mutuallyExclusiveGroups,
		Action:                 runCommand,
		OnUsageError: func(ctx context.Context, cmd *cli.Command, err error, isSubcommand bool) error {
			// Just return the error without showing usage
			return err
		},
	}
	return command
}

// parseConfig extracts configuration from command-line flags
func parseConfig(cmd *cli.Command) *Config {
	// Handle thinking effort - only one can be set due to MutuallyExclusiveFlags
	thinkingEffort := "off"
	switch {
	case cmd.Bool("think-hard"):
		thinkingEffort = "high"
	case cmd.Bool("think-medium"):
		thinkingEffort = "medium"
	case cmd.Bool("think"):
		thinkingEffort = "low"
	}

	config := &Config{
		Settings: Settings{
			// Model configuration
			Model:          cmd.String("model"),
			Temperature:    cmd.Float64("temp"),
			MaxTokens:      cmd.Int("maxtokens"),
			MaxHistory:     cmd.Int("maxhistory"),
			ThinkingEffort: thinkingEffort,
			SystemPrompt:   cmd.String("system"),
			ToolTimeout:    cmd.Duration("tooltimeout"),
		},

		// Runtime configuration
		Timeout: cmd.Duration("timeout"),
		BaseURL: cmd.String("baseurl"),

		// Context operations
		ContextID:      cmd.String("context"),
		ResetContext:   cmd.String("reset"),
		UseLastContext: cmd.Bool("last"),
		ListContexts:   cmd.Bool("list"),
		DeleteContext:  cmd.String("delete"),
		AddToContext:   cmd.Bool("add"),
		PurgeAll:       cmd.Bool("purge"),
		CreateContext:  cmd.String("create"),
		ShowContext:    cmd.String("show"),

		// Input/Output configuration
		Prompt:     cmd.String("prompt"),
		Files:      cmd.StringSlice("file"),
		SchemaPath: cmd.String("schema"),
		Quiet:      cmd.Bool("quiet"),
		Debug:      cmd.Bool("debug"),
		Tools:      cmd.StringSlice("tool"),
	}

	return config
}

// loadAPIKeys loads API keys from environment variables
func loadAPIKeys() map[string]string {
	return map[string]string{
		"ollama":    os.Getenv("POLLYTOOL_OLLAMAKEY"),
		"openai":    os.Getenv("POLLYTOOL_OPENAIKEY"),
		"anthropic": os.Getenv("POLLYTOOL_ANTHROPICKEY"),
		"gemini":    os.Getenv("POLLYTOOL_GEMINIKEY"),
	}
}

func defineFlagsWithGroups() ([]cli.Flag, []cli.MutuallyExclusiveFlags) {
	// Define all flags
	resetFlag := &cli.StringFlag{
		Name:  "reset",
		Usage: "Reset the specified context (clear conversation history, keep settings)",
		Action: func(ctx context.Context, cmd *cli.Command, v string) error {
			if cmd.String("prompt") != "" || len(cmd.StringSlice("file")) > 0 {
				return fmt.Errorf("--reset does not take prompts or files")
			}
			return nil
		},
	}
	purgeFlag := &cli.BoolFlag{
		Name:  "purge",
		Usage: "Delete all sessions and index (requires confirmation)",
		Action: func(ctx context.Context, cmd *cli.Command, v bool) error {
			if !v {
				return nil
			}
			// List of flags that are NOT allowed with purge
			disallowedFlags := []string{
				"context", "last", "prompt", "file", "model", "temp",
				"maxtokens", "timeout", "tool", "mcp", "system", "schema",
				"tooltimeout", "maxhistory", "think", "think-medium",
				"think-hard", "baseurl",
			}
			if slices.ContainsFunc(disallowedFlags, cmd.IsSet) {
				return fmt.Errorf("--purge must be used alone (only --quiet or --debug allowed)")
			}
			return nil
		},
	}
	createFlag := &cli.StringFlag{
		Name:  "create",
		Usage: "Create a new context with specified name and configuration",
		Action: func(ctx context.Context, cmd *cli.Command, v string) error {
			if cmd.String("prompt") != "" {
				return fmt.Errorf("--create does not take a prompt (use model/settings flags to configure)")
			}
			return nil
		},
	}
	showFlag := &cli.StringFlag{
		Name:  "show",
		Usage: "Show configuration for the specified context",
		Action: func(ctx context.Context, cmd *cli.Command, v string) error {
			if cmd.String("prompt") != "" || len(cmd.StringSlice("file")) > 0 {
				return fmt.Errorf("--show does not take prompts or files")
			}
			return nil
		},
	}
	listFlag := &cli.BoolFlag{
		Name:  "list",
		Usage: "List all available context IDs",
		Action: func(ctx context.Context, cmd *cli.Command, v bool) error {
			if v && (cmd.String("prompt") != "" || len(cmd.StringSlice("file")) > 0) {
				return fmt.Errorf("--list does not take prompts or files")
			}
			return nil
		},
	}
	deleteFlag := &cli.StringFlag{
		Name:  "delete",
		Usage: "Delete the specified context",
		Action: func(ctx context.Context, cmd *cli.Command, v string) error {
			if cmd.String("prompt") != "" || len(cmd.StringSlice("file")) > 0 {
				return fmt.Errorf("--delete does not take prompts or files")
			}
			return nil
		},
	}
	addFlag := &cli.BoolFlag{
		Name:  "add",
		Usage: "Add stdin content to context without making an API call",
	}

	// Define thinking flags
	thinkFlag := &cli.BoolFlag{
		Name:  "think",
		Usage: "Enable thinking/reasoning (low effort)",
		Value: false,
	}
	thinkMediumFlag := &cli.BoolFlag{
		Name:  "think-medium",
		Usage: "Enable thinking/reasoning (medium effort)",
		Value: false,
	}
	thinkHardFlag := &cli.BoolFlag{
		Name:  "think-hard",
		Usage: "Enable thinking/reasoning (high effort)",
		Value: false,
	}

	flags := []cli.Flag{
		// Model configuration
		&cli.StringFlag{
			Name:    "model",
			Aliases: []string{"m"},
			Usage:   "Model to use (provider/model format)",
			Value:   "anthropic/claude-sonnet-4-20250514",
			Sources: cli.EnvVars("POLLYTOOL_MODEL"),
			Validator: func(model string) error {
				if model == "" {
					return nil // empty is allowed, uses default
				}
				parts := strings.SplitN(model, "/", 2)
				if len(parts) != 2 {
					return fmt.Errorf("model must include provider prefix (e.g., 'openai/gpt-4o', 'anthropic/claude-sonnet-4-20250514'). Got: %s", model)
				}
				provider := strings.ToLower(parts[0])
				validProviders := []string{"openai", "anthropic", "gemini", "ollama"}
				if !slices.Contains(validProviders, provider) {
					return fmt.Errorf("unknown provider '%s'. Valid providers: %s", provider, strings.Join(validProviders, ", "))
				}
				return nil
			},
		},
		&cli.Float64Flag{
			Name:    "temp",
			Usage:   "Temperature for sampling",
			Value:   1.0,
			Sources: cli.EnvVars("POLLYTOOL_TEMP"),
			Validator: func(temp float64) error {
				if temp < 0.0 || temp > 2.0 {
					return fmt.Errorf("temperature must be between 0.0 and 2.0, got %.1f", temp)
				}
				return nil
			},
		},
		&cli.IntFlag{
			Name:    "maxtokens",
			Usage:   "Maximum tokens to generate",
			Value:   4096,
			Sources: cli.EnvVars("POLLYTOOL_MAXTOKENS"),
		},
		&cli.DurationFlag{
			Name:    "timeout",
			Usage:   "Request timeout",
			Value:   2 * time.Minute,
			Sources: cli.EnvVars("POLLYTOOL_TIMEOUT"),
		},
		thinkFlag,
		thinkMediumFlag,
		thinkHardFlag,

		// API configuration
		&cli.StringFlag{
			Name:    "baseurl",
			Usage:   "Base URL for API (for OpenAI-compatible endpoints or Ollama)",
			Value:   "",
			Sources: cli.EnvVars("POLLYTOOL_BASEURL"),
		},

		// Tool configuration
		&cli.StringSliceFlag{
			Name:    "tool",
			Aliases: []string{"t"},
			Usage:   "Tool provider: shell script (provides 1 tool) or MCP server (can provide multiple tools). Can be specified multiple times",
			// Note: validation removed since we now auto-detect tool type
			// Shell tools will be validated when loaded, MCP servers can be JSON files or commands
		},
		&cli.DurationFlag{
			Name:    "tooltimeout",
			Usage:   "Timeout for tool execution",
			Value:   30 * time.Second,
			Sources: cli.EnvVars("POLLYTOOL_TOOLTIMEOUT"),
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
			Value:   "Your output will be displayed in a unix terminal. Be terse, 512 characters max. Do not use markdown.",
			Sources: cli.EnvVars("POLLYTOOL_SYSTEM"),
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
			Usage:   "Context name for conversation continuity",
			Sources: cli.EnvVars("POLLYTOOL_CONTEXT"),
		},
		&cli.BoolFlag{
			Name:    "last",
			Aliases: []string{"L"},
			Usage:   "Use the last active context",
		},
		resetFlag,
		listFlag,
		deleteFlag,
		addFlag,
		purgeFlag,
		createFlag,
		showFlag,

		// History configuration
		&cli.IntFlag{
			Name:  "maxhistory",
			Usage: "Maximum messages to keep in history (0 = unlimited)",
			Value: 0,
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

	// Define mutually exclusive groups
	mutuallyExclusiveGroups := []cli.MutuallyExclusiveFlags{
		{
			// Context operations are mutually exclusive
			Flags: [][]cli.Flag{
				{resetFlag},
				{purgeFlag},
				{createFlag},
				{showFlag},
				{listFlag},
				{deleteFlag},
				{addFlag},
			},
		},
		{
			// Thinking modes are mutually exclusive
			Flags: [][]cli.Flag{
				{thinkFlag},
				{thinkMediumFlag},
				{thinkHardFlag},
			},
		},
	}

	return flags, mutuallyExclusiveGroups
}
