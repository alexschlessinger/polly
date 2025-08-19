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
	ResetContext   bool // Reset context (clear history, keep settings)
	UseLastContext bool // New field for --last flag
	ListContexts   bool
	DeleteContext  string
	AddToContext   bool
	PurgeAll       bool // Delete all sessions and index

	// Tool configuration
	ToolPaths  []string
	MCPServers []string

	// Input/Output configuration
	Prompt             string
	SystemPrompt       string
	SystemPromptWasSet bool     // Track if system prompt was explicitly provided
	Files              []string // Files/images to include
	SchemaPath         string   // Path to JSON schema file
	Quiet              bool
	Debug              bool
}

// ExecutionState holds runtime state during command execution
type ExecutionState struct {
	NeedFileStore bool
	ContextID     string
}
