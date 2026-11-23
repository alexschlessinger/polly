package sessions

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const MaxPlanBytes = 1 << 20
const maxPlanSteps = 256

// PlanRevision identifies exact bytes, including line endings and prose. It is
// not a hash of the normalized step titles.
func PlanRevision(text string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(text))) }

var (
	planHeading  = regexp.MustCompile(`^#{1,6}(?:[ \t]|$)`)
	stepsHeading = regexp.MustCompile(`^##[ \t]+Steps(?:[ \t]+#+)?[ \t]*$`)
	taskMarker   = regexp.MustCompile(`^\[[ xX]\](?:[ \t]+|$)`)
	planListItem = regexp.MustCompile(`^(?:[-+*]|[0-9]{1,9}[.)])(?:[ \t]+(.*)|$)`)
	planRule     = regexp.MustCompile(`^(?:(?:-[ \t]*){3,}|(?:\*[ \t]*){3,})$`)
)

func validPlanText(text string) bool {
	return utf8.ValidString(text) && !strings.ContainsFunc(text, func(r rune) bool {
		return unicode.IsControl(r) && r != '\t' && r != '\r' && r != '\n'
	})
}

// ParsePlanSteps implements the editable plan's deliberately small Markdown
// dialect. Steps are single-line, unindented list items under a single ATX
// "## Steps" heading. Another heading ends the list; nested items and prose
// are never lifted into outcomes. A task marker is presentation, not progress.
func ParsePlanSteps(text string) ([]string, error) {
	if len(text) > MaxPlanBytes {
		return nil, fmt.Errorf("plan exceeds %d bytes", MaxPlanBytes)
	}
	if !validPlanText(text) {
		return nil, fmt.Errorf("plan must be UTF-8 text without control characters")
	}
	var steps []string
	titles := make(map[string]bool)
	inSteps, found := false, false
	var fence string
	lineNo := 0
	for raw := range strings.SplitSeq(strings.TrimPrefix(text, "\uFEFF"), "\n") {
		lineNo++
		line := strings.TrimSuffix(raw, "\r")
		if strings.ContainsRune(line, '\r') {
			return nil, fmt.Errorf("plan line %d: unexpected carriage return", lineNo)
		}
		// Fences may be indented by up to three spaces. Closing fences
		// must use the same character and be at least as long as the opener.
		left := strings.TrimLeft(line, " ")
		if len(line)-len(left) <= 3 && len(left) >= 3 && (left[0] == '`' || left[0] == '~') {
			i := 1
			for i < len(left) && left[i] == left[0] {
				i++
			}
			if i >= 3 {
				if fence != "" {
					if left[0] == fence[0] && i >= len(fence) && strings.TrimSpace(left[i:]) == "" {
						fence = ""
					}
				} else if left[0] == '~' || !strings.ContainsRune(left[i:], '`') {
					fence = left[:i]
				}
				continue
			}
		}
		if fence != "" {
			continue
		}
		if planHeading.MatchString(line) {
			inSteps = stepsHeading.MatchString(line)
			if inSteps && found {
				return nil, fmt.Errorf("plan line %d: duplicate ## Steps heading", lineNo)
			}
			found = found || inSteps
			continue
		}
		if !inSteps || planRule.MatchString(line) {
			continue
		}
		item := planListItem.FindStringSubmatch(line)
		if item == nil {
			continue
		}
		title := strings.TrimSpace(item[1])
		title = strings.TrimSpace(strings.TrimPrefix(title, taskMarker.FindString(title)))
		if title == "" {
			return nil, fmt.Errorf("plan line %d: step title is empty", lineNo)
		}
		key := strings.ToLower(title)
		if titles[key] {
			return nil, fmt.Errorf("plan line %d: duplicate step title", lineNo)
		}
		if len(steps) == maxPlanSteps {
			return nil, fmt.Errorf("plan exceeds %d steps", maxPlanSteps)
		}
		titles[key] = true
		steps = append(steps, title)
	}
	if fence != "" {
		return nil, fmt.Errorf("plan has an unclosed Markdown fence")
	}
	if !found {
		return nil, fmt.Errorf("plan requires a ## Steps heading")
	}
	if len(steps) == 0 {
		return nil, fmt.Errorf("## Steps requires at least one top-level list item")
	}
	return steps, nil
}
