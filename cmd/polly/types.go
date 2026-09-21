package main

import (
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/llm/replay"
)

// Settings are the per-session settings: the values a session stores in its
// metadata, restores when it is opened, and changes through /set. Every open
// session carries its own copy, so a change made in one tab never reaches
// another, and every turn reads the settings of the session it runs on.
type Settings struct {
	// Model configuration
	Model            string
	ModelHost        string
	Temperature      float64
	MaxTokens        int
	MaxHistoryTokens int  // provider-visible model projection budget
	AutoMaxContext   bool // follow detected model capacity; MaxHistoryTokens is a display snapshot
	ThinkingEffort   string
	SystemPrompt     string

	// Agent configuration
	ToolTimeout   time.Duration
	MaxIterations int

	// Skill configuration
	SkillDirs []string
}

// clone returns a copy that shares no slices with s.
func (s Settings) clone() Settings {
	s.SkillDirs = append([]string(nil), s.SkillDirs...)
	return s
}

// agentConfig is the agent run configuration the settings decide: the
// iteration cap and per-tool timeout. Callers add the run-specific fields.
func (s Settings) agentConfig() llm.AgentConfig {
	return llm.AgentConfig{MaxIterations: s.MaxIterations, ToolTimeout: s.ToolTimeout}
}

// Config is the process configuration: everything that holds for the whole
// run whichever session is visible. Launch carries the settings resolved from
// flags, environment, and defaults at startup: a new session starts from
// them, and a flag given explicitly overrides the stored value of every
// session this process opens.
type Config struct {
	Stream                           bool
	SwarmConcurrent, SwarmExecutions int
	SwarmDirectory                   string
	SwarmApplyTimeout                time.Duration
	Launch                           Settings

	// Runtime configuration
	Timeout       time.Duration
	Deadline      time.Duration
	BaseURL       string
	Confirm       bool
	NoSandbox     bool
	SandboxPreset string
	DenyPaths     []string
	WritePaths    []string
	ReadPaths     []string
	// AddDirs are the extra read-only directories from --add-dir: each is
	// validated at open, merged into the session's persisted
	// ExtraReadDirs (a resume adds to the stored list instead of replacing
	// it), and granted to the sandbox as read-only paths.
	AddDirs  []string
	AllowNet bool
	// NoSandboxProfile leaves the workspace's sandbox profile out of this
	// launch; /sandbox still shows and edits the file.
	NoSandboxProfile bool

	// Skill configuration
	NoSkills bool

	// Theme configuration: the theme this process starts with, naming a
	// builtin preset, a file under ~/.pollytool/themes, or a path. It is
	// process-wide: applying a theme rewrites the global color table every
	// surface resolves through, so it is not a per-session setting and has no
	// settingSpecs row.
	Theme string
	// activeTheme is the selection the startup apply resolved (see theme.go).
	// The managed REPL only receives *Config, so this is how the reload
	// watcher finds the same file to stat.
	activeTheme themeSelection

	// Setup opens the setup form at TUI start: --setup, or a first run with
	// nothing configured (decided in runConversation).
	Setup bool

	// Context operations
	ContextID      string
	UseLastContext bool // --last
	// Management is the context-management flag given instead of a prompt
	// (nil for a conversation); ManagementArg names its context when the
	// flag takes one.
	Management    *managementFlag
	ManagementArg string

	// Input/Output configuration
	Prompt          string
	PromptSet       bool
	Files           []string // Files/images to include
	SchemaPath      string   // Path to JSON schema file
	Meta            bool     // Emit a machine-readable run-outcome trailer (polly-meta lines) to stderr
	ActivityDetails bool     // Print bounded turn details to stderr in one-shot mode
	Quiet           bool
	Debug           bool
	// ShotScript is a headless shot script to play instead of reading a
	// keyboard ("-" is stdin), and ShotSize is the virtual terminal it paints
	// at, as WxH. Both describe the run only, so they stay out of sessions.
	ShotScript string
	ShotSize   string
	// ShotFixture is the fixture a shot run seeds sessions from and plays
	// model turns from (see shot_fixture.go); shotFixture and shotBus are
	// its loaded form and the signal bus its turns share with the script.
	ShotFixture string
	shotFixture *shotFixture
	shotBus     *replay.Bus

	// Temporary storage for command line tools (before conversion to ActiveTools)
	Tools []string

	// Skills to load directly (local paths or URLs, auto-activated)
	Skills []string
}
