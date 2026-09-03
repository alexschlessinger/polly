package main

import (
	"fmt"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
)

// gotuiTurnUI: the TurnUI implementation that pokes the model under lock.

type gotuiTurnUI struct {
	repl        *managedREPL
	config      *Config
	turnID      int64
	reuseUser   bool
	turn        managedTurnInput
	persistence *turnPersistenceAck
}

func (t *gotuiTurnUI) Start() {}
func (t *gotuiTurnUI) Stop()  {}

func (t *gotuiTurnUI) UserMessagePersistenceStarted() {
	t.persistence.beginPersistence()
}

func (t *gotuiTurnUI) UserMessagePersistenceFinished(persisted bool) {
	t.persistence.finishPersistence(persisted)
}

func (t *gotuiTurnUI) activeLocked() bool {
	return t.turnID == 0 || t.repl.model.turnID == t.turnID
}

func (t *gotuiTurnUI) acceptingLocked() bool {
	return t.activeLocked() && !t.repl.model.canceling
}

// TurnPersistenceAllowed reports whether this turn may still append to the
// session. It is deliberately activeLocked, not acceptingLocked: an ordinary
// cancellation still persists the turn's completed work. Only a detached turn
// (^C cancellation timed out, generation advanced) is refused — newer turns
// may already be writing, and a late append would land out of order.
func (t *gotuiTurnUI) TurnPersistenceAllowed() bool {
	t.repl.model.mu.Lock()
	defer t.repl.model.mu.Unlock()
	return t.activeLocked()
}

func denyToolCalls(calls []messages.ChatMessageToolCall) []bool {
	return make([]bool, len(calls))
}

func (t *gotuiTurnUI) ShowThinking(chunk string) {
	t.repl.model.mu.Lock()
	if !t.acceptingLocked() {
		t.repl.model.mu.Unlock()
		return
	}
	t.repl.model.state = turnStateThinking
	t.repl.model.appendThinking(chunk)
	t.repl.model.mu.Unlock()
}

func (t *gotuiTurnUI) AppendAssistantText(content string) {
	t.repl.model.mu.Lock()
	if !t.acceptingLocked() {
		t.repl.model.mu.Unlock()
		return
	}
	t.repl.model.state = turnStateStreaming
	if content != "" {
		t.repl.model.turnHasOutput = true
		// Prose is the aggregation boundary: it closes the reasoning run and
		// the tool run before the first token lands, so activity after the
		// prose opens fresh indicators below it.
		t.repl.model.finishThinkingSegment()
		t.repl.model.completeToolDisclosure()
	}
	t.repl.model.appendAssistant(content)
	t.repl.model.mu.Unlock()
}

func (t *gotuiTurnUI) AppendToolStart(calls []messages.ChatMessageToolCall) {
	t.repl.model.mu.Lock()
	defer t.repl.model.mu.Unlock()
	if !t.acceptingLocked() {
		return
	}
	if len(calls) > 0 {
		t.repl.model.turnHasOutput = true
		// Tools pause the thinking clock without closing the record; an
		// unbroken continuation resumes the same indicator.
		t.repl.model.pauseThinkingSegment()
		t.repl.model.finishAssistantBlock("")
		t.repl.model.runningTools += len(calls)
		t.repl.model.state = turnStateTool
		t.repl.model.toolName = calls[0].Name
	}
	if !toolDisplayEnabled(t.config) {
		return
	}
	var record *toolDisclosureRecord
	for _, c := range calls {
		record = t.repl.model.appendToolStartRow(c.ID, toolLabel(c))
	}
	t.repl.model.refreshToolDisclosure(record)
}

func (t *gotuiTurnUI) ApproveToolCalls(calls []messages.ChatMessageToolCall) []bool {
	if len(calls) == 0 {
		return nil
	}
	t.repl.model.mu.Lock()
	if !t.activeLocked() || t.repl.model.canceling {
		t.repl.model.mu.Unlock()
		return denyToolCalls(calls)
	}
	t.repl.model.mu.Unlock()

	if !t.config.Confirm {
		approved := make([]bool, len(calls))
		for i := range approved {
			approved[i] = true
		}
		return approved
	}
	reply := make(chan []bool, 1)
	t.repl.model.mu.Lock()
	if !t.activeLocked() || t.repl.model.canceling {
		t.repl.model.mu.Unlock()
		return denyToolCalls(calls)
	}
	t.repl.model.approval = &approvalState{calls: calls, reply: reply}
	label := toolLabel(calls[0])
	if len(calls) > 1 {
		label += fmt.Sprintf(" +%d more", len(calls)-1)
	}
	t.repl.model.pushNotice("approval needed: " + truncate(label, 80))
	t.repl.model.mu.Unlock()
	results, ok := <-reply
	if !ok {
		return make([]bool, len(calls))
	}
	return results
}

