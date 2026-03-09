package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
)

// toolDisplayEnabled returns true when tool display should be shown.
func toolDisplayEnabled(config *Config) bool {
	return !config.Quiet && isTerminal()
}

// printToolStart prints a tool call header with summarized args to stderr.
func printToolStart(tc messages.ChatMessageToolCall) {
	name := tc.Name
	summary := summarizeToolArgs(name, tc.Arguments)

	header := fmt.Sprintf("── tool: %s ", name)
	pad := 40 - len(header)
	if pad < 3 {
		pad = 3
	}
	header += strings.Repeat("─", pad)

	fmt.Fprintf(os.Stderr, "%s%s%s\n",
		dimStyle.Styled(""),
		dimStyle.Styled(header),
		dimStyle.Styled(""),
	)
	if summary != "" {
		fmt.Fprintf(os.Stderr, "  %s\n", dimStyle.Styled(summary))
	}
}

// printToolEnd prints a tool completion line with duration to stderr.
func printToolEnd(tc messages.ChatMessageToolCall, duration time.Duration, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "  %s\n", errorStyle.Styled(fmt.Sprintf("✗ %s (%.1fs)", err, duration.Seconds())))
	} else {
		fmt.Fprintf(os.Stderr, "  %s\n", dimStyle.Styled(fmt.Sprintf("✓ %.1fs", duration.Seconds())))
	}
}

// summarizeToolArgs returns a one-line summary of tool arguments.
func summarizeToolArgs(toolName, argsJSON string) string {
	if argsJSON == "" {
		return ""
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return ""
	}

	switch toolName {
	case "bash":
		return strVal(args, "command")
	case "read":
		s := strVal(args, "file_path")
		if offset := intVal(args, "offset"); offset > 0 {
			limit := intVal(args, "limit")
			if limit > 0 {
				s += fmt.Sprintf(" (lines %d-%d)", offset, offset+limit)
			} else {
				s += fmt.Sprintf(" (from line %d)", offset)
			}
		}
		return s
	case "write":
		return strVal(args, "file_path")
	case "edit":
		return strVal(args, "file_path")
	case "glob":
		return strVal(args, "pattern")
	case "grep":
		return strVal(args, "pattern")
	default:
		return ""
	}
}

func strVal(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return truncate(s, 120)
		}
	}
	return ""
}

func intVal(m map[string]any, key string) int {
	if v, ok := m[key]; ok {
		if f, ok := v.(float64); ok {
			return int(f)
		}
	}
	return 0
}

func truncate(s string, max int) string {
	// Take first line only
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > max {
		return s[:max-3] + "..."
	}
	return s
}
