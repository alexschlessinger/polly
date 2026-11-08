package sessions

import (
	"encoding/json"
	"strings"
	"unicode"
)

// CoordinationDisplayReader is an optional callback-local, display-only reader.
// Its compact facts carry no coordination authority. A mark is scoped to one
// durable family incarnation; an empty/foreign mark requests a cold snapshot.
// Returned values are owned by the caller and may outlive the callback.
type CoordinationDisplayReader interface {
	DisplayChanges(mark string) (DisplayChanges, error)
}

type DisplayChanges struct {
	Mark    string
	Reset   bool
	Records []DisplayRecord
}
type DisplayRecord struct {
	Kind, ID string
	Value    json.RawMessage // nil is a deletion, not an absent source record
}
type DisplayMember struct {
	Member, Execution, Task, Message json.RawMessage
	Presentation                     json.RawMessage
	Decisions                        int
	First                            *SemanticDecision
}
type DisplayWorkflow struct {
	Workflow  json.RawMessage
	Decisions int
}
type DisplayHeader struct {
	Counts            SemanticCounts
	HasRuns           bool
	ParentDisposition string
	First             *SemanticDecision
	RetryPause        json.RawMessage
	ColdCompatibility bool
}
type DisplayLaunch struct{ CallID, Member string }

// CompactDisplayRecord strips source-only fields before they enter display
// storage or a compatibility snapshot. In particular it never retains intent,
// schemas, result values, output artifacts, workflow input/output, or receipts.
func CompactDisplayRecord(kind string, payload json.RawMessage) (json.RawMessage, error) {
	if payload == nil {
		return nil, nil
	}
	var fields []string
	switch kind {
	case "member":
		fields = []string{"id", "name", "label", "task", "execution", "control", "controller", "context", "sandboxPresets"}
	case "execution":
		fields = []string{"id", "member", "status", "generation", "workflow", "error", "started", "ended", "stopReason", "retryPause", "iterations", "usage"}
	case "task":
		fields = []string{"id", "description", "status", "revision"}
	case "workflow":
		fields = []string{"id", "name", "status", "started", "callID"}
	case "workflow_step":
		fields = []string{"id", "kind", "status"}
	case "mail":
		fields = []string{"id", "from", "to", "kind", "delivered", "posted"}
	case "parent_turn":
		fields = []string{"retryPause"}
	default:
		return nil, nil
	}
	var src map[string]json.RawMessage
	if err := json.Unmarshal(payload, &src); err != nil {
		return nil, err
	}
	out := map[string]json.RawMessage{}
	for _, key := range fields {
		if v, ok := src[key]; ok {
			out[key] = v
		}
	}
	subset := func(raw json.RawMessage, keys ...string) (json.RawMessage, error) {
		var from map[string]json.RawMessage
		if err := json.Unmarshal(raw, &from); err != nil {
			return nil, err
		}
		to := map[string]json.RawMessage{}
		for _, key := range keys {
			if v, ok := from[key]; ok {
				to[key] = v
			}
		}
		return json.Marshal(to)
	}
	if kind == "execution" {
		if raw := src["request"]; len(raw) > 0 {
			v, err := subset(raw, "callID", "label", "task", "taskID", "maxIterations")
			if err != nil {
				return nil, err
			}
			out["request"] = v
		}
		if raw := src["result"]; len(raw) > 0 && string(raw) != "null" {
			v, err := subset(raw, "usage")
			if err != nil {
				return nil, err
			}
			out["result"] = v
		}
	}
	if kind == "task" {
		var text string
		if json.Unmarshal(src["result"], &text) == nil {
			heading, line := "", ""
			for part := range strings.SplitSeq(text, "\n") {
				part = strings.TrimSpace(part)
				if part == "" {
					continue
				}
				if strings.HasPrefix(part, "#") {
					if heading == "" {
						heading = strings.TrimLeft(part, "# ")
					}
					continue
				}
				line = part
				break
			}
			if line == "" {
				line = heading
			}
			out["result"], _ = json.Marshal(displaySanitize(line))
		}
	}
	if kind == "mail" {
		var text string
		if err := json.Unmarshal(src["text"], &text); err != nil && len(src["text"]) > 0 {
			return nil, err
		}
		text = strings.Join(strings.Fields(displaySanitize(text)), " ")
		runes := []rune(text)
		if len(runes) > 160 {
			text = string(runes[:159]) + "…"
		}
		out["text"], _ = json.Marshal(text)
	}
	return json.Marshal(out)
}

// Match the TUI's image-marker/control sanitization before clipping: invisible
// markers and whitespace must not consume the preview's 160-rune budget.
func displaySanitize(text string) string {
	return strings.Map(func(r rune) rune {
		if r >= '\ue000' && r < '\ue100' {
			return -1
		}
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, text)
}
