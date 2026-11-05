package main

import (
	"slices"

	"github.com/alexschlessinger/pollytool/cmd/polly/internal/style"
	"github.com/alexschlessinger/pollytool/llm"
)

// Reasoning can coalesce across successful tool rounds. Snapshot that accepted
// prefix rather than labeling the entire shared disclosure as discarded.
type tuiModelAttempt struct {
	output bool
	prior  *reasoningRecord
	shared reasoningRecord
	owned  []int64
}

func (*gotuiTurnUI) SupportsAttemptRecovery() bool { return true }

func (t *gotuiTurnUI) ModelAttemptStarted(_, _ int) {
	t.model.mu.Lock()
	defer t.model.mu.Unlock()
	if !t.acceptingLocked() {
		return
	}
	m := t.model
	m.pauseThinkingSegment()
	m.beginTurnStep()
	t.attempt = tuiModelAttempt{}
	if record := m.currentReasoningRecord(); record != nil {
		copy := *record
		copy.tail = slices.Clone(record.tail)
		t.attempt.prior = &copy
	}
	m.retry = modelRetryStatus{}
}

func (t *gotuiTurnUI) appendAttemptThinking(chunk string) {
	m := t.model
	segmentBreak := m.thinkingSegmentBreak
	m.appendThinking(chunk)
	if chunk == "" {
		return
	}
	t.attempt.output = true
	record := m.currentReasoningRecord()
	if record == nil {
		return
	}
	if prior := t.attempt.prior; prior != nil && prior.id == record.id {
		appendReasoningTail(&t.attempt.shared, chunk, segmentBreak)
	} else if !slices.Contains(t.attempt.owned, record.id) {
		t.attempt.owned = append(t.attempt.owned, record.id)
	}
}

func (t *gotuiTurnUI) ModelAttemptAborted(_, attempt int, _ error) {
	t.model.mu.Lock()
	defer t.model.mu.Unlock()
	if !t.activeLocked() {
		return
	}
	m := t.model
	m.finishAssistantBlock("") // attempt labels must not set outcomeLabeled
	m.finishThinkingSegment()
	for _, id := range t.attempt.owned {
		if record := m.reasoningRecords.get(id); record != nil {
			record.unsaved, record.complete, record.active = true, true, false
			m.markReasoningDirty(record)
		}
	}
	if prior := t.attempt.prior; prior != nil {
		if record := m.reasoningRecords.get(prior.id); record != nil {
			t.attempt.shared.elapsed = max(0, record.elapsed-prior.elapsed)
			// Preserve its current position: peer notices or display operations
			// may have moved entries while the provider was streaming.
			anchor, expanded := record.transcriptAnchor, record.expanded
			*record = *prior
			record.transcriptAnchor, record.expanded = anchor, expanded
			record.complete, record.active = true, false
			m.markReasoningDirty(record)
		}
		if len(t.attempt.shared.tail) > 0 {
			record := m.newReasoningRecord(true)
			m.turnReasoningIDs = append(m.turnReasoningIDs, record.id)
			m.appendReasoningTail(record, string(t.attempt.shared.tail), false)
			record.tailPrefix = t.attempt.shared.tailPrefix
			record.elapsed = t.attempt.shared.elapsed
			record.tailCut = record.tailCut || t.attempt.shared.tailCut
			record.unsaved = true
			m.markReasoningDirty(record)
		}
	}
	if t.attempt.output {
		// Append a boundary; never truncate a suffix containing peer notices,
		// completed tools, or image receipts from accepted earlier rounds.
		m.appendLine("  " + style.Styled(discardedAttemptLabel(attempt), "muted", ""))
		m.turnHasOutput = true
	}
	t.attempt = tuiModelAttempt{}
	m.state, m.toolName = turnStateWaiting, ""
}

func (t *gotuiTurnUI) ModelRetry(event llm.RetryEvent) {
	t.model.mu.Lock()
	defer t.model.mu.Unlock()
	if !t.activeLocked() {
		return
	}
	t.model.retry.update(event)
	if event.Waiting {
		t.model.pauseThinkingSegment()
		t.model.state, t.model.toolName = turnStateWaiting, ""
	}
}
