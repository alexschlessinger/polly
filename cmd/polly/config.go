package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/tools/sandbox"
	"github.com/urfave/cli/v3"
)

// defaultSandboxPreset is the policy a sandbox gets when something asks for
// one without naming a preset: the working directory is writable, outbound
// network is allowed, and Git works — .git stays writable with only its
// dangerous leaves (config, hooks, routing pointers) pinned read-only.
// Tighten with e.g. --sandbox workspace+net (whole .git read-only) or
// --sandbox base. It is not what a launch gets untold: sandboxing is opt-in,
// and sandboxPolicyGiven says what asks for it.
const defaultSandboxPreset = sandbox.DefaultPresetSpec

var (
	validModelProviders = []string{"openai", "anthropic", "gemini", "ollama", "huggingface", "deepseek", "qwencloud", "openrouter", "codex"}
	validEmbedProviders = []string{"openai", "gemini"}
	// purgeCompanionFlags are the only flags --purge accepts alongside
	// itself, each under every name cmd.LocalFlagNames reports it by: the
	// primary name first, then aliases. The guard checks all of them and the
	// error text advertises the primary ones, so neither can drift.
	purgeCompanionFlags = [][]string{{"quiet"}, {"debug", "d"}}
)

func getCommand() *cli.Command {
	flags, mutuallyExclusiveGroups := defineFlagsWithGroups()
	return &cli.Command{
		Name:                   "polly",
		Usage:                  "Chat with LLMs using various providers",
		UsageText:              "polly [options]\n   polly ask <prompt> [options]\n   polly embed [options]",
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
}

// parseConfig extracts configuration from command-line flags. Settings flags
// are read through the spec table; only runtime configuration is hand-listed.
func parseConfig(cmd *cli.Command) *Config {
	config := &Config{
		Stream:          cmd.Bool("stream"),
		SwarmConcurrent: cmd.Int("swarm-concurrent"), SwarmExecutions: cmd.Int("swarm-executions"), SwarmDirectory: cmd.String("swarm-directory"), SwarmApplyTimeout: cmd.Duration("swarm-apply-timeout"),
		// Runtime configuration
		Timeout:       cmd.Duration("timeout"),
		Deadline:      cmd.Duration("deadline"),
		BaseURL:       cmd.String("baseurl"),
		Confirm:       cmd.Bool("confirm"),
		NoSandbox:     cmd.Bool("nosandbox"),
		SandboxPreset: cmd.String("sandbox"),
		DenyPaths:     cmd.StringSlice("denypath"),
		WritePaths:    cmd.StringSlice("writepath"),
		ReadPaths:     cmd.StringSlice("readpath"),
		AddDirs:       cmd.StringSlice("add-dir"),
		AllowNet:      cmd.Bool("allownet"),

		NoSandboxProfile: cmd.Bool("nosandboxprofile"),

		// Skill configuration
		NoSkills: cmd.Bool("noskills"),

		// Theme configuration
		Theme: cmd.String("theme"),

		Setup: cmd.Bool("setup"),

		// Context operations
		ContextID:      cmd.String("context"),
		UseLastContext: cmd.Bool("last"),

		ShotScript:  cmd.String("shot-script"),
		ShotSize:    cmd.String("shot-size"),
		ShotFixture: cmd.String("shot-fixture"),

		// Input/Output configuration
		Prompt:          cmd.String("prompt"),
		PromptSet:       cmd.IsSet("prompt"),
		Files:           cmd.StringSlice("file"),
		SchemaPath:      cmd.String("schema"),
		Meta:            cmd.Bool("meta"),
		ActivityDetails: cmd.Bool("activity-details"),
		Quiet:           cmd.Bool("quiet"),
		Debug:           cmd.Bool("debug"),
		Tools:           cmd.StringSlice("tool"),
		Skills:          cmd.StringSlice("skill"),
	}
	// Sandboxing is opt-in: a policy is what asks for one, and a launch
	// nothing asked runs unsandboxed. A policy that names no preset gets the
	// standard one, so a lone --writepath is a grant on top of a sandbox
	// rather than a setting with nothing to apply to.
	if !sandboxPolicyGiven(cmd, config) {
		config.NoSandbox = true
	} else {
		config.SandboxPreset = expandSandboxPreset(config.SandboxPreset)
	}
	config.Management, config.ManagementArg = parseManagementFlag(cmd)
	for _, spec := range settingSpecs {
		if spec.fromCmd != nil {
			spec.fromCmd(&config.Launch, cmd)
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
		"qwencloud":   os.Getenv("POLLYTOOL_QWENCLOUDKEY"),
		"deepseek":    os.Getenv("POLLYTOOL_DEEPSEEKKEY"),
		"openrouter":  os.Getenv("POLLYTOOL_OPENROUTERKEY"),
	}
}

func defineFlagsWithGroups() ([]cli.Flag, []cli.MutuallyExclusiveFlags) {
	resetDefaultSources()
	resetFlag := newPromptAndFileFreeStringFlag("reset", "Reset the specified context (clear conversation history, keep settings)")
	purgeFlag := newPurgeFlag()
	createFlag := newCreateFlag()
	showFlag := newPromptAndFileFreeStringFlag("show", "Show configuration for the specified context")
	listFlag := newPromptAndFileFreeBoolFlag("list", "List all available context IDs")
	listSkillsFlag := newPromptAndFileFreeBoolFlag("listskills", "List discovered Agent Skills")
	deleteFlag := newPromptAndFileFreeStringFlag("delete", "Delete the specified context")
	exportFlag := newPromptAndFileFreeStringFlag("export", "Print the specified context and the agents it spawned as a --shot-fixture file (JSON)")
	addFlag := &cli.BoolFlag{
		Name:  "add",
		Usage: "Add stdin content to context without making an API call",
	}
	loginFlag := newLoginFlag()
	logoutFlag := newLogoutFlag()

	flags := slices.Concat(
		modelConfigFlags(),
		apiConfigFlags(),
		skillConfigFlags(listSkillsFlag),
		toolConfigFlags(),
		inputConfigFlags(),
		contextManagementFlags(),
		historyConfigFlags(),
		approvalConfigFlags(),
		sandboxConfigFlags(),
		outputConfigFlags(),
		themeConfigFlags(),
		headlessConfigFlags(),
		loginConfigFlags(),
	)

	return flags, []cli.MutuallyExclusiveFlags{
		{
			Flags: [][]cli.Flag{
				{resetFlag},
				{purgeFlag},
				{createFlag},
				{showFlag},
				{listFlag},
				{deleteFlag},
				{exportFlag},
				{addFlag},
				{loginFlag},
				{logoutFlag},
			},
		},
	}
}

func modelConfigFlags() []cli.Flag {
	return []cli.Flag{
		&cli.BoolFlag{Name: "setup", Usage: "Choose and save the default provider, model, key, endpoint, reasoning effort, theme, and sandbox: a form in the TUI, questions in other terminals; with --model, --effort, --theme and --sandbox or --nosandbox all given, saves them and exits"},
		&cli.StringFlag{Name: "modelhost", Usage: "Pin an OpenRouter upstream host (automatic clears)", Sources: envDefault(envVarModelHost)},
		&cli.StringFlag{
			Name:      "model",
			Aliases:   []string{"m"},
			Usage:     "Model to use (provider/model format)",
			Value:     "anthropic/claude-opus-5",
			Sources:   envDefault(envVarModel),
			Validator: validateModel,
		},
		&cli.Float64Flag{
			Name:      "temp",
			Usage:     "Temperature for sampling",
			Value:     1.0,
			Sources:   envDefault("POLLYTOOL_TEMP"),
			Validator: validateTemperature,
		},
		&cli.IntFlag{
			Name:      "maxtokens",
			Usage:     "Maximum tokens to generate",
			Value:     64000,
			Sources:   envDefault("POLLYTOOL_MAXTOKENS"),
			Validator: validateMaxTokens,
		},
		&cli.IntFlag{
			Name:    "maxiterations",
			Usage:   "Maximum agent iterations (LLM calls) before stopping",
			Value:   1024,
			Sources: envDefault("POLLYTOOL_MAXITERATIONS"),
		},
		&cli.DurationFlag{
			Name:    "timeout",
			Usage:   "Stream stall timeout: cancel a request after this long with no provider data (0 disables)",
			Value:   30 * time.Minute,
			Sources: envDefault("POLLYTOOL_TIMEOUT"),
		},
		&cli.DurationFlag{
			Name:    "deadline",
			Usage:   "Hard per-request ceiling: cancel a request after this total time even if data is still arriving (0 = no ceiling)",
			Value:   2 * time.Hour,
			Sources: envDefault("POLLYTOOL_DEADLINE"),
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
			Sources: envDefault(envVarBaseURL),
		},
	}
}

func skillConfigFlags(listSkillsFlag *cli.BoolFlag) []cli.Flag {
	return []cli.Flag{
		&cli.StringSliceFlag{
			Name:    "skilldir",
			Usage:   "Skill directory or directory containing skill folders (can be specified multiple times)",
			Sources: envDefault("POLLYTOOL_SKILLDIR"),
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
			Sources:   envDefault("POLLYTOOL_TOOLTIMEOUT"),
			Validator: validateToolTimeout,
		},
	}
}

func inputConfigFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:    "prompt",
			Aliases: []string{"p", "ask"},
			Usage:   "Initial prompt (piped stdin is attached to it, or is the prompt if none is given; starts REPL when neither is provided)",
		},
		&cli.StringFlag{
			Name:    "system",
			Aliases: []string{"s"},
			Usage:   "Custom persona (replaces coding defaults and AGENTS.md loading; display and recall guidance is added automatically)",
			Sources: envDefault("POLLYTOOL_SYSTEM"),
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
			Sources: envDefault("POLLYTOOL_CONTEXT"),
		},
		&cli.BoolFlag{
			Name:    "last",
			Aliases: []string{"L"},
			Usage:   "Use the last active context",
		},
		&cli.BoolFlag{
			Name:  "flat",
			Usage: "With --list, print one line per context instead of nesting agents under the context that spawned them",
		},
		&cli.BoolFlag{
			Name:  "artifacts",
			Usage: "With --export, embed the artifacts (attached images, stored tool output) the transcripts reference",
		},
	}
}

