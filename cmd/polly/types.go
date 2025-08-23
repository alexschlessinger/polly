package main

import (
	"time"
)

// Config holds all configuration from command-line flags
type Config struct {
	// Model configuration
	Model          string
	Temperature    float64
	MaxTokens      int
	Timeout        time.Duration
	ThinkingEffort string

	// API configuration
	BaseURL string

	// Context configuration
	ContextID      string
	ResetContext   string // Reset this context (clear history, keep settings)
	UseLastContext bool   // New field for --last flag
	ListContexts   bool
	DeleteContext  string
	AddToContext   bool
	PurgeAll       bool   // Delete all sessions and index
	CreateContext  string // Create a new context with this name
	ShowContext    string // Show configuration for this context

	// Tool configuration
	ToolPaths  []string
	MCPServers []string

	// Input/Output configuration
	Prompt       string
	SystemPrompt string
	Files        []string // Files/images to include
	SchemaPath   string   // Path to JSON schema file
	Quiet        bool
	Debug        bool
}

// ExecutionState holds runtime state during command execution
type ExecutionState struct {
	NeedFileStore bool
	ContextID     string
}
