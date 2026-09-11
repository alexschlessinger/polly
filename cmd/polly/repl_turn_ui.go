package main

import (
	"context"
	"time"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/markdown"
	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/messages"
)

// gotuiTurnUI: the TurnUI implementation that pokes the model under lock.

type gotuiTurnUI struct {
	turnUIBase
	repl *managedREPL
	// model is the screen model of the tab this turn started on. Every
	// callback lands there, whichever tab is visible when it fires.
	model  *replModel
	config *Config
	// state is the session this turn started on, bound at start so an
	// in-place session switch cannot redirect a running turn.
	state       *conversationState
	turnID      int64
	reuseUser   bool
	turn        managedTurnInput
	persistence *turnPersistenceAck
}

func (t *gotuiTurnUI) UserMessagePersistenceStarted() {
	t.persistence.beginPersistence()
}

func (t *gotuiTurnUI) UserMessagePersistenceFinished(persisted bool) {
	t.persistence.finishPersistence(persisted)
}

func (t *gotuiTurnUI) activeLocked() bool {
	return t.turnID == 0 || t.model.turnID == t.turnID
}

func (t *gotuiTurnUI) acceptingLocked() bool {
	return t.activeLocked() && !t.model.canceling
}

// TurnPersistenceAllowed reports whether this turn may still append to the
// session. It is deliberately activeLocked, not acceptingLocked: an ordinary
// cancellation still persists the turn's completed work. Only a detached turn
// (^C cancellation timed out, generation advanced) is refused — newer turns
// may already be writing, and a late append would land out of order.
func (t *gotuiTurnUI) TurnPersistenceAllowed() bool {
	t.model.mu.Lock()
	defer t.model.mu.Unlock()
	return t.activeLocked()
}

// bindMemberUI gives the tab's session a screen for swarm members that run
// outside a turn, so their approvals land in this tab like a turn's would.
func (r *managedREPL) bindMemberUI(tab *replTab) {
	if tab == nil || tab.state == nil {
		return
	}
	tab.state.setMemberUI(&gotuiTurnUI{repl: r, model: tab.model, config: r.config, state: tab.state})
}

func (t *gotuiTurnUI) ShowThinking(chunk string) {
	t.model.mu.Lock()
	if !t.acceptingLocked() {
		t.model.mu.Unlock()
		return
	}
	t.model.state = turnStateThinking
	t.model.appendThinking(chunk)
	t.model.mu.Unlock()
}

func (t *gotuiTurnUI) AppendAssistantText(content string) {
	t.model.mu.Lock()
	if !t.acceptingLocked() {
		t.model.mu.Unlock()
		return
	}
	t.model.state = turnStateStreaming
	if content != "" {
		t.model.turnHasOutput = true
		// Prose is the aggregation boundary: it closes the reasoning run and
		// the tool run before the first token lands, so activity after the
		// prose opens fresh indicators below it.
		t.model.finishThinkingSegment()
		t.model.completeToolDisclosure()
	}
	t.model.appendAssistant(content)
	t.model.mu.Unlock()
}

func (t *gotuiTurnUI) AppendToolStart(calls []messages.ChatMessageToolCall) {
	t.model.mu.Lock()
	defer t.model.mu.Unlock()
	if !t.acceptingLocked() {
		return
	}
	if len(calls) > 0 {
		t.model.turnHasOutput = true
		// Tools pause the thinking clock without closing the record; an
		// unbroken continuation resumes the same indicator.
		t.model.pauseThinkingSegment()
		t.model.finishAssistantBlock("")
		t.model.runningTools += len(calls)
		t.model.state = turnStateTool
		t.model.toolName = calls[0].Name
	}
	if !toolDisplayEnabled(t.config) {
		for _, call := range calls {
			t.model.inspections.startTool(call)
		}
		return
	}
	var record *toolDisclosureRecord
	for _, c := range calls {
		record = t.model.appendToolCallStart(c)
	}
	t.model.refreshToolDisclosure(record)
}

func (t *gotuiTurnUI) ApproveToolCalls(ctx context.Context, requester string, calls []messages.ChatMessageToolCall) []bool {
	if len(calls) == 0 {
		return nil
	}
	t.model.mu.Lock()
	if ctx.Err() != nil || !t.acceptingLocked() || t.model.approvalsClosed {
		t.model.mu.Unlock()
		return denyToolCalls(calls)
	}
	if !t.config.Confirm {
		t.model.mu.Unlock()
		return approveAllToolCalls(calls)
	}
	a := &approvalState{ctx: ctx, requester: requester, calls: append([]messages.ChatMessageToolCall(nil), calls...), reply: make(chan []bool, 1)}
	t.model.approvalQueue = append(t.model.approvalQueue, a)
	t.model.advanceApprovalLocked()
	t.model.mu.Unlock()
	t.wakeApprovals()
	select {
	case results, ok := <-a.reply:
		if !ok || ctx.Err() != nil {
			return denyToolCalls(calls)
		}
		return results
	case <-ctx.Done():
		t.model.mu.Lock()
		t.model.resolveApprovalLocked(a, denyToolCalls(calls))
		t.model.mu.Unlock()
		t.wakeApprovals()
		return denyToolCalls(calls)
	}
}

func (t *gotuiTurnUI) wakeApprovals() {
	if t.repl != nil {
		t.repl.wakeTabs()
	}
}