func historyConfigFlags() []cli.Flag {
	return []cli.Flag{
		&cli.IntFlag{
			Name:        "maxcontext",
			Usage:       "Maximum estimated tokens sent to the model (default: detected model context, with output reserve; 256000 fallback); explicit limits override detection, 0 = unlimited",
			Value:       defaultContextBudget,
			DefaultText: "detected model context",
			Validator:   validateMaxContext,
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
			Name:      "sandbox",
			Usage:     "Sandbox tool commands under this preset: base, readonly, workspace, git, net, ssh, sshkeys, private-home — join with + (e.g. " + defaultSandboxPreset + "); git requires workspace. Unset runs them unsandboxed",
			Sources:   envDefault(envVarSandbox),
			Validator: validateSandboxPresetSpec,
		},
		&cli.BoolFlag{
			Name:    "nosandbox",
			Usage:   "Run tool commands unsandboxed, as they are without a sandbox policy; refuses to coexist with one",
			Sources: envDefault(envVarNoSandbox),
		},
		&cli.StringSliceFlag{
			Name:    "denypath",
			Usage:   "Additional path blocked from sandboxed reads (repeatable, supports ~)",
			Sources: envDefault(envVarDenyPaths),
		},
		&cli.StringSliceFlag{
			Name:    "writepath",
			Usage:   "Additional path sandboxed tools may write to (repeatable, supports ~)",
			Sources: envDefault(envVarWritePaths),
		},
		&cli.StringSliceFlag{
			Name:    "readpath",
			Usage:   "Additional path sandboxed tools may read inside the private home directory (repeatable, supports ~)",
			Sources: envDefault(envVarReadPaths),
		},
		&cli.StringSliceFlag{
			// No Sources on purpose: extra read dirs are a per-session
			// surface, never an ambient grant that widens every session.
			Name:  "add-dir",
			Usage: "Extra read-only directory sandboxed tools may read (repeatable; validated; persisted per session and merged on resume)",
		},
		&cli.BoolFlag{
			Name:    "allownet",
			Usage:   "Allow sandboxed tools outbound network access",
			Sources: envDefault(envVarAllowNet),
		},
		&cli.BoolFlag{
			Name:    "nosandboxprofile",
			Usage:   "Leave this workspace's sandbox profile (see /sandbox) out of this launch",
			Sources: envDefault("POLLYTOOL_NOSANDBOXPROFILE"),
		},
	}
}

