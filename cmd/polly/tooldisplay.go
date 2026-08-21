package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/tools"
	rw "github.com/mattn/go-runewidth"
)

// toolWasDenied reports whether a tool result represents a user denial rather
// than real tool output. The agent substitutes the llm.ToolDeniedContent
// sentinel for the result of an unapproved call; this predicate owns that
// contract so the renderers don't each hard-code the comparison.
func toolWasDenied(result string) bool {
	return result == llm.ToolDeniedContent
}

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
		return summarizeGenericToolArgs(rawArgs)
	}
}

const genericToolSummaryWidth = 120

// summarizeGenericToolArgs gives custom and MCP tools a useful approval label
// without dumping arbitrary payloads into the terminal. Only scalar top-level
// values are shown; nested objects and arrays are represented by their shape.
// Sorting makes the result stable across Go's randomized map iteration order.
func summarizeGenericToolArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}

	keys := make([]string, 0, len(args))
	for key := range args {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		value := genericToolArgValue(args[key])
		if sensitiveToolArgKey(key) {
			value = "<redacted>"
		}
		parts = append(parts, compactToolArgKey(key)+"="+value)
	}
	return truncate(strings.Join(parts, ", "), genericToolSummaryWidth)
}

func compactToolArgKey(key string) string {
	key = strings.Join(strings.Fields(key), " ")
	return truncate(key, 32)
}

func genericToolArgValue(value any) string {
	switch v := value.(type) {
	case nil:
		return "null"
	case string:
		return strconv.Quote(v)
	case bool:
		return strconv.FormatBool(v)
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64)
	case map[string]any:
		return fmt.Sprintf("{%d fields}", len(v))
	case []any:
		return fmt.Sprintf("[%d items]", len(v))
	default:
		// JSON decoding should constrain values to the cases above. Keep the
		// fallback opaque in case that contract changes; never dump an unknown
		// nested payload into an approval prompt.
		return "<value>"
	}
}

func sensitiveToolArgKey(key string) bool {
	words := splitToolArgKey(key)
	for _, word := range words {
		switch word {
		case "token", "key", "secret", "password", "auth", "authorization", "authentication", "credential", "credentials", "cookie", "cookies":
			return true
		}
		// Cover common unsplit spellings such as accesstoken, clientsecret,
		// apikey, and cookiejar without treating ordinary keys like "author"
		// or "monkey" as sensitive.
		for _, marker := range []string{"token", "secret", "password", "credential", "cookie"} {
			if strings.Contains(word, marker) {
				return true
			}
		}
		switch word {
		case "apikey", "accesskey", "privatekey", "secretkey", "signingkey":
			return true
		}
	}
	return false
}

func splitToolArgKey(key string) []string {
	var normalized strings.Builder
	var previous rune
	for _, r := range key {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			normalized.WriteByte(' ')
			previous = 0
			continue
		}
		if unicode.IsUpper(r) && previous != 0 && (unicode.IsLower(previous) || unicode.IsDigit(previous)) {
			normalized.WriteByte(' ')
		}
		normalized.WriteRune(unicode.ToLower(r))
		previous = r
	}
	return strings.Fields(normalized.String())
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
	if rw.StringWidth(s) > max {
		return rw.Truncate(s, max, "...")
	}
	return s
}

func toolDisplayEnabled(config *Config) bool {
	return !config.Quiet && isTerminal()
}
