package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/control"
	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/messages"
)

// remoteTurnView tells a turn's UI what a backend's events say of the root
// it watches, as the agent's callbacks tell it of a turn run here. It runs on
// the one goroutine that follows the turn.
type remoteTurnView struct {
	state            *conversationState
	ui               TurnUI
	settled, schema  bool
	root             control.ID
	onChild          func(control.Event)
	children         *remoteChildren
	committed        uint64
	toldSince        int
	seen             map[string]uint64
	afterReasoning   bool
	calls            map[string]messages.ChatMessageToolCall
	ends             map[string]remoteToolEnd
	started          map[string]bool
	peakIn, totalOut int
	cacheRead        int
	cacheWrite       int
	summaryIn        int
	summaryOut       int
	toolCalls        int
	toolErrors       int
	lastTool         string
	lastAnswer       string
	ended            bool
	endReply         *control.Reply
	endRefused       *control.Refusal
	interrupted      bool
}

type remoteToolEnd struct {
	took   time.Duration
	failed bool
}

func newRemoteTurnView(state *conversationState, ui TurnUI, settled, schema bool) *remoteTurnView {
	v := &remoteTurnView{state: state, ui: ui, settled: settled, schema: schema, seen: map[string]uint64{}, calls: map[string]messages.ChatMessageToolCall{}, ends: map[string]remoteToolEnd{}, started: map[string]bool{}, children: newRemoteChildren()}
	if state != nil && state.remote != nil {
		v.root = state.remote.session.ID()
	}
	return v
}

// watched takes what a watch found: the committed count, and, of a root at
// rest on a watch that asked for its answer, how its turn ended.
func (v *remoteTurnView) watched(w control.Watched) {
	if w.Row == nil {
		return
	}
	v.committed = max(v.committed, w.Lines, w.Row.At)
	if w.Row.State != control.StateRunning && (w.Reply != nil || w.Refused != nil) {
		v.ended, v.endReply, v.endRefused = true, w.Reply, w.Refused
	}
}

func (v *remoteTurnView) refs() control.OutputRefs {
	if refs, ok := v.state.artifactStore.(control.OutputRefs); ok {
		return refs
	}
	return nil
}

// event tells one event.
func (v *remoteTurnView) event(ev control.Event) {
	if ev.Agent != v.root {
		v.children.event(v.ui, ev)
		if v.onChild != nil {
			v.onChild(ev)
		}
		return
	}
	switch {
	case ev.Delta != nil:
		v.delta(*ev.Delta)
	case ev.Message != nil:
		v.toldSince++
		v.message(ev.Message.Line, ev.Message.Usage)
	case ev.Tool != nil:
		v.tool(*ev.Tool)
	case ev.Notice != nil:
		v.notice(*ev.Notice)
	case ev.Checkpoint != nil:
		if ev.Checkpoint.Lines != nil {
			v.committed = max(v.committed, *ev.Checkpoint.Lines)
		}
		v.toldSince = 0
	case ev.End != nil:
		v.ended, v.endReply, v.endRefused = true, ev.End.Reply, ev.End.Refused
	}
}

func (v *remoteTurnView) delta(d control.Delta) {
	if d.Last {
		// The reply's text has ended: the next reply's text starts at 0.
		v.seen = map[string]uint64{}
		return
	}
	text := d.Text
	if seen := v.seen[d.Kind]; d.At < seen {
		// Told before (a watcher that came late is told from where it
		// was): only what is past it is new.
		skip := seen - d.At
		if skip >= uint64(len(text)) {
			return
		}
		text = text[skip:]
	}
	v.seen[d.Kind] = max(v.seen[d.Kind], d.At+uint64(len(d.Text)))
	switch d.Kind {
	case control.TextReasoning:
		if text != "" {
			v.afterReasoning = true
			v.ui.ShowThinking(text)
		}
	case control.TextAnswer:
		v.ui.ThinkingEnded()
		if v.schema || v.settled {
			return
		}
		if v.afterReasoning {
			text = trimLeadingResponseNewlines(text)
			if text == "" {
				return
			}
			v.afterReasoning = false
		}
		v.ui.AppendAssistantText(text)
	case control.TextCommentary:
		v.ui.ThinkingEnded()
	}
}

