package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/swarm"
	"github.com/alexschlessinger/pollytool/tools"
)

// testAskTurnUI is a TurnUI that answers ask_user without a screen, recording
// the request it was handed.
type testAskTurnUI struct {
	turnUIBase
	req    askRequest
	answer askAnswer
	calls  int
}

func (u *testAskTurnUI) AskUser(_ context.Context, req askRequest) askAnswer {
	u.req, u.calls = req, u.calls+1
	return u.answer
}

func (u *testAskTurnUI) ShowThinking(string)                                         {}
func (u *testAskTurnUI) AppendAssistantText(string)                                  {}
func (u *testAskTurnUI) AppendToolStart([]messages.ChatMessageToolCall)              {}
func (u *testAskTurnUI) AppendWarning(string)                                        {}
func (u *testAskTurnUI) RecordTurnTokens(int, int, bool)                             {}
func (u *testAskTurnUI) CompleteTurn(turnCompletion)                                 {}
func (u *testAskTurnUI) AppendToolMedia(messages.ChatMessageToolCall, []style.Image) {}

func (u *testAskTurnUI) ApproveToolCalls(context.Context, string, []messages.ChatMessageToolCall) []bool {
	return nil
}

func (u *testAskTurnUI) AppendToolEnd(messages.ChatMessageToolCall, string, time.Duration, error) {}

func validAskArgs() tools.Args {
	return tools.Args{
		"question": "Which database should the migration target?",
		"header":   "Database migration",
		"options": []any{
			map[string]any{"label": "PostgreSQL", "description": "the existing prod instance"},
			map[string]any{"label": "SQLite", "description": "keep local development only"},
		},
	}
}

