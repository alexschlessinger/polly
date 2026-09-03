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
	Temperature      float64
	MaxTokens        int
	MaxHistoryTokens int // provider-visible model projection budget
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
	Launch Settings

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
	NoSkills   bool
	ListSkills bool

	// Context operations
	ContextID      string
	ResetContext   string // Reset this context (clear history, keep settings)
	UseLastContext bool   // New field for --last flag
	ListContexts   bool
	DeleteContext  string
	AddToContext   bool
	PurgeAll       bool   // Delete all sessions
	CreateContext  string // Create a new context with this name
	ShowContext    string // Show configuration for this context

	// Input/Output configuration
	Prompt     string
	PromptSet  bool
	Files      []string // Files/images to include
	SchemaPath string   // Path to JSON schema file
	Meta       bool     // Emit a machine-readable run-outcome trailer (polly-meta lines) to stderr
	Quiet      bool
	Debug      bool

	// Temporary storage for command line tools (before conversion to ActiveTools)
	Tools []string

	// Skills to load directly (local paths or URLs, auto-activated)
	Skills []string
}
