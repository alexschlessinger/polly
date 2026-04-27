package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/urfave/cli/v3"
)

var (
	validModelProviders  = []string{"openai", "anthropic", "gemini", "ollama", "huggingface"}
	validEmbedProviders  = []string{"openai", "gemini"}
	purgeDisallowedFlags = []string{
		"context", "last", "prompt", "file", "model", "temp",
		"maxtokens", "maxiterations", "timeout", "tool", "mcp", "system", "schema",
		"tooltimeout", "maxcontext", "thinkingeffort", "baseurl",
		"skilldir", "skill", "noskills", "listskills",
	}
)

func getCommand() *cli.Command {
	flags, mutuallyExclusiveGroups := defineFlagsWithGroups()
	command := &cli.Command{
		Name:                   "polly",
		Usage:                  "Chat with LLMs using various providers",
		Flags:                  flags,
		MutuallyExclusiveFlags: mutuallyExclusiveGroups,
		Action:                 runCommand,
		Commands: []*cli.Command{
			embedCommand(),
		},
		OnUsageError: func(ctx context.Context, cmd *cli.Command, err error, isSubcommand bool) error {
			// Just return the error without showing usage
			return err
		},
	}
	return command
}

// parseConfig extracts configuration from command-line flags
func parseConfig(cmd *cli.Command) *Config {
	config := &Config{
		Settings: Settings{
			// Model configuration
			Model:            cmd.String("model"),
			Temperature:      cmd.Float64("temp"),
			MaxTokens:        cmd.Int("maxtokens"),
			MaxHistoryTokens: cmd.Int("maxcontext"),
			ThinkingEffort:   cmd.String("thinkingeffort"),
			SystemPrompt:     cmd.String("system"),
			ToolTimeout:      cmd.Duration("tooltimeout"),
			SkillDirs:        cmd.StringSlice("skilldir"),
		},

		// Runtime configuration
		Timeout:       cmd.Duration("timeout"),
		MaxIterations: int(cmd.Int("maxiterations")),
		BaseURL:       cmd.String("baseurl"),
		Confirm:       cmd.Bool("confirm"),
		NoSandbox:     cmd.Bool("nosandbox"),

		// Skill configuration
		NoSkills:   cmd.Bool("noskills"),
		ListSkills: cmd.Bool("listskills"),

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
		Skills:     cmd.StringSlice("skill"),
	}

	return config
}

// loadAPIKeys loads API keys from environment variables
func loadAPIKeys() map[string]string {
	return map[string]string{
		"ollama":      os.Getenv("POLLYTOOL_OLLAMAKEY"),
		"openai":      os.Getenv("POLLYTOOL_OPENAIKEY"),
		"anthropic":   os.Getenv("POLLYTOOL_ANTHROPICKEY"),
		"gemini":      os.Getenv("POLLYTOOL_GEMINIKEY"),
		"huggingface": os.Getenv("POLLYTOOL_HUGGINGFACEKEY"),
	}
}