// validateSandboxPresetSpec validates only the user-facing preset syntax.
// Building a workspace policy resolves and scans the filesystem, which belongs
// at sandbox startup rather than flag parsing: management commands, embed, and
// --nosandbox do not construct a sandbox at all.
// expandSandboxPreset spells a spec out into the presets it names, so one
// canonical form reaches the policy, the posture and the saved configuration.
// "default" and a spec that names nothing at all are input spellings of the
// standard preset, never what a launch reports it runs under.
func expandSandboxPreset(spec string) string {
	if strings.TrimSpace(spec) == "" {
		return defaultSandboxPreset
	}
	parts := strings.Split(spec, "+")
	for i, part := range parts {
		if strings.TrimSpace(part) == "default" {
			parts[i] = defaultSandboxPreset
		}
	}
	return strings.Join(parts, "+")
}

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
		// "default" selects both, so it pairs git with a workspace the way
		// its expansion does.
		workspaceSelected = workspaceSelected || name == "workspace" || name == "default"
		gitSelected = gitSelected || name == "git"
	}
	// Pure spec-level pairing check, mirrored from sandbox.ParsePreset so the
	// mistake surfaces at flag parsing instead of sandbox startup.
	if gitSelected && !workspaceSelected {
		return fmt.Errorf("sandbox preset %q requires %q (e.g. workspace+git): it selects how workspace Git metadata is protected", "git", "workspace")
	}
	return nil
}

