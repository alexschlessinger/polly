package main

// ask_user: the TUI-only tool that stops a turn to ask the user a question
// with enumerated choices, and continues with the answer in context.
//
// It is the model-facing half of the question dialog (repl_ask_dialog.go) and
// follows set_theme's availability rule: the dialog needs the managed TUI's
// event loop, so a line, one-shot or raw run never sees the tool. The tool
// reaches the screen through a TurnUI capability resolved from the context,
// the way set_theme resolves themeUI and sandbox_propose resolves
// sandboxReviewer; nothing in llm/ knows about it.
//
// Two rules shape the contract:
//
//   - A dismissed question is a successful result, not an error. The user
//     declined to decide, and the description tells the model to stop and
//     report what it needs rather than pick an option for them.
//   - The answer is not reproducible information, so the tool is not a recall
//     tool and never claims ReadsOnly: a journaled batch that asked the user
//     must not be replayed as if re-asking cost nothing.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/alexschlessinger/pollytool/schema"
	"github.com/alexschlessinger/pollytool/tools"
)

const askUserToolName = "ask_user"

// The limits one call may name. They bound the dialog, not the model: a
// question longer than the window would be unreadable, and a description
// longer than a sentence would push the choices off the screen.
const (
	askQuestionBytes    = 8 << 10
	askMinOptions       = 2
	askMaxOptions       = 6
	askLabelRunes       = 60
	askDescriptionBytes = 240
	askHeaderRunes      = 48
)

// The error codes the tool's failures carry.
const (
	askCodeInvalidQuestion = "INVALID_QUESTION"
	askCodeUnreachable     = "USER_UNREACHABLE"
	askCodeBusy            = "ASK_BUSY"
)

// The outcomes an answer reports. answered and dismissed are results;
// busy and unreachable are failures the tool turns into ToolErrors.
const (
	askOutcomeAnswered    = "answered"
	askOutcomeDismissed   = "dismissed"
	askOutcomeBusy        = "busy"
	askOutcomeUnreachable = "unreachable"
)

// askOtherLabel is the free-text row's label and the choice an answer that
// used it reports.
const askOtherLabel = "Other…"

// askOption is one choice: a short label and the sentence that explains what
// picking it means.
type askOption struct {
	Label       string
	Description string
}

// askRequest is one question as the dialog shows it.
type askRequest struct {
	Question string
	Header   string
	Options  []askOption
	// Other adds a free-text row. Defaults to true in parseAskRequest.
	Other bool
	// Requester names the swarm member asking, or is "" for the tab's own
	// turn. The dialog titles itself with it so a member's question is not
	// mistaken for the turn's.
	Requester string
}

// askAnswer is how a question ended. Index is the chosen option's position,
// or len(Options) when the free-text row was used; Text is what was typed
// there.
type askAnswer struct {
	Outcome string
	Index   int
	Choice  string
	Text    string
	Why     string
}

// askUI is the TurnUI capability ask_user needs: a question only the user
// answers, returning once they answer, dismiss it, or ctx ends.
type askUI interface {
	AskUser(ctx context.Context, req askRequest) askAnswer
}

// askUserTool is ask_user's tool value. It declares itself context-independent
// because the dialog lives on the parent tab, not in the caller's workspace:
// without the declaration a member's bound registry would omit it as a local
// tool it cannot rebind, and a member could never ask.
type askUserTool struct {
	tools.Func
}

func (*askUserTool) ContextIndependent() bool { return true }

// registerAskUserTool installs ask_user for a managed-TUI session and marks it
// built-in, so no tool view or allow-list can hide the affordance the surface
// itself provides: a skill that needs a decision, and a member whose bound
// registry restricts its tools, must keep the tool they ask with. The surface
// gate is the whole availability rule.
func registerAskUserTool(state *conversationState) {
	if state == nil || state.toolRegistry == nil || state.outputCapabilities.surface != outputSurfaceManagedTUI {
		return
	}
	state.toolRegistry.Register(&askUserTool{Func: tools.Func{
		Name: askUserToolName,
		Desc: "Ask the user a question with enumerated choices and wait for the answer. Use it when a decision is " +
			"genuinely the user's and cannot be resolved from the workspace: which of two designs to take, which " +
			"environment to target, whether to keep a change. question is Markdown shown above the choices; each " +
			"option's description is one short sentence explaining its consequence. The user answers in one " +
			"keystroke and the turn continues with the answer. Do not use it for open-ended questions, or for " +
			"anything you can check yourself: end the turn and ask in prose instead. An outcome of \"dismissed\" " +
			"means the user declined to decide: stop and report what you need, and never pick an option for them. " +
			"Images in question are not shown in the dialog.",
		Params: schema.Params{
			"question": schema.S("Markdown shown above the choices. Keep it short; the dialog shows it in a scrolling window."),
			"header":   schema.S("Optional one-line title for the dialog, at most 48 characters."),
			"options": map[string]any{
				"type":        "array",
				"description": "Two to six choices, in the order they should appear.",
				"minItems":    askMinOptions,
				"maxItems":    askMaxOptions,
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"label":       schema.S("The choice as the user sees it, at most 60 characters."),
						"description": schema.S("One sentence explaining what picking this means, at most 240 bytes."),
					},
					"required":             []any{"label", "description"},
					"additionalProperties": false,
				},
			},
			"other": map[string]any{
				"type":        "boolean",
				"description": "Add an \"Other…\" row that takes a typed answer. Defaults to true.",
				"default":     true,
			},
		},
		Required:    []string{"question", "options"},
		Strict:      true,
		LongRunning: true,
		Exclusive:   true,
		Run:         runAskUser,
	}})
	state.toolRegistry.MarkBuiltin(askUserToolName)
}

