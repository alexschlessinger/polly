package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

// toolDisplayEnabled returns true when tool display should be shown.
func toolDisplayEnabled(config *Config) bool {
	return !config.Quiet && isTerminal()
}

// printToolStart prints a tool start indicator with summarized args to stderr.
func printToolStart(tc messages.ChatMessageToolCall) {
	fmt.Fprintf(os.Stderr, "  %s\n", dimStyle.Styled("→ "+toolLabel(tc)))
}

// printToolEnd prints a tool completion line with duration to stderr.
func printToolEnd(tc messages.ChatMessageToolCall, duration time.Duration, err error) {
	label := toolLabel(tc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  %s\n", errorStyle.Styled(fmt.Sprintf("✗ %.1fs %s — %s", duration.Seconds(), label, err)))
	} else {
		fmt.Fprintf(os.Stderr, "  %s\n", dimStyle.Styled(fmt.Sprintf("✓ %.1fs %s", duration.Seconds(), label)))
	}
}

func toolLabel(tc messages.ChatMessageToolCall) string {
	summary := summarizeToolArgs(tc.Name, tc.Arguments)
	if summary == "" {
		return tc.Name
	}
	return tc.Name + " " + summary
}

// summarizeToolArgs returns a one-line summary of tool arguments.
func summarizeToolArgs(toolName, argsJSON string) string {
	if argsJSON == "" {
		return ""
	}
	var rawArgs map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &rawArgs); err != nil {
		return ""
	}
	args := tools.Args(rawArgs)

	switch toolName {
	case "bash":
		return summarizeBashCommand(args)
	case "read":
		return summarizeReadArgs(args)
	case "write":
		return truncate(args.String("file_path"), 120)
	case "edit":
		return truncate(args.String("file_path"), 120)
	case "glob":
		return truncate(args.String("pattern"), 120)
	case "grep":
		return truncate(args.String("pattern"), 120)
	case "activate_skill":
		return truncate(args.String("name"), 120)
	case "read_skill_file":
		return summarizeReadSkillFileArgs(args)
	default:
		return ""
	}
}

func summarizeReadArgs(args tools.Args) string {
	summary := truncate(args.String("file_path"), 120)
	if offset := args.Int("offset", 0); offset > 0 {
		limit := args.Int("limit", 0)
		if limit > 0 {
			summary += fmt.Sprintf(" (lines %d-%d)", offset, offset+limit)
		} else {
			summary += fmt.Sprintf(" (from line %d)", offset)
		}
	}
	return summary
}

func summarizeReadSkillFileArgs(args tools.Args) string {
	skill := truncate(args.String("skill"), 120)
	path := truncate(args.String("path"), 120)
	if skill != "" && path != "" {
		return skill + "/" + path
	}
	return skill + path
}

// summarizeBashCommand returns a one-line summary for a bash command,
// collapsing heredocs to show the command prefix and first body line.
func summarizeBashCommand(args tools.Args) string {
	cmd := args.String("command")
	if cmd == "" {
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
