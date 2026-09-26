package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
	"github.com/alexschlessinger/pollytool/llm/openai"
)

// terminalCodes are the backend refusals no retry can change: the plan's
// allowance is spent, or the plan carries no Codex access at all. An
// ordinary rate limit is not among them and is retried as usual.
var terminalCodes = map[string]bool{
	"usage_limit_reached": true,
	"usage_not_included":  true,
	"insufficient_quota":  true,
}

// UsageError is the backend refusing to serve the account on its plan.
type UsageError struct {
	Code    string
	Message string
	// Plan is the account's plan as the backend names it, when it said.
	Plan string
	// ResetsAt is when the allowance returns, when the backend said.
	ResetsAt time.Time
}

func (e *UsageError) Error() string {
	var b strings.Builder
	switch e.Code {
	case "usage_limit_reached":
		b.WriteString("codex: the ChatGPT usage limit is reached")
		if e.Plan != "" {
			fmt.Fprintf(&b, " (%s plan)", e.Plan)
		}
		if !e.ResetsAt.IsZero() {
			fmt.Fprintf(&b, "; it resets %s", resetText(e.ResetsAt))
		}
	case "usage_not_included":
		b.WriteString("codex: this ChatGPT plan does not include Codex")
	case "insufficient_quota":
		b.WriteString("codex: the ChatGPT account has no Codex quota left")
	default:
		fmt.Fprintf(&b, "codex: %s", e.Code)
	}
	if e.Message != "" && e.Code != "usage_limit_reached" {
		fmt.Fprintf(&b, ": %s", e.Message)
	}
	return b.String()
}

// resetText says when a window resets, relative when that is soon.
func resetText(at time.Time) string {
	until := time.Until(at)
	switch {
	case until <= 0:
		return "now"
	case until < time.Hour:
		return fmt.Sprintf("in about %d min", int(until.Minutes())+1)
	case until < 48*time.Hour:
		return fmt.Sprintf("in about %dh %02dm", int(until.Hours()), int(until.Minutes())%60)
	}
	return "at " + at.Local().Format("Jan 2 15:04")
}

// isTerminal reports an API error carrying a terminal refusal, for the
// retrier. A body outside the standard envelope reaches the error as raw
// text, so the codes are looked for there too.
func isTerminal(err error) bool {
	var apiErr *openai.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	if terminalCodes[apiErr.Type] || terminalCodes[string(apiErr.Code)] {
		return true
	}
	if rawBody(apiErr.Message) {
		for code := range terminalCodes {
			if strings.Contains(apiErr.Message, code) {
				return true
			}
		}
	}
	return false
}

// rawBody reports an error message that is a response body the standard
// envelope did not fit, which llm/openai passes through as it came.
func rawBody(message string) bool {
	return strings.HasPrefix(strings.TrimSpace(message), "{")
}

// refusalBody is a refusal as the backend spells it, under "error" in the
// standard envelope or under "detail" in its own.
type refusalBody struct {
	Type     string          `json:"type"`
	Code     json.RawMessage `json:"code"`
	Message  string          `json:"message"`
	PlanType string          `json:"plan_type"`
	ResetsAt json.RawMessage `json:"resets_at"`
	ResetsIn float64         `json:"resets_in_seconds"`
}

// parseUsageError reads a terminal refusal out of an error body, with the
// plan and reset time the backend adds to it; nil when the body is
// anything else.
func parseUsageError(body []byte) *UsageError {
	var envelope struct {
		Error  json.RawMessage `json:"error"`
		Detail json.RawMessage `json:"detail"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return nil
	}
	var refusal refusalBody
	for _, raw := range [][]byte{envelope.Error, envelope.Detail} {
		if len(raw) > 0 && raw[0] == '{' && json.Unmarshal(raw, &refusal) == nil {
			break
		}
	}
	code := refusal.Type
	if !terminalCodes[code] {
		code = strings.Trim(string(refusal.Code), `"`)
	}
	if !terminalCodes[code] {
		return nil
	}
	e := &UsageError{Code: code, Message: refusal.Message, Plan: refusal.PlanType}
	if raw := strings.Trim(string(refusal.ResetsAt), `"`); raw != "" && raw != "null" {
		e.ResetsAt = parseTimestamp(raw)
	}
	if e.ResetsAt.IsZero() && refusal.ResetsIn > 0 {
		e.ResetsAt = time.Now().Add(time.Duration(refusal.ResetsIn * float64(time.Second)))
	}
	return e
}

// detailMessage reads the backend's own refusal shape, {"detail": "..."},
// out of an error's raw text.
func detailMessage(message string) string {
	var body struct {
		Detail json.RawMessage `json:"detail"`
	}
	if json.Unmarshal([]byte(message), &body) != nil || len(body.Detail) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(body.Detail, &text) == nil {
		return text
	}
	var refusal refusalBody
	if json.Unmarshal(body.Detail, &refusal) == nil && refusal.Message != "" {
		return refusal.Message
	}
	return ""
}

// describe turns a transport or API failure into what the user should
// hear: the plan refusal with its reset time, a sign-in the backend no
// longer accepts, or the failure itself.
func describe(err error, call *callState) error {
	if err == nil {
		return nil
	}
	if refusal := call.terminalError(); refusal != nil && isTerminal(err) {
		return refusal
	}
	var apiErr *openai.APIError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.StatusCode == http.StatusUnauthorized:
			if refreshErr := call.refreshError(); refreshErr != nil {
				return refreshErr
			}
			return fmt.Errorf("codex: the backend rejected the sign-in; sign in again: %w", contract.ErrNotSignedIn)
		case isTerminal(err):
			code := apiErr.Type
			if !terminalCodes[code] {
				code = string(apiErr.Code)
			}
			if !terminalCodes[code] {
				for candidate := range terminalCodes {
					if strings.Contains(apiErr.Message, candidate) {
						code = candidate
					}
				}
			}
			return &UsageError{Code: code, Message: detailOr(apiErr.Message)}
		default:
			if detail := detailMessage(apiErr.Message); detail != "" {
				return fmt.Errorf("codex: %s (HTTP %d)", detail, apiErr.StatusCode)
			}
		}
	}
	return err
}

// detailOr is the detail a raw body carries, else the text as it is.
func detailOr(message string) string {
	if detail := detailMessage(message); detail != "" {
		return detail
	}
	return message
}
