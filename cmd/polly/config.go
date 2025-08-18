package main

import (
	"os"
	"strconv"
	"time"

	"github.com/urfave/cli/v3"
)

// Constants
const (
	memoryStoreTTL = 0 // No expiration for polly CLI sessions (runs for minutes at most)
)

// Default values from environment variables
var (
	defaultModel        = getEnvOrDefault("POLLYTOOL_MODEL", "anthropic/claude-sonnet-4-20250514")
	defaultSystemPrompt = getEnvOrDefault("POLLYTOOL_SYSTEM", "Your output will be displayed in a unix terminal. Be terse, 512 characters max. Do not use markdown.")
	defaultBaseURL      = getEnvOrDefault("POLLYTOOL_BASEURL", "")
	defaultTemperature  = getEnvFloat("POLLYTOOL_TEMP", 1.0)
	defaultMaxTokens    = getEnvInt("POLLYTOOL_MAXTOKENS", 4096)
	defaultTimeout      = getEnvDuration("POLLYTOOL_TIMEOUT", 2*time.Minute)
)

// Environment variable parsing functions

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

// parseConfig extracts configuration from command-line flags
func parseConfig(cmd *cli.Command) *Config {
	return &Config{
		// Model configuration
		Model:       cmd.String("model"),
		Temperature: cmd.Float64("temp"),
		MaxTokens:   cmd.Int("maxtokens"),
		Timeout:     cmd.Duration("timeout"),

		// API configuration
		BaseURL: cmd.String("baseurl"),

		// Context configuration
		ContextID:      cmd.String("context"),
		ResetContext:   cmd.Bool("reset"),
		UseLastContext: cmd.Bool("last"),
		ListContexts:   cmd.Bool("list"),
		DeleteContext:  cmd.String("delete"),
		AddToContext:   cmd.Bool("add"),

		// Tool configuration
		ToolPaths:  cmd.StringSlice("tool"),
		MCPServers: cmd.StringSlice("mcp"),

		// Input/Output configuration
		Prompt:             cmd.String("prompt"),
		SystemPrompt:       cmd.String("system"),
		SystemPromptWasSet: cmd.IsSet("system"),
		Files:              cmd.StringSlice("file"),
		SchemaPath:         cmd.String("schema"),
		Quiet:              cmd.Bool("quiet"),
		Debug:              cmd.Bool("debug"),
	}
}
