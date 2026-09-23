// Package replay is the model behind a headless shot fixture: a provider that
// plays scripted turns instead of calling an API, so a TUI frame that only a
// live stream shows — thinking, half-streamed text, a tool call being handed
// over, a stream error — can be captured deterministically and without a
// credential. The turns are installed by name; nothing routes to replay/<name>
// until then, so an interactive run naming it gets an error, not a script.
package replay

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/alexschlessinger/pollytool/messages"
)

// Turn is one scripted completion.
type Turn struct {
	// Match keys the turn to a request whose last user message contains it.
	// A request takes the first unconsumed turn keyed to it, else the first
	// unconsumed unkeyed turn.
	Match string `json:"match,omitempty"`
	Steps []Step `json:"steps,omitempty"`
	Usage *Usage `json:"usage,omitempty"`
	// Stop is the stop reason; empty is end_turn (tool_use when the turn
	// carries tool calls).
	Stop messages.StopReason `json:"stop,omitempty"`
	// Error fails the stream instead of playing steps.
	Error string `json:"error,omitempty"`
	// Mark is reported once the turn's stream has completed.
	Mark string `json:"mark,omitempty"`
}

// Usage is the token accounting a turn reports, with the cost a billing
// gateway would report for it.
type Usage struct {
	Input  int      `json:"input"`
	Output int      `json:"output"`
	Cost   *float64 `json:"cost,omitempty"`
}

// Step is one emit on the stream: exactly one of Reasoning, Content, Tool or
// Gate, optionally delayed and optionally marked once done.
type Step struct {
	Reasoning string    `json:"reasoning,omitempty"`
	Content   string    `json:"content,omitempty"`
	Tool      *ToolCall `json:"tool,omitempty"`
	// Gate blocks the stream until the shot script releases it.
	Gate string `json:"gate,omitempty"`
	// Mark is reported to the shot script once the step has been emitted.
	Mark    string `json:"mark,omitempty"`
	DelayMS int    `json:"delay_ms,omitempty"`
}

// ToolCall is a scripted call; Arguments is an object or a JSON string.
type ToolCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// arguments renders the call's arguments as the JSON text a model sends.
func (t *ToolCall) arguments() string {
	raw := strings.TrimSpace(string(t.Arguments))
	if raw == "" {
		return "{}"
	}
	var text string
	if json.Unmarshal(t.Arguments, &text) == nil {
		return text
	}
	return raw
}

// Validate reports the first mistake in turns, so a fixture fails before
// anything is played.
func Validate(turns []Turn) error {
	for i, t := range turns {
		if t.Error != "" && len(t.Steps) > 0 {
			return fmt.Errorf("turns[%d]: an error turn has no steps", i)
		}
		switch t.Stop {
		case "", messages.StopReasonEndTurn, messages.StopReasonToolUse, messages.StopReasonMaxTokens, messages.StopReasonContentFilter:
		default:
			return fmt.Errorf("turns[%d]: unknown stop %q", i, t.Stop)
		}
		for j, s := range t.Steps {
			kinds := 0
			for _, set := range []bool{s.Reasoning != "", s.Content != "", s.Tool != nil, s.Gate != ""} {
				if set {
					kinds++
				}
			}
			if kinds != 1 {
				return fmt.Errorf("turns[%d].steps[%d]: exactly one of reasoning, content, tool or gate", i, j)
			}
			if s.Tool != nil && s.Tool.Name == "" {
				return fmt.Errorf("turns[%d].steps[%d]: tool needs a name", i, j)
			}
			if s.DelayMS < 0 {
				return fmt.Errorf("turns[%d].steps[%d]: negative delay", i, j)
			}
		}
	}
	return nil
}

// Gates lists every gate the turns wait on.
func Gates(turns []Turn) map[string]bool {
	gates := map[string]bool{}
	for _, t := range turns {
		for _, s := range t.Steps {
			if s.Gate != "" {
				gates[s.Gate] = true
			}
		}
	}
	return gates
}

// Marks lists every mark the turns report.
func Marks(turns []Turn) map[string]bool {
	marks := map[string]bool{}
	for _, t := range turns {
		if t.Mark != "" {
			marks[t.Mark] = true
		}
		for _, s := range t.Steps {
			if s.Mark != "" {
				marks[s.Mark] = true
			}
		}
	}
	return marks
}
