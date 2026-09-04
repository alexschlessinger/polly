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

// settingSpec declares one session setting. The table drives the REPL key
// lists, /get rendering, /set parsing, and the metadata copy gates.
//
// The gates are deliberately distinct and must stay distinct:
//   - flagged rows (fromCmd != nil; the flag shares the row's key) gate the
//     IsSet twin walks in prepareConversation (restore from metadata when
//     NOT set) and applyFlagSettings (override metadata when set), and are
//     updateContextInfo's startup write-back set: the resolved settings hold
//     the stored value unless a flag overrode it, so the copy is a no-op for
//     an untouched row;
//   - parse != nil marks the /set-able rows, and exactly those are what
//     persistReplSettings writes after a /set: what /set can change, /set
//     must persist, or the change silently dies at relaunch.
type settingSpec struct {
	// key is the REPL name used by /get and /set, and the CLI flag name on
	// flagged rows. Table order is load-bearing: it defines /get all output
	// order and the settable-keys error text.
	key string

	// parse validates value and writes it onto s; nil marks the key
	// read-only for /set. Error texts appear in transcripts; keep stable.
	parse func(s *Settings, value string) error

	// show renders the value for /get; nil hides the key from the REPL
	// entirely (maxiterations). Formats are test-pinned; keep stable.
	show func(ctx *replCommandContext, s *Settings) string

	// setWords are value completions offered after "/set <key> "; nil for
	// free-form values.
	setWords []string

	// postReplSet runs after a successful /set of this key, for settings a
	// live component captures at construction.
	postReplSet func(ctx *replCommandContext)

	// fromCmd reads the flag's parsed value onto s in parseConfig; nil on
	// derived flagless rows.
	fromCmd func(s *Settings, cmd *cli.Command)

	// fromMeta restores a persisted value onto s (flag not set at launch);
	// toMeta copies the resolved value onto md and is shared by every copy
	// gate. Both nil only on derived flagless rows.
	fromMeta func(s *Settings, md *sessions.Metadata)
	toMeta   func(s *Settings, md *sessions.Metadata)
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
		key: "model",
		parse: func(s *Settings, value string) error {
			if value == "" {
				return fmt.Errorf("model requires a provider/model value")
			}
			if err := validateModel(value); err != nil {
				return err
			}
			s.Model = value
			return nil
		},
		show:     func(_ *replCommandContext, s *Settings) string { return s.Model },
		fromCmd:  func(s *Settings, cmd *cli.Command) { s.Model = cmd.String("model") },
		fromMeta: func(s *Settings, md *sessions.Metadata) { s.Model = md.Model },
		toMeta:   func(s *Settings, md *sessions.Metadata) { md.Model = s.Model },
	},
	{
		key: "temp",
		parse: func(s *Settings, value string) error {
			f, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return fmt.Errorf("temp must be a number, got %q", value)
			}
			if err := validateTemperature(f); err != nil {
				return err
			}
			s.Temperature = f
			return nil
		},
		show: func(_ *replCommandContext, s *Settings) string {
			return fmt.Sprintf("%.2f", s.Temperature)
		},
		fromCmd:  func(s *Settings, cmd *cli.Command) { s.Temperature = cmd.Float64("temp") },
		fromMeta: func(s *Settings, md *sessions.Metadata) { s.Temperature = md.Temperature },
		toMeta:   func(s *Settings, md *sessions.Metadata) { md.Temperature = s.Temperature },
	},
	{
		key: "maxtokens",
		parse: func(s *Settings, value string) error {
			n, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("maxtokens must be a non-negative integer (0 = provider default), got %q", value)
			}
			if err := validateMaxTokens(n); err != nil {
				return err
			}
			s.MaxTokens = n
			return nil
		},
		show: func(_ *replCommandContext, s *Settings) string {
			return fmt.Sprintf("%d", s.MaxTokens)
		},
		fromCmd:  func(s *Settings, cmd *cli.Command) { s.MaxTokens = cmd.Int("maxtokens") },
		fromMeta: func(s *Settings, md *sessions.Metadata) { s.MaxTokens = md.MaxTokens },
		toMeta:   func(s *Settings, md *sessions.Metadata) { md.MaxTokens = s.MaxTokens },
	},
	{
		key: "maxcontext",
		parse: func(s *Settings, value string) error {
			n, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("maxcontext must be a non-negative integer (0 = unlimited), got %q", value)
			}
			if err := validateMaxContext(n); err != nil {
				return err
			}
			s.MaxHistoryTokens = n
			return nil
		},
		show: func(_ *replCommandContext, s *Settings) string {
			return fmt.Sprintf("%d", s.MaxHistoryTokens)
		},
		fromCmd:  func(s *Settings, cmd *cli.Command) { s.MaxHistoryTokens = cmd.Int("maxcontext") },
		fromMeta: func(s *Settings, md *sessions.Metadata) { s.MaxHistoryTokens = md.MaxHistoryTokens },
		toMeta:   func(s *Settings, md *sessions.Metadata) { md.MaxHistoryTokens = s.MaxHistoryTokens },
	},
	{
		key: "thinking",
		parse: func(s *Settings, value string) error {
			if _, err := llm.ParseThinkingEffort(value); err != nil {
				return err
			}
			s.ThinkingEffort = value
			return nil
		},
		show: func(_ *replCommandContext, s *Settings) string { return s.ThinkingEffort },
		// A raw token budget is also accepted; "auto" is deliberately not
		// offered.
		setWords: llm.ThinkingEffortWords(),
		fromCmd:  func(s *Settings, cmd *cli.Command) { s.ThinkingEffort = cmd.String("thinking") },
		fromMeta: func(s *Settings, md *sessions.Metadata) { s.ThinkingEffort = md.ThinkingEffort },
		toMeta:   func(s *Settings, md *sessions.Metadata) { md.ThinkingEffort = s.ThinkingEffort },
	},
	{
		key: "system",
		show: func(_ *replCommandContext, s *Settings) string {
			if s.SystemPrompt == "" {
				return "(none)"
			}
			return s.SystemPrompt
		},
		fromCmd: func(s *Settings, cmd *cli.Command) { s.SystemPrompt = cmd.String("system") },
		// The -s conversation-reset detection is control flow, not a copy, and
		// stays in prepareConversation ahead of the table walk.
		fromMeta: func(s *Settings, md *sessions.Metadata) { s.SystemPrompt = md.SystemPrompt },
		toMeta:   func(s *Settings, md *sessions.Metadata) { md.SystemPrompt = s.SystemPrompt },
	},
	{
		key: "display",
		show: func(ctx *replCommandContext, _ *Settings) string {
			if ctx.state == nil {
				return "(none)"
			}
			return ctx.state.displayContract
		},
	},
	{
		key: "tooltimeout",
		parse: func(s *Settings, value string) error {
			d, err := time.ParseDuration(value)
			if err != nil {
				return fmt.Errorf("tooltimeout must be a non-negative duration (e.g. 45s; 0 = no timeout), got %q", value)
			}
			if err := validateToolTimeout(d); err != nil {
				return err
			}
			s.ToolTimeout = d
			return nil
		},
		show: func(_ *replCommandContext, s *Settings) string { return s.ToolTimeout.String() },
		// The agent captures the tool timeout at construction; push the change
		// through so it applies to the next turn, not the next launch.
		postReplSet: func(ctx *replCommandContext) {
			if ctx.state != nil && ctx.state.agent != nil && ctx.settings != nil {
				ctx.state.agent.SetToolTimeout(ctx.settings.ToolTimeout)
			}
		},
		fromCmd:  func(s *Settings, cmd *cli.Command) { s.ToolTimeout = cmd.Duration("tooltimeout") },
		fromMeta: func(s *Settings, md *sessions.Metadata) { s.ToolTimeout = md.ToolTimeout },
		toMeta:   func(s *Settings, md *sessions.Metadata) { md.ToolTimeout = s.ToolTimeout },
	},
	{
		key: "skilldir",
		show: func(_ *replCommandContext, s *Settings) string {
			if len(s.SkillDirs) == 0 {
				return "[]"
			}
			return strings.Join(s.SkillDirs, ", ")
		},
		fromCmd: func(s *Settings, cmd *cli.Command) { s.SkillDirs = cmd.StringSlice("skilldir") },
		fromMeta: func(s *Settings, md *sessions.Metadata) {
			s.SkillDirs = append([]string(nil), md.SkillDirs...)
		},
		toMeta: func(s *Settings, md *sessions.Metadata) { md.SkillDirs = s.SkillDirs },
	},
	{
		key: "sandbox",
		show: func(ctx *replCommandContext, _ *Settings) string {
			return sandboxPostureForContext(ctx).settingString()
		},
	},
	{
		// maxiterations rides the flag-persistence gates but stays invisible
		// to /get, /set, and completions.
		key:      "maxiterations",
		fromCmd:  func(s *Settings, cmd *cli.Command) { s.MaxIterations = cmd.Int("maxiterations") },
		fromMeta: func(s *Settings, md *sessions.Metadata) { s.MaxIterations = md.MaxIterations },
		toMeta:   func(s *Settings, md *sessions.Metadata) { md.MaxIterations = s.MaxIterations },
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

// flagged reports whether a CLI flag named key backs the row; derived rows
// (display, sandbox) have none.
func (s settingSpec) flagged() bool {
	return s.fromCmd != nil
}

// flagSet reports whether the setting's flag was given, by argument or
// environment, so its value must reach metadata even outside the startup
// write-back set. An explicit zero (--maxcontext 0 = unlimited) counts.
func (s settingSpec) flagSet(cmd *cli.Command) bool {
	return s.flagged() && cmd.IsSet(s.key)
}

func settingSpecFor(key string) (settingSpec, bool) {
	for _, s := range settingSpecs {
		if s.key == key {
			return s, true
		}
	}
	return settingSpec{}, false
}