// sandboxPolicyFlags are the flags that carry a sandbox policy: what asks a
// launch for a sandbox, and what an explicit --nosandbox refuses to coexist
// with. --add-dir is not one: it widens a sandbox rather than asking for one.
var sandboxPolicyFlags = []string{"sandbox", "denypath", "writepath", "readpath", "allownet"}

// sandboxPolicyGiven reports whether anything asked this launch for a
// sandbox: a preset, a path grant or a network grant, from a flag, the
// environment or the configuration file. Sandboxing is opt-in, so a launch
// nothing asked runs its tool commands unsandboxed. --sandbox counts even
// when it names no preset (--sandbox=, POLLYTOOL_SANDBOX=): an empty value
// asks for a sandbox the way a lone grant does, and gets the standard preset
// rather than silently running without one.
func sandboxPolicyGiven(cmd *cli.Command, config *Config) bool {
	return cmd.IsSet("sandbox") || config.SandboxPreset != "" || config.AllowNet ||
		len(config.DenyPaths) > 0 || len(config.WritePaths) > 0 || len(config.ReadPaths) > 0
}

// noSandboxExported reports whether the environment turns the sandbox off.
// Only the environment counts: a configuration file's line is one the setup
// form writes over, while an exported variable outlives the save and would
// meet a saved policy at the next launch, which refuses the pair.
func noSandboxExported() bool {
	value, ok := os.LookupEnv(envVarNoSandbox)
	if !ok {
		return false
	}
	off, err := strconv.ParseBool(strings.TrimSpace(value))
	return err == nil && off
}

func validateSandboxFlagCombination(cmd *cli.Command, config *Config) error {
	if config == nil || !config.NoSandbox {
		return nil
	}

	// A policy from the environment conflicts too: sandboxing fails closed,
	// so an ambient policy and an explicit --nosandbox must not coexist.
	var conflicts []string
	for _, flag := range sandboxPolicyFlags {
		if cmd.IsSet(flag) {
			conflicts = append(conflicts, "--"+flag)
		}
	}
	if len(conflicts) == 0 {
		return nil
	}
	return fmt.Errorf("--nosandbox cannot be enabled with %s; pass --nosandbox=false to re-enable sandboxing or remove the sandbox policy flags",
		strings.Join(conflicts, ", "))
}

