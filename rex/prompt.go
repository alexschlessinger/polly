package rex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Prompts. A Rex app can ask the person a question in its command palette
// (rex.ui.pick and rex.ui.input), but only on behalf of an action defined in
// the server's configuration file: a script run by "rex do" runs in the CLI,
// where the same calls answer "unsupported" without asking anyone. So Pick and
// Input perform an action that init.lua has to define, which asks the question
// it is handed (AskActionLua writes one); every connected app shows it, and
// the first answer wins.

// PickItem is one row of a list the person chooses from.
type PickItem struct {
	// ID is what Pick returns for the row; it must be unique in the list.
	ID       string `json:"id"`
	Title    string `json:"title,omitempty"`
	Subtitle string `json:"subtitle,omitempty"`
	// Icon is one of Rex's icon names, such as terminal or clock.
	Icon string `json:"icon,omitempty"`
	// Detail is short text at the far end of the row.
	Detail string `json:"detail,omitempty"`
}

// PickRequest is a list for the person to choose one row of.
type PickRequest struct {
	Title string
	// Placeholder is the hint in the field that filters the list.
	Placeholder string
	Items       []PickItem
	// AllowOther lets the person answer with what they typed in that field
	// instead of a row.
	AllowOther bool
	// Timeout is how long the apps wait for an answer. Rex waits five
	// minutes by default and an hour at most.
	Timeout time.Duration
}

// PickAnswer is what the person answered a list with: the ID of a row, or
// with AllowOther the Text they typed instead.
type PickAnswer struct {
	ID   string
	Text string
}

// InputRequest is a line of text for the person to type.
type InputRequest struct {
	Title       string
	Placeholder string
	// Value is the text the field starts with, for the person to edit.
	Value string
	// Confirm labels the row that submits.
	Confirm string
	Timeout time.Duration
}

// The reasons a prompt ends without an answer.
const (
	PromptCancelled   = "cancelled"   // the person closed it
	PromptTimeout     = "timeout"     // nobody answered in time
	PromptNoClient    = "no_client"   // no app is connected
	PromptUnsupported = "unsupported" // no connected app can show prompts
	PromptBusy        = "busy"        // every app is showing another prompt
	PromptFailed      = "failed"      // anything else; Detail says what
)

// PromptError reports a prompt that ended without an answer.
type PromptError struct {
	Reason string
	Detail string
}

func (e *PromptError) Error() string {
	if e.Detail != "" {
		return "rex prompt: " + e.Reason + ": " + e.Detail
	}
	return "rex prompt: " + e.Reason
}

// Dismissed reports whether the person, rather than Rex, left the prompt
// unanswered: they closed it or let it time out.
func (e *PromptError) Dismissed() bool {
	return e.Reason == PromptCancelled || e.Reason == PromptTimeout
}

// MaxPickItems is the longest list Rex shows.
const MaxPickItems = 2000

// ErrNoAskAction reports that the configuration file defines no action of
// the name Pick or Input was given.
var ErrNoAskAction = errors.New("rex: the configuration file defines no such action")

// AskActionLua is the action Pick and Input perform, for init.lua to define
// under name: it asks the question it is handed, a list when the question has
// items and a line of text otherwise, and returns the answer.
func AskActionLua(name, title string) string {
	return `rex.action{
  name  = "` + name + `",
  title = "` + title + `",
  args  = { ask = "object" },
  run   = function(ctx, args)
    local ask = args.ask
    if type(ask) ~= "table" then return { why = "failed", detail = "only Polly runs this" } end
    local answer, why, detail
    if ask.items then
      answer, why, detail = rex.ui.pick(ask)
    else
      answer, why, detail = rex.ui.input(ask)
    end
    if answer == nil then return { why = why or "failed", detail = detail } end
    if type(answer) == "table" then return { text = answer.text } end
    if ask.items then return { id = answer } end
    return { text = answer }
  end,
}
`
}