// runAskUser is ask_user: validate the call, then wait for the user.
func runAskUser(ctx context.Context, args tools.Args) (string, error) {
	req, err := parseAskRequest(args)
	if err != nil {
		return "", tools.NewToolError(err.Error(), askCodeInvalidQuestion)
	}
	req.Requester = askingMemberFrom(ctx)
	asker, ok := parentTurnUIFrom(ctx).(askUI)
	if !ok {
		return "", tools.NewToolError("the user cannot be asked from here: this run has no question dialog (ask_user is available in the TUI only)", askCodeUnreachable)
	}
	if err := ctx.Err(); err != nil {
		return askDismissedJSON(askAnswer{Why: "the turn ended"})
	}
	answer := asker.AskUser(ctx, req)
	switch answer.Outcome {
	case askOutcomeAnswered:
		return askAnsweredJSON(answer)
	case askOutcomeBusy:
		return "", tools.NewToolError("a dialog is already open on this tab; ask again once it closes", askCodeBusy)
	case askOutcomeUnreachable:
		why := answer.Why
		if why == "" {
			why = "the user cannot be asked from here"
		}
		return "", tools.NewToolError(why, askCodeUnreachable)
	default:
		return askDismissedJSON(answer)
	}
}

// parseAskRequest validates one call. Every failure names the argument to
// correct, since the model reads it and retries.
func parseAskRequest(args tools.Args) (askRequest, error) {
	req := askRequest{
		Question: strings.TrimSpace(args.String("question")),
		Header:   strings.TrimSpace(args.String("header")),
		Other:    true,
	}
	if raw, ok := args["other"]; ok {
		if other, ok := raw.(bool); ok {
			req.Other = other
		}
	}
	switch {
	case req.Question == "":
		return askRequest{}, fmt.Errorf("question is required")
	case len(req.Question) > askQuestionBytes:
		return askRequest{}, fmt.Errorf("question is at most %d bytes; this one is %d", askQuestionBytes, len(req.Question))
	case utf8.RuneCountInString(req.Header) > askHeaderRunes:
		return askRequest{}, fmt.Errorf("header is at most %d characters", askHeaderRunes)
	}
	list, ok := args["options"].([]any)
	if !ok || len(list) < askMinOptions || len(list) > askMaxOptions {
		return askRequest{}, fmt.Errorf("options must list %d to %d choices", askMinOptions, askMaxOptions)
	}
	seen := make(map[string]bool, len(list))
	for i, entry := range list {
		fields, _ := entry.(map[string]any)
		label := strings.TrimSpace(stringOf(fields["label"]))
		description := strings.TrimSpace(stringOf(fields["description"]))
		switch {
		case label == "":
			return askRequest{}, fmt.Errorf("option %d needs a label", i+1)
		case utf8.RuneCountInString(label) > askLabelRunes:
			return askRequest{}, fmt.Errorf("option %d: label is at most %d characters", i+1, askLabelRunes)
		case len(description) > askDescriptionBytes:
			return askRequest{}, fmt.Errorf("option %d: description is at most %d bytes", i+1, askDescriptionBytes)
		case seen[label]:
			return askRequest{}, fmt.Errorf("option %d repeats the label %q; labels must be distinct", i+1, label)
		}
		seen[label] = true
		req.Options = append(req.Options, askOption{Label: label, Description: description})
	}
	return req, nil
}

func stringOf(v any) string {
	s, _ := v.(string)
	return s
}

// askAnsweredResult and askDismissedResult are the durable shapes the model
// reads back. Index is always present so the model can refer to the choice by
// position as well as by label.
type askAnsweredResult struct {
	Outcome string `json:"outcome"`
	Choice  string `json:"choice"`
	Index   int    `json:"index"`
	Text    string `json:"text,omitempty"`
}

type askDismissedResult struct {
	Outcome string `json:"outcome"`
	Why     string `json:"why,omitempty"`
}

func askAnsweredJSON(answer askAnswer) (string, error) {
	encoded, err := json.Marshal(askAnsweredResult{Outcome: askOutcomeAnswered, Choice: answer.Choice, Index: answer.Index, Text: answer.Text})
	if err != nil {
		return "", tools.NewToolError("the answer could not be encoded: "+err.Error(), "ANSWER_ENCODE_FAILED")
	}
	return string(encoded), nil
}

func askDismissedJSON(answer askAnswer) (string, error) {
	why := answer.Why
	if why == "" {
		why = "the user closed the question"
	}
	encoded, err := json.Marshal(askDismissedResult{Outcome: askOutcomeDismissed, Why: why})
	if err != nil {
		return "", tools.NewToolError("the answer could not be encoded: "+err.Error(), "ANSWER_ENCODE_FAILED")
	}
	return string(encoded), nil
}