func (v *remoteTurnView) message(raw json.RawMessage, usage *control.LineUsage) {
	msg, err := control.LineToMessage(raw, v.root, v.refs())
	if err != nil {
		v.ui.AppendWarning("a transcript line could not be read: " + err.Error())
		return
	}
	switch msg.Role {
	case messages.MessageRoleAssistant:
		v.ui.ThinkingEnded()
		for _, call := range msg.ToolCalls {
			v.calls[call.ID] = call
		}
		if answer := messages.AnswerText(msg.TextBlocks); answer != "" {
			v.lastAnswer = answer
		} else if msg.Content != "" {
			v.lastAnswer = msg.Content
		}
		if usage != nil && usage.TokensReported {
			v.peakIn = max(v.peakIn, int(usage.InputTokens))
			v.totalOut += int(usage.OutputTokens)
			v.ui.RecordTurnTokens(v.peakIn, v.totalOut+v.summaryOut, false)
			if usage.InputTokens > 0 {
				v.ui.RecordContextUsage(int(usage.InputTokens), false, v.budget())
			}
		}
		if usage != nil && usage.CacheReported {
			v.cacheRead += int(usage.CacheReadTokens)
			v.cacheWrite += int(usage.CacheWriteTokens)
		}
		// A reply that came whole ends its text, mark or no mark.
		v.seen = map[string]uint64{}
		v.afterReasoning = false
	case messages.MessageRoleTool:
		v.toolResult(msg)
	}
}

func (v *remoteTurnView) budget() contextBudgetDetails {
	if b := v.state.currentBudget(&v.state.settings); b != nil {
		return *b
	}
	return contextBudgetDetails{input: v.state.settings.MaxHistoryTokens}
}

func (v *remoteTurnView) callOf(id, name string) messages.ChatMessageToolCall {
	if call, ok := v.calls[id]; ok {
		return call
	}
	return messages.ChatMessageToolCall{ID: id, Name: name, Arguments: "{}"}
}

func (v *remoteTurnView) tool(t control.ToolEvent) {
	switch t.State {
	case "start":
		call := v.callOf(t.ID, t.Name)
		if call.DisplayID == "" {
			call.DisplayID = t.Display
		}
		v.calls[t.ID] = call
		v.started[t.ID] = true
		v.toolCalls++
		v.lastTool = t.Name
		v.ui.AppendToolStart([]messages.ChatMessageToolCall{call})
		v.children.spawned(call)
	case "end":
		v.ends[t.ID] = remoteToolEnd{took: time.Duration(t.Ns), failed: t.Failed}
	}
}

// toolResult ends a call with what it answered: the backend tells a call's
// end before its result's line, and polly's UI takes the two together.
func (v *remoteTurnView) toolResult(msg messages.ChatMessage) {
	call := v.callOf(msg.ToolCallID, msg.ToolName)
	end := v.ends[msg.ToolCallID]
	content := msg.GetContent()
	var err error
	if ok, present := msg.Metadata[messages.MetadataKeyToolSucceeded].(bool); end.failed || (present && !ok) {
		err = errors.New(firstLine(content, "the tool failed"))
		v.toolErrors++
	}
	if !v.started[msg.ToolCallID] {
		v.ui.AppendToolStart([]messages.ChatMessageToolCall{call})
	}
	delete(v.started, msg.ToolCallID)
	delete(v.ends, msg.ToolCallID)
	v.ui.AppendToolEnd(call, content, end.took, err)
	v.ui.AppendToolResult(call, msg)
	v.children.result(v.ui, call, content)
	if images := inspectionTranscriptImages(msg, v.state.artifactStore); len(images) > 0 {
		v.ui.AppendToolMedia(call, images)
	}
}

func firstLine(text, otherwise string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	if line == "" {
		return otherwise
	}
	return line
}

func (v *remoteTurnView) notice(n control.Notice) {
	switch n.Kind {
	case "note":
		if n.Text == "" {
			return
		}
		if n.Feature == "compaction" {
			v.ui.AppendNotice(n.Text)
			return
		}
		v.ui.AppendWarning(n.Text)
	case "retry":
		ui, ok := v.ui.(retryStatusUI)
		if !ok {
			return
		}
		event := llm.RetryEvent{Waiting: n.Waiting, RetryCount: int(n.Attempt), Delay: time.Duration(n.DelayNs), Canceled: n.Cancelled}
		if n.Waiting {
			event.NextRetryAt = time.Now().Add(event.Delay)
		}
		if n.Cause != "" {
			event.Err = errors.New(n.Cause)
		}
		if n.WaitError != "" {
			event.WaitError = errors.New(n.WaitError)
		}
		ui.ModelRetry(event)
	case "summary":
		if n.Usage != nil {
			v.summaryIn += int(n.Usage.InputTokens)
			v.summaryOut += int(n.Usage.OutputTokens)
			v.ui.RecordTurnTokens(v.peakIn, v.totalOut+v.summaryOut, false)
		}
	}
}