func (t *gotuiTurnUI) AppendToolEnd(call messages.ChatMessageToolCall, result string, duration time.Duration, err error) {
	label := stripTranscriptImageMarkers(toolLabel(call))
	denied := toolWasDenied(result)
	var discoveredImages []transcriptImage
	if toolDisplayEnabled(t.config) && !denied {
		// Tool output can be large. Discovery touches only Markdown/path syntax
		// and the filesystem, so keep it outside the model lock and let the TUI
		// continue painting while it runs.
		discoveredImages = discoverToolOutputImages(result, t.repl.model.imageBaseDir)
	}
	t.repl.model.mu.Lock()
	defer t.repl.model.mu.Unlock()
	m := t.repl.model
	if !t.acceptingLocked() {
		return
	}
	if m.runningTools > 0 {
		m.runningTools--
	}
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
	var final string
	switch {
	case denied:
		final = toolDeniedLine(label)
	case err != nil:
		final = toolErrorLine(label, formatElapsed(duration), toolFailureMeta(err))
	default:
		final = toolOKLine(label, formatElapsed(duration), resultLineMeta(result))
	}
	final = stripTranscriptImageMarkers(final)
	images := discoveredImages
	// Freeze the final line over its running disclosure row. Fall back to a new
	// row if the display was cleared while the tool was in flight.
	record := m.currentToolDisclosure()
	if rowIndex, ok := m.takeActiveTool(call.ID); ok && record != nil && rowIndex >= 0 && rowIndex < len(record.rows) {
		row := &record.rows[rowIndex]
		row.line = final
		row.images = append([]transcriptImage(nil), images...)
		row.settled = true
	} else {
		record = m.ensureToolDisclosure()
		record.rows = append(record.rows, toolDisclosureRow{
			callID:  call.ID,
			label:   label,
			line:    final,
			images:  append([]transcriptImage(nil), images...),
			settled: true,
		})
	}
	m.refreshToolDisclosure(record)
	// The disclosure stays live past the batch: an unbroken continuation's
	// next batch folds into it. Assistant prose or turn settlement closes it.
}

func (t *gotuiTurnUI) AppendToolMedia(call messages.ChatMessageToolCall, images []transcriptImage) {
	if len(images) == 0 || (t.config != nil && t.config.Quiet) {
		return
	}
	t.repl.model.mu.Lock()
	defer t.repl.model.mu.Unlock()
	m := t.repl.model
	if !t.acceptingLocked() {
		return
	}
	record, row := m.toolDisclosureRowForCall(call.ID)
	if row == nil {
		record = m.ensureToolDisclosure()
		record.rows = append(record.rows, toolDisclosureRow{
			callID:  call.ID,
			label:   stripTranscriptImageMarkers(toolLabel(call)),
			line:    toolOKLine(toolLabel(call), "", ""),
			settled: true,
		})
		row = &record.rows[len(record.rows)-1]
	}
	m.mutateAnchored(m.disclosureLayoutWidth(0), matchToolGroup([]int64{record.id}), func(bool) {
		row.inspectionImages = append([]transcriptImage(nil), images...)
		m.refreshToolDisclosureWithAnchor(record, false)
		// The third Images field and its gallery are derived from
		// inspectionImages; neither necessarily changes the canonical raw tool
		// text.
		m.visual.invalidate()
	})
}

func (t *gotuiTurnUI) AppendWarning(text string) {
	t.repl.model.mu.Lock()
	if !t.acceptingLocked() {
		t.repl.model.mu.Unlock()
		return
	}
	t.repl.model.appendNoticeLine("Warning: " + text)
	t.repl.model.turnHasOutput = true
	t.repl.model.mu.Unlock()
}

func (t *gotuiTurnUI) RecordTurnTokens(in, out int) {
	t.repl.model.mu.Lock()
	if !t.acceptingLocked() {
		t.repl.model.mu.Unlock()
		return
	}
	t.repl.model.lastIn = in
	t.repl.model.lastOut = out
	t.repl.model.turnDock.inputTokens = in
	t.repl.model.turnDock.outputTokens = out
	t.repl.model.mu.Unlock()
}

func (t *gotuiTurnUI) RecordContextUsage(used, limit int, estimated bool) {
	t.repl.model.mu.Lock()
	if t.acceptingLocked() {
		t.repl.model.status.recordContextUsage(used, limit, estimated)
	}
	t.repl.model.mu.Unlock()
}

func (t *gotuiTurnUI) FinishTextTurn() {
	t.repl.model.mu.Lock()
	if t.acceptingLocked() {
		t.repl.model.finishAssistantBlock("")
	}
	t.repl.model.mu.Unlock()
}
