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

// printToolStart prints a tool start indicator with summarized args to stderr.
func printToolStart(tc messages.ChatMessageToolCall) {
	summary := summarizeToolArgs(tc.Name, tc.Arguments)
	label := tc.Name
	if summary != "" {
		label += " " + summary
	}
	fmt.Fprintf(os.Stderr, "  %s\n", dimStyle.Styled("→ "+label))
}

// printToolEnd prints a tool completion line with duration to stderr.
func printToolEnd(tc messages.ChatMessageToolCall, duration time.Duration, err error) {
	summary := summarizeToolArgs(tc.Name, tc.Arguments)
	label := tc.Name
	if summary != "" {
		label += " " + summary
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "  %s\n", errorStyle.Styled(fmt.Sprintf("✗ %.1fs %s — %s", duration.Seconds(), label, err)))
	} else {
		fmt.Fprintf(os.Stderr, "  %s\n", dimStyle.Styled(fmt.Sprintf("✓ %.1fs %s", duration.Seconds(), label)))
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
		return summarizeBashCommand(args)
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
	case "activate_skill":
		return strVal(args, "name")
	case "read_skill_file":
		skill := strVal(args, "skill")
		path := strVal(args, "path")
		if skill != "" && path != "" {
			return skill + "/" + path
		}
		return skill + path
	default:
		return ""
	}
}

// summarizeBashCommand returns a one-line summary for a bash command,
// collapsing heredocs to show the command prefix and first body line.
func summarizeBashCommand(args map[string]any) string {
	cmd, ok := args["command"].(string)
	if !ok || cmd == "" {
		return ""
	}

	lines := strings.SplitN(cmd, "\n", 20)
	first := lines[0]

	// Detect heredoc: look for <<EOF, <<'EOF', <<"EOF", <<-EOF etc.
	if idx := strings.Index(first, "<<"); idx >= 0 && len(lines) > 1 {
		prefix := strings.TrimSpace(first[:idx+2])
		// Find first non-empty body line (skip the delimiter line)
		for _, line := range lines[1:] {
			line = strings.TrimSpace(line)
			if line != "" && line != "EOF" && line != "'EOF'" {
				return truncate(prefix+" "+line, 120)
			}
		}
	}

	return truncate(first, 120)
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
