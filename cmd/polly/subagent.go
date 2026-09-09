package main

import (
	"context"
	"strings"
	"sync"
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

// childTurnUI keeps the child's reply for the parent's tool result. Optional
// line activity reports progress without exposing that reply; approvals go to
// the parent's UI so a confirming user still decides.
type childTurnUI struct {
	parent   TurnUI
	activity *lineChildActivity

	mu    sync.Mutex
	text  strings.Builder
	reply string
	in    int
	out   int
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
	u.mu.Lock()
	defer u.mu.Unlock()
	u.text.WriteString(content)
}

// AppendToolStart drops the text streamed before a tool batch: that was
// the child working, not its reply.
func (u *childTurnUI) AppendToolStart(calls []messages.ChatMessageToolCall) {
	if u.activity != nil {
		u.activity.tools(calls)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.text.Reset()
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
	u.mu.Lock()
	defer u.mu.Unlock()
	u.in, u.out = in, out
}

func (u *childTurnUI) FinishTextTurn() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.reply = u.text.String()
}

func (u *childTurnUI) CompleteTurn(completion turnCompletion) {
	if u.activity != nil {
		u.activity.complete(completion)
	}
}

// result is the reply and usage; a turn that never finished (a failure)
// yields the text streamed so far.
func (u *childTurnUI) result() (reply string, in, out int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	reply = u.reply
	if reply == "" {
		reply = u.text.String()
	}
	return reply, u.in, u.out
}