func defineFlagsWithGroups() ([]cli.Flag, []cli.MutuallyExclusiveFlags) {
	resetFlag := newPromptAndFileFreeStringFlag("reset", "Reset the specified context (clear conversation history, keep settings)")
	purgeFlag := newPurgeFlag()
	createFlag := newCreateFlag()
	showFlag := newPromptAndFileFreeStringFlag("show", "Show configuration for the specified context")
	listFlag := newPromptAndFileFreeBoolFlag("list", "List all available context IDs")
	listSkillsFlag := newPromptAndFileFreeBoolFlag("listskills", "List discovered Agent Skills")
	deleteFlag := newPromptAndFileFreeStringFlag("delete", "Delete the specified context")
	addFlag := &cli.BoolFlag{
		Name:  "add",
		Usage: "Add stdin content to context without making an API call",
	}

	flags := append([]cli.Flag{}, modelConfigFlags()...)
	flags = append(flags, apiConfigFlags()...)
	flags = append(flags, skillConfigFlags(listSkillsFlag)...)
	flags = append(flags, toolConfigFlags()...)
	flags = append(flags, inputConfigFlags()...)
	flags = append(flags, contextManagementFlags(resetFlag, listFlag, deleteFlag, addFlag, purgeFlag, createFlag, showFlag)...)
	flags = append(flags, historyConfigFlags()...)
	flags = append(flags, approvalConfigFlags()...)
	flags = append(flags, sandboxConfigFlags()...)
	flags = append(flags, outputConfigFlags()...)

	return flags, []cli.MutuallyExclusiveFlags{
		{
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
	}
}

func modelConfigFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:    "model",
			Aliases: []string{"m"},
			Usage:   "Model to use (provider/model format)",
			Value:   "anthropic/claude-sonnet-4-6",
			Sources: cli.EnvVars("POLLYTOOL_MODEL"),
			Validator: func(model string) error {
				return validateModel(model)
			},
		},
		&cli.Float64Flag{
			Name:    "temp",
			Usage:   "Temperature for sampling",
			Value:   1.0,
			Sources: cli.EnvVars("POLLYTOOL_TEMP"),
			Validator: func(temp float64) error {
				return validateTemperature(temp)
			},
		},
		&cli.IntFlag{
			Name:    "maxtokens",
			Usage:   "Maximum tokens to generate",
			Value:   50000,
			Sources: cli.EnvVars("POLLYTOOL_MAXTOKENS"),
		},
		&cli.IntFlag{
			Name:    "maxiterations",
			Usage:   "Maximum agent iterations (LLM calls) before stopping",
			Value:   50,
			Sources: cli.EnvVars("POLLYTOOL_MAXITERATIONS"),
		},
		&cli.DurationFlag{
			Name:    "timeout",
			Usage:   "Request timeout",
			Value:   2 * time.Minute,
			Sources: cli.EnvVars("POLLYTOOL_TIMEOUT"),
		},
		newThinkingEffortFlag(),
	}
}

func apiConfigFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:    "baseurl",
			Usage:   "Base URL for API (for OpenAI-compatible endpoints or Ollama)",
			Value:   "",
			Sources: cli.EnvVars("POLLYTOOL_BASEURL"),
		},
	}
}

func skillConfigFlags(listSkillsFlag *cli.BoolFlag) []cli.Flag {
	return []cli.Flag{
		&cli.StringSliceFlag{
			Name:    "skilldir",
			Usage:   "Skill directory or directory containing skill folders (can be specified multiple times)",
			Sources: cli.EnvVars("POLLYTOOL_SKILLDIR"),
		},
		&cli.StringSliceFlag{
			Name:    "skill",
			Aliases: []string{"S"},
			Usage:   "Skill to load: local directory, git repo URL, or archive URL. Auto-activated on start.",
		},
		&cli.BoolFlag{
			Name:  "noskills",
			Usage: "Disable Agent Skill discovery and runtime skill tools",
		},
		listSkillsFlag,
	}
}

func toolConfigFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringSliceFlag{
			Name:    "tool",
			Aliases: []string{"t"},
			Usage:   "Tool provider: shell script (provides 1 tool) or MCP server (can provide multiple tools). Can be specified multiple times",
		},
		&cli.DurationFlag{
			Name:    "tooltimeout",
			Usage:   "Timeout for tool execution",
			Value:   30 * time.Second,
			Sources: cli.EnvVars("POLLYTOOL_TOOLTIMEOUT"),
		},
	}
}

func inputConfigFlags() []cli.Flag {
	return []cli.Flag{
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
	}
}

func contextManagementFlags(resetFlag *cli.StringFlag, listFlag *cli.BoolFlag, deleteFlag *cli.StringFlag, addFlag *cli.BoolFlag, purgeFlag *cli.BoolFlag, createFlag *cli.StringFlag, showFlag *cli.StringFlag) []cli.Flag {
	return []cli.Flag{
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
	}
}

func historyConfigFlags() []cli.Flag {
	return []cli.Flag{
		&cli.IntFlag{
			Name:  "maxcontext",
			Usage: "Maximum tokens to keep in history (0 = unlimited)",
			Value: 100000,
		},
	}
}

