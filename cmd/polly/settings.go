package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/urfave/cli/v3"
)

// settingSpec declares one context setting. The table drives the REPL key
// lists, /get rendering, /set parsing, and the metadata copy gates.
//
// The gates are deliberately distinct and must stay distinct:
//   - flag != "" gates the IsSet twin walks in initializeConversation
//     (restore from metadata when NOT set) and applyFlagSettings (override
//     metadata when set);
//   - startupWriteBack marks updateContextInfo's unconditional startup
//     write-back set — maxcontext, system, and thinking are excluded on
//     purpose and reach metadata only through applyFlagSettings;
//   - parse != nil marks the /set-able rows, and exactly those are what
//     persistReplSettings writes after a /set: what /set can change, /set
//     must persist, or the change silently dies at relaunch.
type settingSpec struct {
	// key is the REPL name used by /get and /set. Table order is load-bearing:
	// it defines /get all output order and the settable-keys error text.
	key string

	// flag is the CLI flag name, or "" for derived flagless settings
	// (display, sandbox).
	flag string

	startupWriteBack bool

	// parse validates value and writes it onto cfg; nil marks the key
	// read-only for /set. Error texts appear in transcripts; keep stable.
	parse func(cfg *Config, value string) error

	// show renders the value for /get; nil hides the key from the REPL
	// entirely (maxiterations). Formats are test-pinned; keep stable.
	show func(ctx *replCommandContext, cfg *Config) string

	// setWords are value completions offered after "/set <key> "; nil for
	// free-form values.
	setWords []string

	// postReplSet runs after a successful /set of this key, for settings a
	// live component captures at construction.
	postReplSet func(ctx *replCommandContext)

	// fromCmd reads the flag's parsed value onto cfg in parseConfig; nil on
	// derived flagless rows.
	fromCmd func(cfg *Config, cmd *cli.Command)

	// fromMeta restores a persisted value onto cfg (flag not set at launch);
	// toMeta copies the resolved cfg value onto md and is shared by every
	// copy gate. Both nil only on derived flagless rows. Closures take
	// *Config, not *Settings, because MaxIterations lives on Config outside
	// the embedded Settings.
	fromMeta func(cfg *Config, md *sessions.Metadata)
	toMeta   func(cfg *Config, md *sessions.Metadata)
}

