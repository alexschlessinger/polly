package main

import (
	"os"
	"strconv"
	"time"
)

// Get default values from environment variables if set
func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvFloat(key string, defaultValue float64) float64 {
	if value := os.Getenv(key); value != "" {
		if f, err := strconv.ParseFloat(value, 64); err == nil {
			return f
		}
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if i, err := strconv.Atoi(value); err == nil {
			return i
		}
	}
	return defaultValue
}

func getEnvDuration(key string, defaultValue time.Duration) time.Duration {
	if value := os.Getenv(key); value != "" {
		if d, err := time.ParseDuration(value); err == nil {
			return d
		}
	}
	return defaultValue
}

var (
	defaultModel        = getEnvOrDefault("POLLYTOOL_MODEL", "anthropic/claude-sonnet-4-20250514")
	defaultSystemPrompt = getEnvOrDefault("POLLYTOOL_SYSTEM", "Your output will be displayed in a unix terminal. Be terse, 512 characters max. Do not use markdown.")
	defaultBaseURL      = getEnvOrDefault("POLLYTOOL_BASEURL", "")
	defaultTemperature  = getEnvFloat("POLLYTOOL_TEMP", 1.0)
	defaultMaxTokens    = getEnvInt("POLLYTOOL_MAXTOKENS", 4096)
	defaultTimeout      = getEnvDuration("POLLYTOOL_TIMEOUT", 2*time.Minute)
)

const (
	memoryStoreTTL = 30 * time.Minute
)

// Config holds all configuration from command-line flags
type Config struct {
	// Model configuration
	Model       string
	Temperature float64
	MaxTokens   int
	Timeout     time.Duration

	// API configuration
	BaseURL string

	// Context configuration
	ContextID      string
	ResetContext   string  // Reset context (clear history, keep settings)
	UseLastContext bool    // New field for --last flag
	ListContexts   bool
	DeleteContext  string
	AddToContext   bool

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