func approvalConfigFlags() []cli.Flag {
	return []cli.Flag{
		&cli.BoolFlag{
			Name:  "confirm",
			Usage: "Require confirmation before each tool call",
		},
	}
}

func sandboxConfigFlags() []cli.Flag {
	return []cli.Flag{
		&cli.BoolFlag{
			Name:    "nosandbox",
			Usage:   "Disable sandboxing of bash commands",
			Sources: cli.EnvVars("POLLYTOOL_NOSANDBOX"),
		},
	}
}

func outputConfigFlags() []cli.Flag {
	return []cli.Flag{
		&cli.BoolFlag{
			Name:  "quiet",
			Usage: "Suppress status and tool display output",
		},
		&cli.BoolFlag{
			Name:    "debug",
			Aliases: []string{"d"},
			Usage:   "Enable debug logging",
		},
	}
}

func newThinkingEffortFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:    "thinkingeffort",
		Usage:   "Thinking/reasoning effort level: off, low, medium, high",
		Value:   "off",
		Sources: cli.EnvVars("POLLYTOOL_THINKINGEFFORT"),
		Validator: func(v string) error {
			_, err := llm.ParseThinkingEffort(v)
			return err
		},
	}
}

func newPromptAndFileFreeStringFlag(name, usage string) *cli.StringFlag {
	return &cli.StringFlag{
		Name:  name,
		Usage: usage,
		Action: func(ctx context.Context, cmd *cli.Command, v string) error {
			return validateNoPromptOrFiles(cmd, name)
		},
	}
}

func newPromptAndFileFreeBoolFlag(name, usage string) *cli.BoolFlag {
	return &cli.BoolFlag{
		Name:  name,
		Usage: usage,
		Action: func(ctx context.Context, cmd *cli.Command, v bool) error {
			if !v {
				return nil
			}
			return validateNoPromptOrFiles(cmd, name)
		},
	}
}

func newCreateFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:  "create",
		Usage: "Create a new context with specified name and configuration",
		Action: func(ctx context.Context, cmd *cli.Command, v string) error {
			if cmd.String("prompt") != "" {
				return fmt.Errorf("--create does not take a prompt (use model/settings flags to configure)")
			}
			return nil
		},
	}
}

func newPurgeFlag() *cli.BoolFlag {
	return &cli.BoolFlag{
		Name:  "purge",
		Usage: "Delete all sessions and index (requires confirmation)",
		Action: func(ctx context.Context, cmd *cli.Command, v bool) error {
			if !v {
				return nil
			}
			if slices.ContainsFunc(purgeDisallowedFlags, cmd.IsSet) {
				return fmt.Errorf("--purge must be used alone (only --quiet or --debug allowed)")
			}
			return nil
		},
	}
}

func validateNoPromptOrFiles(cmd *cli.Command, flagName string) error {
	if cmd.String("prompt") != "" || len(cmd.StringSlice("file")) > 0 {
		return fmt.Errorf("--%s does not take prompts or files", flagName)
	}
	return nil
}

func validateModel(model string) error {
	return validateModelWithProviders(model, validModelProviders, "anthropic/claude-sonnet-4-6")
}

func validateEmbedModel(model string) error {
	return validateModelWithProviders(model, validEmbedProviders, "openai/text-embedding-3-large")
}

func validateModelWithProviders(model string, providers []string, example string) error {
	if model == "" {
		return nil
	}

	parts := strings.SplitN(model, "/", 2)
	if len(parts) != 2 {
		return fmt.Errorf("model must include provider prefix (e.g., %q). Got: %s", example, model)
	}

	provider := strings.ToLower(parts[0])
	if !slices.Contains(providers, provider) {
		return fmt.Errorf("unknown provider '%s'. Valid providers: %s", provider, strings.Join(providers, ", "))
	}

	return nil
}

func validateTemperature(temp float64) error {
	if temp < 0.0 || temp > 2.0 {
		return fmt.Errorf("temperature must be between 0.0 and 2.0, got %.1f", temp)
	}
	return nil
}