// settingSpecs is ordered: model, temp, maxtokens, maxcontext, thinking,
// system, display, tooltimeout, skilldir, sandbox, then the REPL-invisible
// maxiterations. The order reproduces the /get key list and the settable-keys
// error text byte-for-byte; insert new rows where they should appear there.
//
// system, skilldir, and sandbox stay launch-time only (parse == nil): the
// system prompt is embedded in session history at creation, and skill/sandbox
// wiring happens during tool loading. display is derived from the active
// frontend and never settable.
var settingSpecs = []settingSpec{
	{
		key:              "model",
		flag:             "model",
		startupWriteBack: true,
		parse: func(cfg *Config, value string) error {
			if value == "" {
				return fmt.Errorf("model requires a provider/model value")
			}
			if err := validateModel(value); err != nil {
				return err
			}
			cfg.Model = value
			return nil
		},
		show:     func(_ *replCommandContext, cfg *Config) string { return cfg.Model },
		fromCmd:  func(cfg *Config, cmd *cli.Command) { cfg.Model = cmd.String("model") },
		fromMeta: func(cfg *Config, md *sessions.Metadata) { cfg.Model = md.Model },
		toMeta:   func(cfg *Config, md *sessions.Metadata) { md.Model = cfg.Model },
	},
	{
		key:              "temp",
		flag:             "temp",
		startupWriteBack: true,
		parse: func(cfg *Config, value string) error {
			f, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return fmt.Errorf("temp must be a number, got %q", value)
			}
			if err := validateTemperature(f); err != nil {
				return err
			}
			cfg.Temperature = f
			return nil
		},
		show: func(_ *replCommandContext, cfg *Config) string {
			return fmt.Sprintf("%.2f", cfg.Temperature)
		},
		fromCmd:  func(cfg *Config, cmd *cli.Command) { cfg.Temperature = cmd.Float64("temp") },
		fromMeta: func(cfg *Config, md *sessions.Metadata) { cfg.Temperature = md.Temperature },
		toMeta:   func(cfg *Config, md *sessions.Metadata) { md.Temperature = cfg.Temperature },
	},
	{
		key:              "maxtokens",
		flag:             "maxtokens",
		startupWriteBack: true,
		parse: func(cfg *Config, value string) error {
			n, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("maxtokens must be a positive integer, got %q", value)
			}
			if err := validateMaxTokens(n); err != nil {
				return err
			}
			cfg.MaxTokens = n
			return nil
		},
		show: func(_ *replCommandContext, cfg *Config) string {
			return fmt.Sprintf("%d", cfg.MaxTokens)
		},
		fromCmd:  func(cfg *Config, cmd *cli.Command) { cfg.MaxTokens = cmd.Int("maxtokens") },
		fromMeta: func(cfg *Config, md *sessions.Metadata) { cfg.MaxTokens = md.MaxTokens },
		toMeta:   func(cfg *Config, md *sessions.Metadata) { md.MaxTokens = cfg.MaxTokens },
	},
	{
		key:  "maxcontext",
		flag: "maxcontext",
		parse: func(cfg *Config, value string) error {
			n, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("maxcontext must be a non-negative integer (0 = unlimited), got %q", value)
			}
			if err := validateMaxContext(n); err != nil {
				return err
			}
			cfg.MaxHistoryTokens = n
			return nil
		},
		show: func(_ *replCommandContext, cfg *Config) string {
			return fmt.Sprintf("%d", cfg.MaxHistoryTokens)
		},
		fromCmd:  func(cfg *Config, cmd *cli.Command) { cfg.MaxHistoryTokens = cmd.Int("maxcontext") },
		fromMeta: func(cfg *Config, md *sessions.Metadata) { cfg.MaxHistoryTokens = md.MaxHistoryTokens },
		toMeta:   func(cfg *Config, md *sessions.Metadata) { md.MaxHistoryTokens = cfg.MaxHistoryTokens },
	},
	{
		key:  "thinking",
		flag: "thinking",
		parse: func(cfg *Config, value string) error {
			if _, err := llm.ParseThinkingEffort(value); err != nil {
				return err
			}
			cfg.ThinkingEffort = value
			return nil
		},
		show: func(_ *replCommandContext, cfg *Config) string { return cfg.ThinkingEffort },
		// A raw token budget is also accepted; "auto" is deliberately not
		// offered.
		setWords: llm.ThinkingEffortWords(),
		fromCmd:  func(cfg *Config, cmd *cli.Command) { cfg.ThinkingEffort = cmd.String("thinking") },
		fromMeta: func(cfg *Config, md *sessions.Metadata) { cfg.ThinkingEffort = md.ThinkingEffort },
		toMeta:   func(cfg *Config, md *sessions.Metadata) { md.ThinkingEffort = cfg.ThinkingEffort },
	},
	{
		key:  "system",
		flag: "system",
		show: func(_ *replCommandContext, cfg *Config) string {
			if cfg.SystemPrompt == "" {
				return "(none)"
			}
			return cfg.SystemPrompt
		},
		fromCmd: func(cfg *Config, cmd *cli.Command) { cfg.SystemPrompt = cmd.String("system") },
		// A stored legacy default reads as the empty persona. The -s
		// conversation-reset detection is control flow, not a copy, and stays
		// in initializeConversation ahead of the table walk.
		fromMeta: func(cfg *Config, md *sessions.Metadata) {
			cfg.SystemPrompt = normalizeLegacySystemPrompt(md.SystemPrompt)
		},
		toMeta: func(cfg *Config, md *sessions.Metadata) { md.SystemPrompt = cfg.SystemPrompt },
	},
	{
		key: "display",
		show: func(ctx *replCommandContext, _ *Config) string {
			if ctx.state == nil {
				return "(none)"
			}
			return ctx.state.displayContract
		},
	},
	{
		key:              "tooltimeout",
		flag:             "tooltimeout",
		startupWriteBack: true,
		parse: func(cfg *Config, value string) error {
			d, err := time.ParseDuration(value)
			if err != nil {
				return fmt.Errorf("tooltimeout must be a positive duration (e.g. 45s), got %q", value)
			}
			if err := validateToolTimeout(d); err != nil {
				return err
			}
			cfg.ToolTimeout = d
			return nil
		},
		show: func(_ *replCommandContext, cfg *Config) string { return cfg.ToolTimeout.String() },
		// The agent captures the tool timeout at construction; push the change
		// through so it applies to the next turn, not the next launch.
		postReplSet: func(ctx *replCommandContext) {
			if ctx.state != nil && ctx.state.agent != nil {
				ctx.state.agent.SetToolTimeout(ctx.config.ToolTimeout)
			}
		},
		fromCmd:  func(cfg *Config, cmd *cli.Command) { cfg.ToolTimeout = cmd.Duration("tooltimeout") },
		fromMeta: func(cfg *Config, md *sessions.Metadata) { cfg.ToolTimeout = md.ToolTimeout },
		toMeta:   func(cfg *Config, md *sessions.Metadata) { md.ToolTimeout = cfg.ToolTimeout },
	},
	{
		key:              "skilldir",
		flag:             "skilldir",
		startupWriteBack: true,
		show: func(_ *replCommandContext, cfg *Config) string {
			if len(cfg.SkillDirs) == 0 {
				return "[]"
			}
			return strings.Join(cfg.SkillDirs, ", ")
		},
		fromCmd: func(cfg *Config, cmd *cli.Command) { cfg.SkillDirs = cmd.StringSlice("skilldir") },
		fromMeta: func(cfg *Config, md *sessions.Metadata) {
			cfg.SkillDirs = append([]string(nil), md.SkillDirs...)
		},
		toMeta: func(cfg *Config, md *sessions.Metadata) { md.SkillDirs = cfg.SkillDirs },
	},
	{
		key: "sandbox",
		show: func(ctx *replCommandContext, _ *Config) string {
			return sandboxPostureForContext(ctx).settingString()
		},
	},
	{
		// maxiterations rides the flag-persistence gates but stays invisible
		// to /get, /set, and completions.
		key:              "maxiterations",
		flag:             "maxiterations",
		startupWriteBack: true,
		fromCmd:          func(cfg *Config, cmd *cli.Command) { cfg.MaxIterations = cmd.Int("maxiterations") },
		fromMeta:         func(cfg *Config, md *sessions.Metadata) { cfg.MaxIterations = md.MaxIterations },
		toMeta:           func(cfg *Config, md *sessions.Metadata) { md.MaxIterations = cfg.MaxIterations },
	},
}

// The REPL key lists are derived from the table so contents and order can
// never drift from it.
var (
	replSettingKeys  = settingKeysWhere(func(s settingSpec) bool { return s.show != nil })
	replSettableKeys = settingKeysWhere(func(s settingSpec) bool { return s.parse != nil })
)

func settingKeysWhere(pred func(settingSpec) bool) []string {
	keys := make([]string, 0, len(settingSpecs))
	for _, s := range settingSpecs {
		if pred(s) {
			keys = append(keys, s.key)
		}
	}
	return keys
}

// flagSet reports whether the setting's flag was given, by argument or
// environment, so its value must reach metadata even outside the startup
// write-back set. An explicit zero (--maxcontext 0 = unlimited) counts.
func (s settingSpec) flagSet(cmd *cli.Command) bool {
	return s.flag != "" && s.toMeta != nil && cmd.IsSet(s.flag)
}

func settingSpecFor(key string) (settingSpec, bool) {
	for _, s := range settingSpecs {
		if s.key == key {
			return s, true
		}
	}
	return settingSpec{}, false
}