// headlessConfigFlags are the off-screen shot run's own flags: they describe
// how a frame is captured, never what goes into a session, so they have no
// settingSpecs row and no session record.
func headlessConfigFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:    "shot-script",
			Usage:   "Run the TUI off-screen, playing this script of typed input, keys and captures (\"-\" reads stdin)",
			Sources: envDefault("POLLYTOOL_SHOT_SCRIPT"),
		},
		&cli.StringFlag{
			Name:    "shot-size",
			Value:   "120x40",
			Usage:   "Virtual terminal size a --shot-script run paints at, as WxH",
			Sources: envDefault("POLLYTOOL_SHOT_SIZE"),
		},
		&cli.StringFlag{
			Name:    "shot-fixture",
			Usage:   "Seed sessions and play scripted model turns from this fixture file during a --shot-script run",
			Sources: envDefault("POLLYTOOL_SHOT_FIXTURE"),
		},
	}
}

func outputConfigFlags() []cli.Flag {
	return []cli.Flag{
		&cli.BoolFlag{Name: "stream", Usage: "Stream one-shot assistant text immediately (default: emit the settled answer)", Sources: envDefault("POLLYTOOL_STREAM")},
		&cli.IntFlag{Name: "swarm-concurrent", Value: 32, Usage: "Maximum concurrent child executions", Sources: envDefault("POLLYTOOL_SWARM_CONCURRENT")},
		&cli.IntFlag{Name: "swarm-executions", Value: 256, Usage: "Total logical child executions per swarm run", Sources: envDefault("POLLYTOOL_SWARM_EXECUTIONS")},
		&cli.DurationFlag{Name: "swarm-apply-timeout", Value: 2 * time.Minute, Usage: "Timeout for a started integration write; outcome recording is separately bounded", Sources: envDefault("POLLYTOOL_SWARM_APPLY_TIMEOUT")},
		&cli.StringFlag{Name: "swarm-directory", Usage: "Runtime-owned worktree directory outside the source checkout", Sources: envDefault("POLLYTOOL_SWARM_DIRECTORY")},
		&cli.BoolFlag{
			Name:    "activity-details",
			Usage:   "Print bounded thought, tool, agent, and image details at turn end (one-shot only; ignored by the REPL)",
			Sources: envDefault("POLLYTOOL_ACTIVITY_DETAILS"),
		},
		&cli.BoolFlag{
			Name:  "quiet",
			Usage: "Suppress status, tool display, and sandbox notice output",
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
		Name: "effort",
		// --thinking was this flag's name; it still parses, and the help
		// text lists it alongside --effort.
		Aliases: []string{effortFormerKey},
		Usage:   "Reasoning effort: " + llm.ThinkingEffortForms(),
		Value:   defaultThinkingEffort,
		Sources: envDefault(envVarEffort, envVarThinking),
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
			// LocalFlagNames lists every name of every flag set by argument
			// or default source, mutually exclusive groups included; only
			// the ones given on the command line count as companions.
			for _, name := range cmd.LocalFlagNames() {
				if name != "purge" && !purgeCompanionFlag(name) && flagGiven(cmd, name) {
					return fmt.Errorf("--purge must be used alone (only %s allowed)", purgeCompanionUsage())
				}
			}
			return nil
		},
	}
}

func purgeCompanionFlag(name string) bool {
	for _, names := range purgeCompanionFlags {
		if slices.Contains(names, name) {
			return true
		}
	}
	return false
}

// purgeCompanionUsage renders the companion flags by primary name for the
// --purge error text.
func purgeCompanionUsage() string {
	names := make([]string, len(purgeCompanionFlags))
	for i, flag := range purgeCompanionFlags {
		names[i] = "--" + flag[0]
	}
	if len(names) == 1 {
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
}

func validateNoPromptOrFiles(cmd *cli.Command, flagName string) error {
	if cmd.String("prompt") != "" || len(cmd.StringSlice("file")) > 0 {
		return fmt.Errorf("--%s does not take prompts or files", flagName)
	}
	return nil
}

func validateModel(model string) error {
	return validateModelWithProviders(model, validModelProviders, "anthropic/claude-opus-5")
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
