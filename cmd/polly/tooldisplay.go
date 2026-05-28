package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
)

func toolLabel(tc messages.ChatMessageToolCall) string {
	summary := summarizeToolArgs(tc.Name, tc.Arguments)
	if summary == "" {
		return tc.Name
	}
	return tc.Name + " " + summary
}

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

func summarizeBashCommand(args tools.Args) string {
	cmd := args.String("command")
	if cmd == "" {
		return ""
	}

	lines := strings.SplitN(cmd, "\n", 20)
	first := lines[0]

	if idx := strings.Index(first, "<<"); idx >= 0 && len(lines) > 1 {
		prefix := strings.TrimSpace(first[:idx+2])
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
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > max {
		return s[:max-3] + "..."
	}
	return s
}

func toolDisplayEnabled(config *Config) bool {
	return !config.Quiet && isTerminal()
}
