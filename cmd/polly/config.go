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

// parseConfig extracts configuration from command-line flags
func parseConfig(cmd *cli.Command) *Config {
	// Handle thinking effort - check which flag is set
	var thinkingEffort string
	if cmd.Bool("think-hard") {
		thinkingEffort = "high"
	} else if cmd.Bool("think-medium") {
		thinkingEffort = "medium"
	} else if cmd.Bool("think") {
		thinkingEffort = "low"
	} else {
		// Default to "off" instead of empty string
		thinkingEffort = "off"
	}

	return &Config{
		Settings: Settings{
			// Model configuration
			Model:          cmd.String("model"),
			Temperature:    cmd.Float64("temp"),
			MaxTokens:      cmd.Int("maxtokens"),
			MaxHistory:     cmd.Int("maxhistory"),
			ThinkingEffort: thinkingEffort,
			SystemPrompt:   cmd.String("system"),

			// Tool configuration
			ToolPaths:   cmd.StringSlice("tool"),
			MCPServers:  cmd.StringSlice("mcp"),
			ToolTimeout: cmd.Duration("tooltimeout"),
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
	}
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

// validateTemperature checks if temperature is within valid range
func validateTemperature(temp float64) error {
	if temp < 0.0 || temp > 2.0 {
		return fmt.Errorf("temperature must be between 0.0 and 2.0, got %.1f", temp)
	}
	return nil
}

func defineFlags() []cli.Flag {
	return []cli.Flag{
		// Model configuration
		&cli.StringFlag{
			Name:    "model",
			Aliases: []string{"m"},
			Usage:   "Model to use (provider/model format)",
			Value:   "anthropic/claude-sonnet-4-20250514",
			Sources: cli.EnvVars("POLLYTOOL_MODEL"),
		},
		&cli.Float64Flag{
			Name:    "temp",
			Usage:   "Temperature for sampling",
			Value:   1.0,
			Sources: cli.EnvVars("POLLYTOOL_TEMP"),
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
		&cli.BoolFlag{
			Name:  "think",
			Usage: "Enable thinking/reasoning (low effort)",
			Value: false,
		},
		&cli.BoolFlag{
			Name:  "think-medium",
			Usage: "Enable thinking/reasoning (medium effort)",
			Value: false,
		},
		&cli.BoolFlag{
			Name:  "think-hard",
			Usage: "Enable thinking/reasoning (high effort)",
			Value: false,
		},

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
			Usage:   "Shell tool executable path (can be specified multiple times)",
		},
		&cli.StringSliceFlag{
			Name:  "mcp",
			Usage: "MCP server and arguments (can be specified multiple times)",
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
		// Check for any other non-output flags using IsSet
		if cmd.IsSet("context") || cmd.IsSet("last") ||
			cmd.IsSet("prompt") || cmd.IsSet("file") ||
			cmd.IsSet("model") || cmd.IsSet("temp") ||
			cmd.IsSet("maxtokens") || cmd.IsSet("timeout") ||
			cmd.IsSet("tool") || cmd.IsSet("mcp") ||
			cmd.IsSet("system") || cmd.IsSet("schema") ||
			cmd.IsSet("tooltimeout") || cmd.IsSet("maxhistory") ||
			cmd.IsSet("think") || cmd.IsSet("think-medium") || cmd.IsSet("think-hard") ||
			cmd.IsSet("baseurl") {
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

	// Validate model format and provider
	model := cmd.String("model")
	if model != "" {
		parts := strings.SplitN(model, "/", 2)
		if len(parts) != 2 {
			return ctx, fmt.Errorf("model must include provider prefix (e.g., 'openai/gpt-4o', 'anthropic/claude-sonnet-4-20250514'). Got: %s", model)
		}

		provider := strings.ToLower(parts[0])
		validProviders := []string{"openai", "anthropic", "gemini", "ollama"}
		if !slices.Contains(validProviders, provider) {
			return ctx, fmt.Errorf("unknown provider '%s'. Valid providers: %s", provider, strings.Join(validProviders, ", "))
		}
	}

	// Validate tool paths exist and are executable
	toolPaths := cmd.StringSlice("tool")
	for _, toolPath := range toolPaths {
		if _, err := os.Stat(toolPath); err != nil {
			if os.IsNotExist(err) {
				return ctx, fmt.Errorf("tool not found: %s", toolPath)
			}
			return ctx, fmt.Errorf("cannot access tool: %s (%v)", toolPath, err)
		}

		// Check if file is executable
		if info, err := os.Stat(toolPath); err == nil {
			if info.Mode()&0111 == 0 {
				return ctx, fmt.Errorf("tool is not executable: %s", toolPath)
			}
		}
	}

	// Validate temperature range
	if err := validateTemperature(cmd.Float64("temp")); err != nil {
		return ctx, err
	}

	return ctx, nil
}
