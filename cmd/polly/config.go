package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
	"github.com/urfave/cli/v3"
)

// defaultSandboxPreset is the sandbox policy when --sandbox is not given:
// the working directory is writable, outbound network is allowed, and Git
// works — .git stays writable with only its dangerous leaves (config, hooks,
// routing pointers) pinned read-only. Tighten with e.g. --sandbox
// workspace+net (whole .git read-only) or --sandbox base.
const defaultSandboxPreset = "workspace+net+git"

var (
	validModelProviders = []string{"openai", "anthropic", "gemini", "ollama", "huggingface", "deepseek", "openrouter"}
	validEmbedProviders = []string{"openai", "gemini"}
	// purgeCompanionFlags are the only flags --purge accepts alongside
	// itself; any other flag that is set is an error.
	purgeCompanionFlags = []string{"purge", "quiet", "debug"}
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

// parseConfig extracts configuration from command-line flags. Settings flags
// are read through the spec table; only runtime configuration is hand-listed.
func parseConfig(cmd *cli.Command) *Config {
	config := &Config{
		// Runtime configuration
		Timeout:       cmd.Duration("timeout"),
		Deadline:      cmd.Duration("deadline"),
		BaseURL:       cmd.String("baseurl"),
		Confirm:       cmd.Bool("confirm"),
		NoSandbox:     cmd.Bool("nosandbox"),
		SandboxPreset: cmd.String("sandbox"),
		DenyPaths:     cmd.StringSlice("denypath"),
		WritePaths:    cmd.StringSlice("writepath"),
		AllowNet:      cmd.Bool("allownet"),

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
		PromptSet:  cmd.IsSet("prompt"),
		Files:      cmd.StringSlice("file"),
		SchemaPath: cmd.String("schema"),
		Meta:       cmd.Bool("meta"),
		Quiet:      cmd.Bool("quiet"),
		Debug:      cmd.Bool("debug"),
		Tools:      cmd.StringSlice("tool"),
		Skills:     cmd.StringSlice("skill"),
	}
	for _, spec := range settingSpecs {
		if spec.fromCmd != nil {
			spec.fromCmd(config, cmd)
		}
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
		"deepseek":    os.Getenv("POLLYTOOL_DEEPSEEKKEY"),
		"openrouter":  os.Getenv("POLLYTOOL_OPENROUTERKEY"),
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
	flags = append(flags, contextManagementFlags()...)
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
			Name:      "maxtokens",
			Usage:     "Maximum tokens to generate",
			Value:     64000,
			Sources:   cli.EnvVars("POLLYTOOL_MAXTOKENS"),
			Validator: validateMaxTokens,
		},
		&cli.IntFlag{
			Name:    "maxiterations",
			Usage:   "Maximum agent iterations (LLM calls) before stopping",
			Value:   250,
			Sources: cli.EnvVars("POLLYTOOL_MAXITERATIONS"),
		},
		&cli.DurationFlag{
			Name:    "timeout",
			Usage:   "Stream stall timeout: cancel a request after this long with no provider data (0 disables)",
			Value:   30 * time.Minute,
			Sources: cli.EnvVars("POLLYTOOL_TIMEOUT"),
		},
		&cli.DurationFlag{
			Name:    "deadline",
			Usage:   "Hard per-request ceiling: cancel a request after this total time even if data is still arriving (0 = no ceiling)",
			Value:   2 * time.Hour,
			Sources: cli.EnvVars("POLLYTOOL_DEADLINE"),
		},
		newThinkingFlag(),
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
			Name:      "tooltimeout",
			Usage:     "Timeout for tool execution",
			Value:     5 * time.Minute,
			Sources:   cli.EnvVars("POLLYTOOL_TOOLTIMEOUT"),
			Validator: validateToolTimeout,
		},
	}
}

func inputConfigFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:    "prompt",
			Aliases: []string{"p"},
			Usage:   "Initial prompt (reads from stdin if not provided; starts REPL when neither is provided)",
		},
		&cli.StringFlag{
			Name:    "system",
			Aliases: []string{"s"},
			Usage:   "System prompt (persona; a per-frontend display contract is added automatically)",
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

func contextManagementFlags() []cli.Flag {
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
	}
}

func historyConfigFlags() []cli.Flag {
	return []cli.Flag{
		&cli.IntFlag{
			Name:      "maxcontext",
			Usage:     "Maximum estimated tokens sent to the model, clamped to the model's advertised context window when discoverable; full history is retained (0 = unlimited, never clamped)",
			Value:     256000,
			Validator: validateMaxContext,
		},
	}
}

func approvalConfigFlags() []cli.Flag {
	return []cli.Flag{
		&cli.BoolFlag{
			Name:        "confirm",
			Usage:       "Require confirmation before each tool call",
			DefaultText: "false",
		},
	}
}

func sandboxConfigFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:    "sandbox",
			Usage:   "Sandbox preset: base, readonly, workspace, git, net, ssh, sshkeys — join with + (e.g. workspace+net+git+ssh); git requires workspace",
			Value:   defaultSandboxPreset,
			Sources: cli.EnvVars("POLLYTOOL_SANDBOX"),
			Validator: func(spec string) error {
				return validateSandboxPresetSpec(spec)
			},
		},
		&cli.BoolFlag{
			Name:    "nosandbox",
			Usage:   "Disable sandboxing of tool commands",
			Sources: cli.EnvVars("POLLYTOOL_NOSANDBOX"),
		},
		&cli.StringSliceFlag{
			Name:    "denypath",
			Usage:   "Additional path blocked from sandboxed reads (repeatable, supports ~)",
			Sources: cli.EnvVars("POLLYTOOL_DENYPATHS"),
		},
		&cli.StringSliceFlag{
			Name:    "writepath",
			Usage:   "Additional path sandboxed tools may write to (repeatable, supports ~)",
			Sources: cli.EnvVars("POLLYTOOL_WRITEPATHS"),
		},
		&cli.BoolFlag{
			Name:    "allownet",
			Usage:   "Allow sandboxed tools outbound network access",
			Sources: cli.EnvVars("POLLYTOOL_ALLOWNET"),
		},
	}
}

// validateSandboxPresetSpec validates only the user-facing preset syntax.
// Building a workspace policy resolves and scans the filesystem, which belongs
// at sandbox startup rather than flag parsing: management commands, embed, and
// --nosandbox do not construct a sandbox at all.
func validateSandboxPresetSpec(spec string) error {
	if strings.TrimSpace(spec) == "" {
		return nil
	}
	var workspaceSelected, gitSelected bool
	for _, part := range strings.Split(spec, "+") {
		name := strings.TrimSpace(part)
		if !slices.Contains(sandbox.PresetNames, name) {
			return fmt.Errorf("unknown sandbox preset %q (valid: %s, joined with +)",
				name, strings.Join(sandbox.PresetNames, ", "))
		}
		workspaceSelected = workspaceSelected || name == "workspace"
		gitSelected = gitSelected || name == "git"
	}
	// Pure spec-level pairing check, mirrored from sandbox.ParsePreset so the
	// mistake surfaces at flag parsing instead of sandbox startup.
	if gitSelected && !workspaceSelected {
		return fmt.Errorf("sandbox preset %q requires %q (e.g. workspace+git): it selects how workspace Git metadata is protected", "git", "workspace")
	}
	return nil
}

func validateSandboxFlagCombination(cmd *cli.Command, config *Config) error {
	if config == nil || !config.NoSandbox {
		return nil
	}

	var conflicts []string
	for _, name := range []string{"sandbox", "denypath", "writepath", "allownet"} {
		if cmd.IsSet(name) {
			conflicts = append(conflicts, "--"+name)
		}
	}
	if len(conflicts) == 0 {
		return nil
	}
	return fmt.Errorf("--nosandbox cannot be enabled with %s; pass --nosandbox=false to re-enable sandboxing or remove the sandbox policy flags",
		strings.Join(conflicts, ", "))
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
		&cli.BoolFlag{
			Name:  "meta",
			Usage: "Emit a machine-readable run-outcome trailer (polly-meta key=value lines) to stderr",
		},
	}
}

func newThinkingFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:    "thinking",
		Usage:   "Reasoning effort: off, dynamic, a level (" + strings.Join(llm.ThinkingLevelNames(), ", ") + "), or a token budget (e.g. 12000)",
		Value:   "off",
		Sources: cli.EnvVars("POLLYTOOL_THINKING"),
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
		Usage: "Delete all sessions (requires confirmation)",
		Action: func(ctx context.Context, cmd *cli.Command, v bool) error {
			if !v {
				return nil
			}
			for _, name := range setFlagNames(cmd) {
				if !slices.Contains(purgeCompanionFlags, name) {
					return fmt.Errorf("--purge must be used alone (only --quiet or --debug allowed)")
				}
			}
			return nil
		},
	}
}

// setFlagNames returns the primary name of every flag set on cmd, by
// argument or environment, including the flags in its mutually exclusive
// groups.
func setFlagNames(cmd *cli.Command) []string {
	var names []string
	visit := func(f cli.Flag) {
		if f.IsSet() {
			names = append(names, f.Names()[0])
		}
	}
	for _, f := range cmd.Flags {
		visit(f)
	}
	for _, group := range cmd.MutuallyExclusiveFlags {
		for _, flags := range group.Flags {
			for _, f := range flags {
				visit(f)
			}
		}
	}
	return names
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

// validateMaxTokens, validateMaxContext, and validateToolTimeout are shared
// by the CLI flags and /set, so a value one path rejects the other cannot
// smuggle into metadata. Zero is a sentinel on every row: no max_tokens on
// the request, no clamp, no per-tool timeout.
func validateMaxTokens(n int) error {
	if n < 0 {
		return fmt.Errorf("maxtokens must be a non-negative integer (0 = provider default), got %d", n)
	}
	return nil
}

func validateMaxContext(n int) error {
	if n < 0 {
		return fmt.Errorf("maxcontext must be a non-negative integer (0 = unlimited), got %d", n)
	}
	return nil
}

func validateToolTimeout(d time.Duration) error {
	if d < 0 {
		return fmt.Errorf("tooltimeout must be a non-negative duration (e.g. 45s; 0 = no timeout), got %s", d)
	}
	return nil
}