// catchUp tells the committed lines a connection that ended missed: those
// past the ones it was told.
func (v *remoteTurnView) catchUp(lines []json.RawMessage) {
	skip := min(v.toldSince, len(lines))
	for _, raw := range lines[skip:] {
		msg, err := control.LineToMessage(raw, v.root, v.refs())
		if err != nil {
			continue
		}
		switch msg.Role {
		case messages.MessageRoleAssistant:
			if text := messages.AnswerText(msg.TextBlocks); text != "" && !v.settled && !v.schema {
				v.ui.AppendAssistantText(text)
			} else if msg.Content != "" && !v.settled && !v.schema {
				v.ui.AppendAssistantText(msg.Content)
			}
			for _, call := range msg.ToolCalls {
				v.calls[call.ID] = call
				if !v.started[call.ID] {
					v.started[call.ID] = true
					v.ui.AppendToolStart([]messages.ChatMessageToolCall{call})
				}
			}
			v.message(raw, nil)
		case messages.MessageRoleTool:
			v.toolResult(msg)
		}
	}
	v.committed += uint64(len(lines))
	v.toldSince = 0
}

// endAnswer is how the turn ended, as its end event said.
func (v *remoteTurnView) endAnswer() (control.Reply, error) {
	switch {
	case v.endReply != nil:
		return *v.endReply, nil
	case v.endRefused != nil:
		return control.Reply{}, fmt.Errorf("the turn failed: %s", v.endRefused.Text)
	}
	return control.Reply{}, errors.New("the turn ended and its answer was not kept")
}

// finish settles the turn's UI from how the turn ended, and returns the exit
// code its outcome maps to.
func (v *remoteTurnView) finish(ctx context.Context, config *Config, schema *llm.Schema, reply control.Reply, complete func(messages.StopReason, error), model string, started time.Time) (int, error) {
	for id := range v.started {
		end := v.ends[id]
		v.ui.AppendToolEnd(v.callOf(id, ""), "", end.took, nil)
	}
	v.started = map[string]bool{}
	in := v.peakIn
	if in == 0 {
		in = int(reply.Usage.InputTokens)
	}
	out := int(reply.Usage.OutputTokens)
	v.ui.RecordTurnTokens(in, out, false)
	if reply.Context.Used > 0 {
		budget := v.budget()
		if reply.Context.Size > 0 {
			budget.input = int(reply.Context.Size)
		}
		v.state.recordBudget(&v.state.settings, budget)
		v.ui.RecordContextUsage(int(reply.Context.Used), false, budget)
	}
	reason := messages.StopReasonEndTurn
	code := 0
	var err error
	switch reply.Stop {
	case control.StopEndTurn:
	case control.StopContentFilter:
		reason = messages.StopReasonContentFilter
	case control.StopMaxTokens:
		reason, code = messages.StopReasonMaxTokens, 2
		v.ui.AppendWarning("response truncated " + truncationCause(0, v.state.settings.MaxTokens))
	case control.StopMaxIterations:
		reason, code = messages.StopReasonMaxIterations, 3
		err = fmt.Errorf("%w (%d model calls)", llm.ErrMaxIterations, reply.ModelCalls)
	case control.StopCancelled:
		reason, code = messages.StopReasonError, 130
		if v.interrupted && context.Cause(ctx) != nil {
			err = context.Cause(ctx)
		} else {
			err = errStoppedElsewhere
		}
	default:
		reason, code = messages.StopReasonError, 1
		err = errors.New(firstLine(reply.Failure, "the turn failed in the backend"))
	}
	if err == nil && config.SchemaPath != "" {
		if line, ok := v.ui.(*lineTurnUI); ok {
			line.pauseActivity()
		}
		output := ""
		if reply.Output != nil {
			output = *reply.Output
		}
		if serr := outputStructured(output, schema); serr != nil {
			err, code, reason = serr, 1, messages.StopReasonError
		}
	}
	if v.settled && config.SchemaPath == "" {
		if err == nil && v.lastAnswer != "" {
			v.ui.AppendAssistantText(v.lastAnswer)
		} else if err != nil {
			name, _ := v.state.session.GetName(context.WithoutCancel(ctx))
			text := v.lastAnswer
			if text == "" {
				text = "Blocked: " + err.Error() + "\n\nSession: " + name + "\n"
			}
			v.ui.AppendAssistantText(text)
		}
	}
	v.ui.FinishTextTurn()
	if outputErr := flushTurnOutputError(v.ui); outputErr != nil {
		err, code = errors.Join(err, outputErr), 1
	}
	complete(reason, err)
	if config.Meta {
		meta := metaFields{
			StopReason: reason, Model: model, Iterations: int(reply.ModelCalls), ToolCalls: int(reply.ToolCalls), ToolErrors: int(reply.ToolFails),
			LastTool: v.lastTool, InputTokens: int(reply.Usage.InputTokens), OutputTokens: int(reply.Usage.OutputTokens),
			CacheReadTokens: int(reply.Usage.CacheReadTokens), CacheWriteTokens: int(reply.Usage.CacheWriteTokens), DurationMS: time.Since(started).Milliseconds(),
		}
		if err != nil {
			meta.Err = err.Error()
		}
		writeMetaTrailer(os.Stderr, meta)
	}
	return code, err
}
