package main

import (
	"context"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
	"github.com/alexschlessinger/pollytool/swarm"
)

type parentTurnUIKey struct{}

// withParentTurnUI hands a turn's UI to the tools it runs.
func withParentTurnUI(ctx context.Context, turnUI TurnUI) context.Context {
	return context.WithValue(ctx, parentTurnUIKey{}, turnUI)
}

func parentTurnUIFrom(ctx context.Context) TurnUI {
	turnUI, _ := ctx.Value(parentTurnUIKey{}).(TurnUI)
	return turnUI
}

type toolCallKey struct{}

// withToolCall hands a tool the call it is answering, so a spawned child can
// attach itself to that call's disclosure row.
func withToolCall(ctx context.Context, call messages.ChatMessageToolCall) context.Context {
	return context.WithValue(swarm.WithWorkflowCallID(ctx, call.ID), toolCallKey{}, call)
}

func toolCallFrom(ctx context.Context) messages.ChatMessageToolCall {
	call, _ := ctx.Value(toolCallKey{}).(messages.ChatMessageToolCall)
	return call
}

// childTurnUI reports child activity and routes approvals to the parent.
// The swarm runtime owns the child's reply and usage.
type childTurnUI struct {
	parent   TurnUI
	activity *lineChildActivity
}

func (u *childTurnUI) Start() {}
func (u *childTurnUI) Stop()  {}
func (u *childTurnUI) ShowThinking(chunk string) {
	if u.activity != nil && chunk != "" {
		u.activity.phase(turnStateThinking)
	}
}
func (u *childTurnUI) AppendWarning(text string) {
	if u.activity != nil {
		u.activity.warning(text)
	}
}
func (u *childTurnUI) RecordContextUsage(int, int, bool)   {}
func (u *childTurnUI) UserMessagePersistenceStarted()      {}
func (u *childTurnUI) UserMessagePersistenceFinished(bool) {}
func (u *childTurnUI) TurnPersistenceAllowed() bool        { return true }
func (u *childTurnUI) AppendToolEnd(call messages.ChatMessageToolCall, result string, duration time.Duration, err error) {
	if u.activity != nil {
		u.activity.toolEnd(call, result, duration, err)
	}
}
func (u *childTurnUI) AppendToolMedia(_ messages.ChatMessageToolCall, images []transcriptImage) {
	if u.activity != nil {
		u.activity.media(images)
	}
}

func (u *childTurnUI) childActivity(call messages.ChatMessageToolCall) *lineChildActivity {
	if u.activity == nil {
		return nil
	}
	return u.activity.ui.newChildActivity(u.activity.scope, call)
}

func (u *childTurnUI) AppendAssistantText(content string) {
	if u.activity != nil && content != "" {
		u.activity.phase(turnStateStreaming)
	}
}

func (u *childTurnUI) AppendToolStart(calls []messages.ChatMessageToolCall) {
	if u.activity != nil {
		u.activity.tools(calls)
	}
}

func (u *childTurnUI) ApproveToolCalls(calls []messages.ChatMessageToolCall) []bool {
	if u.parent != nil {
		return u.parent.ApproveToolCalls(calls)
	}
	approved := make([]bool, len(calls))
	for i := range approved {
		approved[i] = true
	}
	return approved
}

func (u *childTurnUI) RecordTurnTokens(in, out int) {
	if u.activity != nil {
		u.activity.usage(in, out)
	}
}

func (u *childTurnUI) FinishTextTurn() {}

func (u *childTurnUI) CompleteTurn(completion turnCompletion) {
	if u.activity != nil {
		u.activity.complete(completion)
	}
}