func TestAskUserValidatesItsArguments(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(tools.Args)
		want   string
	}{
		{name: "valid"},
		{name: "empty question", mutate: func(args tools.Args) { args["question"] = "  \n " }, want: "question is required"},
		{name: "long question", mutate: func(args tools.Args) { args["question"] = strings.Repeat("x", askQuestionBytes+1) }, want: "question is at most 8192 bytes"},
		{name: "long header", mutate: func(args tools.Args) { args["header"] = strings.Repeat("x", askHeaderRunes+1) }, want: "header is at most 48 characters"},
		{name: "one option", mutate: func(args tools.Args) { args["options"] = []any{map[string]any{"label": "Only"}} }, want: "2 to 6 choices"},
		{name: "seven options", mutate: func(args tools.Args) {
			var options []any
			for i := range 7 {
				options = append(options, map[string]any{"label": string(rune('A' + i))})
			}
			args["options"] = options
		}, want: "2 to 6 choices"},
		{name: "options not a list", mutate: func(args tools.Args) { args["options"] = "PostgreSQL" }, want: "2 to 6 choices"},
		{name: "empty label", mutate: func(args tools.Args) {
			args["options"] = []any{map[string]any{"label": " "}, map[string]any{"label": "B"}}
		}, want: "option 1 needs a label"},
		{name: "long label", mutate: func(args tools.Args) {
			args["options"] = []any{map[string]any{"label": strings.Repeat("x", askLabelRunes+1)}, map[string]any{"label": "B"}}
		}, want: "label is at most 60 characters"},
		{name: "long description", mutate: func(args tools.Args) {
			args["options"] = []any{map[string]any{"label": "A", "description": strings.Repeat("x", askDescriptionBytes+1)}, map[string]any{"label": "B"}}
		}, want: "description is at most 240 bytes"},
		{name: "duplicate labels", mutate: func(args tools.Args) {
			args["options"] = []any{map[string]any{"label": "A"}, map[string]any{"label": " A "}}
		}, want: "repeats the label"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			args := validAskArgs()
			if test.mutate != nil {
				test.mutate(args)
			}
			req, err := parseAskRequest(args)
			if test.want == "" {
				if err != nil {
					t.Fatalf("parseAskRequest = %v", err)
				}
				if !req.Other {
					t.Fatal("the free-text row is not offered by default")
				}
				if len(req.Options) != 2 || req.Options[0].Label != "PostgreSQL" {
					t.Fatalf("options = %+v", req.Options)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestAskUserHonoursOtherFalse(t *testing.T) {
	args := validAskArgs()
	args["other"] = false
	req, err := parseAskRequest(args)
	if err != nil {
		t.Fatal(err)
	}
	if req.Other {
		t.Fatal("other: false still offers the free-text row")
	}
}

func TestAskUserReturnsTheChoice(t *testing.T) {
	ui := &testAskTurnUI{answer: askAnswer{Outcome: askOutcomeAnswered, Index: 1, Choice: "SQLite"}}
	ctx := withParentTurnUI(context.Background(), ui)
	out, err := runAskUser(ctx, validAskArgs())
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Outcome string `json:"outcome"`
		Choice  string `json:"choice"`
		Index   int    `json:"index"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("result %q: %v", out, err)
	}
	if result.Outcome != askOutcomeAnswered || result.Choice != "SQLite" || result.Index != 1 {
		t.Fatalf("result = %+v", result)
	}
	if ui.req.Header != "Database migration" || len(ui.req.Options) != 2 {
		t.Fatalf("request = %+v", ui.req)
	}
}

func TestAskUserReturnsFreeText(t *testing.T) {
	ui := &testAskTurnUI{answer: askAnswer{Outcome: askOutcomeAnswered, Index: 2, Choice: askOtherLabel, Text: "use the prod DSN"}}
	ctx := withParentTurnUI(context.Background(), ui)
	out, err := runAskUser(ctx, validAskArgs())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"text":"use the prod DSN"`) || !strings.Contains(out, `"choice":"Other…"`) {
		t.Fatalf("result = %q", out)
	}
}

func TestAskUserReturnsDismissalAsAResult(t *testing.T) {
	ui := &testAskTurnUI{answer: askAnswer{Outcome: askOutcomeDismissed, Why: "the user closed the question"}}
	ctx := withParentTurnUI(context.Background(), ui)
	out, err := runAskUser(ctx, validAskArgs())
	if err != nil {
		t.Fatalf("a dismissal is not an error: %v", err)
	}
	if !strings.Contains(out, `"outcome":"dismissed"`) || !strings.Contains(out, "the user closed the question") {
		t.Fatalf("result = %q", out)
	}
}

func TestAskUserLabelsTheAskingMember(t *testing.T) {
	ui := &testAskTurnUI{answer: askAnswer{Outcome: askOutcomeDismissed}}
	ctx := withAskingMember(withParentTurnUI(context.Background(), ui), "scout")
	if _, err := runAskUser(ctx, validAskArgs()); err != nil {
		t.Fatal(err)
	}
	if ui.req.Requester != "scout" {
		t.Fatalf("requester = %q", ui.req.Requester)
	}
}

func TestAskUserFailsWithoutADialog(t *testing.T) {
	_, err := runAskUser(context.Background(), validAskArgs())
	var toolErr *tools.ToolError
	if !errors.As(err, &toolErr) || toolErr.Code != askCodeUnreachable {
		t.Fatalf("error = %v, want %s", err, askCodeUnreachable)
	}
}

func TestAskUserReportsABusyTab(t *testing.T) {
	ui := &testAskTurnUI{answer: askAnswer{Outcome: askOutcomeBusy}}
	ctx := withParentTurnUI(context.Background(), ui)
	_, err := runAskUser(ctx, validAskArgs())
	var toolErr *tools.ToolError
	if !errors.As(err, &toolErr) || toolErr.Code != askCodeBusy {
		t.Fatalf("error = %v, want %s", err, askCodeBusy)
	}
}

func TestAskUserIsRegisteredOnlyInTheTUI(t *testing.T) {
	line := tools.NewToolRegistry(nil)
	registerAskUserTool(&conversationState{toolRegistry: line, outputCapabilities: outputCapabilities{surface: outputSurfaceLineANSI}})
	if _, ok := line.Get(askUserToolName); ok {
		t.Fatal("a line surface registered ask_user")
	}
	if _, ok := line.Get(askUserToolName); ok {
		t.Fatal("an unregistered registry serves ask_user")
	}

	registry := tools.NewToolRegistry(nil)
	registerAskUserTool(&conversationState{toolRegistry: registry, outputCapabilities: outputCapabilities{surface: outputSurfaceManagedTUI}})
	tool, ok := registry.Get(askUserToolName)
	if !ok {
		t.Fatal("the managed TUI did not register ask_user")
	}
	if untimed, ok := tool.(tools.UntimedTool); !ok || !untimed.Untimed() {
		t.Fatal("ask_user is timed, so a slow answer would be cut off")
	}
	if exclusive, ok := tool.(tools.ExclusiveTool); !ok || !exclusive.ExclusiveBatch() {
		t.Fatal("ask_user is not exclusive, so it could run beside a mutating call")
	}
	if _, recall := tools.RecallStub(tool); recall {
		t.Fatal("ask_user claims a recall stub; its answer is not reproducible")
	}
	if tools.ReadsOnly(tool) {
		t.Fatal("ask_user claims ReadsOnly; asking again is not free")
	}
	// A skill's allow-list must not hide the tool it asks with.
	if _, ok := registry.Derive(tools.AllowTools("nothing-matches")).Get(askUserToolName); !ok {
		t.Fatal("a tool allow-list hides ask_user")
	}
}

func TestAskUserRowSummaryAndMeta(t *testing.T) {
	if got := summarizeAskUserArgs(validAskArgs()); got != "Which database should the migration target?" {
		t.Fatalf("summary = %q", got)
	}
	if got := summarizeAskUserArgs(tools.Args{"question": "## **Pick one**\n\nDetails."}); got != "Pick one" {
		t.Fatalf("markdown summary = %q", got)
	}
	if got := toolResultMeta(askUserToolName, `{"outcome":"answered","choice":"PostgreSQL","index":0}`); got != "PostgreSQL" {
		t.Fatalf("answered meta = %q", got)
	}
	if got := toolResultMeta(askUserToolName, `{"outcome":"answered","choice":"Other…","index":2,"text":"prod"}`); got != "Other…: prod" {
		t.Fatalf("free-text meta = %q", got)
	}
	if got := toolResultMeta(askUserToolName, `{"outcome":"dismissed"}`); got != "dismissed" {
		t.Fatalf("dismissed meta = %q", got)
	}
	if got := toolResultMeta("bash", "one\ntwo"); got != "2 lines" {
		t.Fatalf("bash meta = %q", got)
	}
}

func TestAskUserRowShowsTheQuestionAsMarkdown(t *testing.T) {
	call := messages.ChatMessageToolCall{
		Name:      askUserToolName,
		Arguments: `{"question":"Target **PostgreSQL 16**, which prod runs.\n\n- no schema port\n- one week","options":[{"label":"PostgreSQL","description":"prod"}]}`,
	}
	result := messages.ChatMessage{Role: messages.MessageRoleTool, ToolName: askUserToolName, Content: `{"outcome":"answered","choice":"PostgreSQL","index":0}`}
	row := toolDisclosureRow{toolName: askUserToolName, output: newInlineToolOutput(call, result), settled: true}
	label, body, _, _ := row.inlineOutputSection(80)
	plain := plainStyledText(label)
	if plain != "question" {
		t.Fatalf("label = %q", plain)
	}
	text := plainStyledText(body)
	for _, want := range []string{"Target PostgreSQL 16, which prod runs.", "no schema port", "→ PostgreSQL"} {
		if !strings.Contains(text, want) {
			t.Fatalf("body = %q, missing %q", text, want)
		}
	}
	if strings.Contains(text, `"outcome"`) {
		t.Fatalf("body repeats the JSON result: %q", text)
	}
	// The width is part of the cache key: a narrower row reflows.
	if narrow := plainStyledText(row.output.questionBody(30)); !strings.Contains(narrow, "Target") {
		t.Fatalf("narrow body = %q", narrow)
	}
	if got := row.output.rendered.width; got != 30 {
		t.Fatalf("rendered width = %d", got)
	}
}

// TestAskUserSurvivesAMemberBinding pins the declaration a member's tool list
// depends on: a bound registry keeps only tools that can rebind themselves or
// say they do not need to, and ask_user's dialog belongs to the parent tab.
func TestAskUserSurvivesAMemberBinding(t *testing.T) {
	parent := tools.NewToolRegistry(nil, tools.WithNativeTools(), tools.WithUnsafeNoSandbox())
	defer parent.Close()
	registerAskUserTool(&conversationState{toolRegistry: parent, outputCapabilities: outputCapabilities{surface: outputSurfaceManagedTUI}})
	root := t.TempDir()
	ec, err := parent.ExecutionPolicy(root, tools.ExecutionGrant{SourceRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	bound, omitted, err := parent.BindExecutionContext(ec, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	if _, ok := bound.Get(askUserToolName); !ok {
		t.Fatalf("a member's bound registry omitted ask_user (omitted: %v)", omitted)
	}
	// The member's own agent derives from the bound registry, so the tool must
	// survive that too.
	derived := bound.Derive()
	defer derived.Close()
	if _, ok := derived.Get(askUserToolName); !ok {
		t.Fatal("the member's derived registry lost ask_user")
	}
}

// TestMemberCallbacksLabelTheAskingMember pins the plumbing between a member's
// tool run and the dialog: the member's name rides the call's context, and the
// parent tab's UI is the one that answers.
func TestMemberCallbacksLabelTheAskingMember(t *testing.T) {
	state := &conversationState{}
	state.setMemberUI(&gotuiTurnUI{})
	cb := memberUICallbacks(context.Background(), &Config{}, state, swarm.Member{ID: "scout"})
	if cb.BeforeToolExecute == nil {
		t.Fatal("a member's callbacks carry no BeforeToolExecute")
	}
	ctx := cb.BeforeToolExecute(context.Background(), messages.ChatMessageToolCall{Name: askUserToolName}, nil)
	if got := askingMemberFrom(ctx); got != "scout" {
		t.Fatalf("asking member = %q", got)
	}
	if parentTurnUIFrom(ctx) == nil {
		t.Fatal("the call carries no parent UI")
	}
}
