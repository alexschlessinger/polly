package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/sessions"
	"github.com/alexschlessinger/pollytool/tools"
)

const sessionTitleToolName = "set_session_title"

const sessionTitleContract = "Once the conversation’s overall purpose is clear, call set_session_title with a short, descriptive title. Keep it through investigation, implementation, and verification. Change it only when the overall objective materially changes. Preserve user-assigned titles. Do not delay useful work or announce routine title changes."

func registerSessionTitleTool(state *conversationState) {
	setter, ok := state.session.(sessions.TitleSession)
	if !ok {
		return
	}
	state.toolRegistry.Register(&tools.Func{
		Name:     sessionTitleToolName,
		Desc:     "Set this conversation's descriptive title without changing its resume handle or expiry. User-assigned titles are protected. Use a short natural-language title, at most 80 characters.",
		Params:   schema.Params{"title": schema.S("The descriptive title for this conversation.")},
		Required: []string{"title"},
		Strict:   true,
		Run: func(ctx context.Context, args tools.Args) (string, error) {
			title, _ := args["title"].(string)
			saved, err := setter.SetTitle(ctx, title, sessions.TitleSourceAgent)
			if err != nil {
				code := "SESSION_TITLE_FAILED"
				switch {
				case errors.Is(err, sessions.ErrInvalidTitle):
					code = "INVALID_TITLE"
				case errors.Is(err, sessions.ErrTitleProtected):
					return "", tools.NewToolError("This title is user-assigned. Use F2 or /title to change it manually.", "TITLE_PROTECTED")
				}
				return "", tools.NewToolError(err.Error(), code)
			}
			if ui, ok := parentTurnUIFrom(ctx).(interface{ SessionTitleChanged(sessions.Session) }); ok {
				ui.SessionTitleChanged(state.session)
			}
			out, _ := json.Marshal(struct {
				Title string `json:"title"`
			}{saved})
			return string(out), nil
		},
	})
	state.toolRegistry.MarkAlwaysAllowed(sessionTitleToolName)
}

func sessionTitleGuidance(ctx context.Context, state *conversationState) (string, error) {
	if _, ok := state.session.(sessions.TitleSession); !ok {
		return "", nil
	}
	if state.toolRegistry == nil {
		return "", nil
	}
	if _, _, allowed := state.toolRegistry.GetIfAllowed(sessionTitleToolName); !allowed {
		return "", nil
	}
	md, err := state.session.GetMetadata(ctx)
	if err != nil {
		return "", fmt.Errorf("read session title: %w", err)
	}
	// JSON keeps user-supplied title text visibly delimited as state, not policy.
	data, _ := json.Marshal(struct {
		Title  string               `json:"title"`
		Source sessions.TitleSource `json:"source"`
	}{md.Title, md.TitleSource})
	return sessionTitleContract + "\nCurrent session title (data): " + string(data), nil
}