func (t *gotuiTurnUI) AppendToolEnd(call messages.ChatMessageToolCall, result string, duration time.Duration, err error) {
	label := style.StripImageMarkers(toolLabel(call))
	denied := toolWasDenied(result)
	var discoveredImages []style.Image
	if toolDisplayEnabled(t.config) && !denied {
		// Tool output can be large. Discovery touches only Markdown/path syntax
		// and the filesystem, so keep it outside the model lock and let the TUI
		// continue painting while it runs.
		discoveredImages = markdown.DiscoverToolOutputImages(result, t.model.imageBaseDir)
	}
	t.model.mu.Lock()
	defer t.model.mu.Unlock()
	m := t.model
	if !t.acceptingLocked() {
		return
	}
	if m.runningTools > 0 {
		m.runningTools--
	}
	m.inspections.finishTool(call, result, duration, err)
	m.turnHasOutput = true
	// Return to "waiting" only once every tool in the batch has finished;
	// otherwise the first of several parallel tools to complete would flip the
	// status back to waiting (and drop the running-tool name) mid-batch.
	batchDone := m.busy && m.runningTools == 0
	if batchDone {
		m.state = turnStateWaiting
		m.toolName = ""
	}
	if !toolDisplayEnabled(t.config) {
		return
	}
	final := inlineToolLine{modifier: "bold", duration: formatElapsed(duration)}
	switch {
	case denied:
		final.glyph, final.tone, final.meta, final.duration = "✗", "err", "denied", ""
	case err != nil:
		final.glyph, final.tone, final.meta = "✗", "err", toolFailureMeta(err)
	default:
		final.glyph, final.tone, final.meta = "✓", "ok", resultLineMeta(result)
	}
	images := discoveredImages
	// Freeze the final line over its running disclosure row. Fall back to a new
	// row if the display was cleared while the tool was in flight.
	record := m.currentToolDisclosure()
	if rowIndex, ok := m.takeActiveTool(call.ID); ok && record != nil && rowIndex >= 0 && rowIndex < len(record.rows) {
		row := &record.rows[rowIndex]
		row.finishAgentCall(call, denied, err)
		row.setLine(final)
		row.images = append([]style.Image(nil), images...)
		row.settled = true
	} else {
		record = m.ensureToolDisclosure()
		record.rows = append(record.rows, toolDisclosureRow{
			callID:  call.ID,
			label:   label,
			images:  append([]style.Image(nil), images...),
			settled: true,
		})
		row := &record.rows[len(record.rows)-1]
		row.setCall(call)
		row.setLine(final)
		row.finishAgentCall(call, denied, err)
	}
	m.refreshToolDisclosure(record)
	if call.Name == "spawn_agent" {
		m.refreshAgentRecord(record)
	}
	// The disclosure stays live past the batch: an unbroken continuation's
	// next batch folds into it. Assistant prose or turn settlement closes it.
}

func (t *gotuiTurnUI) AppendToolMedia(call messages.ChatMessageToolCall, images []style.Image) {
	if len(images) == 0 || (t.config != nil && t.config.Quiet) {
		return
	}
	t.model.mu.Lock()
	defer t.model.mu.Unlock()
	m := t.model
	if !t.acceptingLocked() {
		return
	}
	record, row := m.toolDisclosureRowForCall(call.ID)
	if row == nil {
		record = m.ensureToolDisclosure()
		record.rows = append(record.rows, toolDisclosureRow{
			callID:  call.ID,
			label:   style.StripImageMarkers(toolLabel(call)),
			settled: true,
		})
		row = &record.rows[len(record.rows)-1]
		row.setCall(call)
		row.setLine(inlineToolLine{glyph: "✓", tone: "ok", modifier: "bold"})
	}
	m.mutateAnchored(m.disclosureLayoutWidth(0), matchToolGroup([]int64{record.id}), func(bool) {
		row.inspectionImages = append([]style.Image(nil), images...)
		m.refreshToolDisclosureWithAnchor(record, false)
		// The third Images field and its gallery are derived from
		// inspectionImages; neither necessarily changes the canonical raw tool
		// text.
		m.visual.invalidate()
	})
}

func (t *gotuiTurnUI) AppendToolResult(call messages.ChatMessageToolCall, result messages.ChatMessage) {
	t.model.mu.Lock()
	defer t.model.mu.Unlock()
	if t.activeLocked() {
		t.model.inspections.setResult(call, result)
	}
}

func (t *gotuiTurnUI) AppendWarning(text string) {
	t.model.mu.Lock()
	if !t.acceptingLocked() {
		t.model.mu.Unlock()
		return
	}
	t.model.appendNoticeLine("Warning: " + text)
	t.model.turnHasOutput = true
	t.model.mu.Unlock()
}

func (t *gotuiTurnUI) RecordTurnTokens(in, out int) {
	t.model.mu.Lock()
	if !t.acceptingLocked() {
		t.model.mu.Unlock()
		return
	}
	t.model.lastIn = in
	t.model.lastOut = out
	t.model.turnDock.inputTokens = in
	t.model.turnDock.outputTokens = out
	t.model.mu.Unlock()
}

func (t *gotuiTurnUI) RecordContextUsage(used, limit int, estimated bool) {
	t.model.mu.Lock()
	if t.acceptingLocked() {
		t.model.status.recordContextUsage(used, limit, estimated)
	}
	t.model.mu.Unlock()
}

func (t *gotuiTurnUI) FinishTextTurn() {
	t.model.mu.Lock()
	accepted := t.acceptingLocked()
	if accepted {
		t.model.finishAssistantBlock("")
	}
	t.model.mu.Unlock()
}

func (t *gotuiTurnUI) CompleteTurn(completion turnCompletion) {
	t.model.mu.Lock()
	accepted := t.activeLocked() && t.model.completion == nil
	if accepted {
		t.model.completion = &completion
	}
	t.model.mu.Unlock()
}