// HasAction reports whether the server's configuration file defines an
// action called name.
func (c *Client) HasAction(ctx context.Context, name string) (bool, error) {
	actions, err := c.ListActions(ctx, "")
	if err != nil {
		return false, err
	}
	for _, action := range actions {
		if action.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// Pick shows req in the command palette of the connected Rex apps, through
// the configuration file's action (see AskActionLua), and returns the row the
// person chose. Without that action the error is ErrNoAskAction, and a prompt
// that ends without an answer is a *PromptError. Pick waits for as long as
// the prompt is open, so ctx should outlast req.Timeout.
func (c *Client) Pick(ctx context.Context, session, action string, req PickRequest) (PickAnswer, error) {
	if len(req.Items) > MaxPickItems {
		return PickAnswer{}, fmt.Errorf("rex prompt: %d items, at most %d", len(req.Items), MaxPickItems)
	}
	ask := map[string]any{"title": req.Title, "items": req.Items}
	if len(req.Items) == 0 {
		ask["items"] = []PickItem{}
	}
	if req.Placeholder != "" {
		ask["placeholder"] = req.Placeholder
	}
	if req.AllowOther {
		ask["allow_other"] = true
	}
	id, text, err := c.ask(ctx, session, action, ask, req.Timeout)
	if err != nil {
		return PickAnswer{}, err
	}
	if id == nil && (text == nil || !req.AllowOther) {
		return PickAnswer{}, &PromptError{Reason: PromptFailed, Detail: "the action returned no row"}
	}
	if id != nil {
		return PickAnswer{ID: *id}, nil
	}
	return PickAnswer{Text: *text}, nil
}

// Input asks for a line of text in the command palette of the connected Rex
// apps, as Pick asks for a row, and returns it exactly as typed: it may be
// empty.
func (c *Client) Input(ctx context.Context, session, action string, req InputRequest) (string, error) {
	ask := map[string]any{"title": req.Title}
	for key, value := range map[string]string{"placeholder": req.Placeholder, "value": req.Value, "confirm": req.Confirm} {
		if value != "" {
			ask[key] = value
		}
	}
	_, text, err := c.ask(ctx, session, action, ask, req.Timeout)
	if err != nil {
		return "", err
	}
	if text == nil {
		return "", &PromptError{Reason: PromptFailed, Detail: "the action returned no text"}
	}
	return *text, nil
}

// ask performs the action with one question and reports the row or the text
// it came back with.
func (c *Client) ask(ctx context.Context, session, action string, ask map[string]any, timeout time.Duration) (id, text *string, err error) {
	if title, _ := ask["title"].(string); strings.TrimSpace(title) == "" {
		return nil, nil, fmt.Errorf("rex prompt: title is required")
	}
	if defined, err := c.HasAction(ctx, action); err != nil {
		return nil, nil, err
	} else if !defined {
		return nil, nil, ErrNoAskAction
	}
	if timeout > 0 {
		ask["timeout"] = max(1, int(timeout.Seconds()))
	}
	encoded, err := json.Marshal(map[string]any{"ask": ask})
	if err != nil {
		return nil, nil, fmt.Errorf("rex prompt: encode: %w", err)
	}
	// The question goes in on stdin: a command line has no room for a long
	// list. "rex do" waits for the action however long the person takes.
	subArgs := []string{"do"}
	if sid := c.resolveSession(session); sid != "" {
		subArgs = append(subArgs, "-s", sid)
	}
	subArgs = append(subArgs, action, "--args", "-")
	var out struct {
		ID     *string `json:"id"`
		Text   *string `json:"text"`
		Why    string  `json:"why"`
		Detail string  `json:"detail"`
	}
	if err := c.runJSON(ctx, subArgs, encoded, "do "+action, &out); err != nil {
		return nil, nil, err
	}
	if out.ID == nil && out.Text == nil {
		reason := out.Why
		if reason == "" {
			reason = PromptFailed
		}
		return nil, nil, &PromptError{Reason: reason, Detail: out.Detail}
	}
	return out.ID, out.Text, nil
}

// SelectWindow asks the Rex app clientID to show a window of a session,
// switching to that session if it is showing another. The app's tab
// selection is its own: making a window the session's active one, as
// FocusWindow and NewWindow do, does not move it. An app accepts this only
// with Remote Control turned on in its settings, and refuses otherwise.
func (c *Client) SelectWindow(ctx context.Context, clientID, sessionID, windowID string) error {
	args := map[string]any{"session_id": sessionID}
	if windowID != "" {
		args["window_id"] = windowID
	}
	_, err := c.PerformClientAction(ctx, PerformActionRequest{ClientID: clientID, Action: "session.select", Args: args})
	return err
}
