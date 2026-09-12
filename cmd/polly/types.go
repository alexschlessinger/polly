package main

import (
	"time"
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
	AllowNet      bool

	// Skill configuration
	NoSkills bool

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

	// Temporary storage for command line tools (before conversion to ActiveTools)
	Tools []string

	// Skills to load directly (local paths or URLs, auto-activated)
	Skills []string
}
