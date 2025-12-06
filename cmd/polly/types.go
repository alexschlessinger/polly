package main

import (
	"time"

	"github.com/alexschlessinger/pollytool/sessions"
)

// Settings contains configuration that can be persisted with a context
type Settings struct {
	// Model configuration
	Model            string  `json:"model,omitempty"`
	Temperature      float64 `json:"temperature,omitempty"`
	MaxTokens        int     `json:"maxTokens,omitempty"`
	MaxHistoryTokens int     `json:"maxHistoryTokens,omitempty"` // max tokens for context history
	ThinkingEffort   string  `json:"thinkingEffort,omitempty"`
	SystemPrompt     string  `json:"systemPrompt,omitempty"`

	// Tool configuration
	ToolTimeout time.Duration `json:"toolTimeout,omitempty"`
}

// Config holds all configuration from command-line flags
type Config struct {
	Settings // Embed the shared settings

	// Runtime configuration
	Timeout time.Duration
	BaseURL string

	// Context operations
	ContextID      string
	ResetContext   string // Reset this context (clear history, keep settings)
	UseLastContext bool   // New field for --last flag
	ListContexts   bool
	DeleteContext  string
	AddToContext   bool
	PurgeAll       bool   // Delete all sessions and index
	CreateContext  string // Create a new context with this name
	ShowContext    string // Show configuration for this context

	// Input/Output configuration
	Prompt     string
	Files      []string // Files/images to include
	SchemaPath string   // Path to JSON schema file
	Quiet      bool
	Debug      bool

	// Temporary storage for command line tools (before conversion to ActiveTools)
	Tools []string
}

// ToMetadataSettings copies Settings fields to Metadata
func (s Settings) ToMetadataSettings(m *sessions.Metadata) {
	m.Model = s.Model
	m.Temperature = s.Temperature
	m.MaxTokens = s.MaxTokens
	m.MaxHistoryTokens = s.MaxHistoryTokens
	m.ThinkingEffort = s.ThinkingEffort
	m.SystemPrompt = s.SystemPrompt
	m.ToolTimeout = s.ToolTimeout
}
