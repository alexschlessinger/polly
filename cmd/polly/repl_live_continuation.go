package main

import "github.com/alexschlessinger/pollytool/messages"

// prepareLiveDisplay appends a bounded, uncommitted observation to an immutable
// saved display. The caller has already paired the snapshots by session identity
// and checkpoint sequence. This model is never input to an agent or a writer.
func prepareLiveDisplay(saved *replModel, tail []messages.ChatMessage) *replModel {
	m := childDisplayCopy(saved)
	m.artifactStore = saved.artifactStore
	h := historyHydrator{m: m}
	if saved.historyContinuation != nil {
		h = *saved.historyContinuation
		h.m = m
		for len(m.transcript) > saved.historyContinuationAt {
			m.deleteTranscriptEntry(len(m.transcript) - 1)
		}
		tool := func(record *toolDisclosureRecord) *toolDisclosureRecord {
			if record == nil {
				return nil
			}
			return m.toolDisclosures.get(record.id)
		}
		thought := func(record *reasoningRecord) *reasoningRecord {
			if record == nil {
				return nil
			}
			return m.reasoningRecords.get(record.id)
		}
		h.tools = tool(h.tools)
		h.reasoning, h.dockReasoning = thought(h.reasoning), thought(h.dockReasoning)
		h.toolGroups = make([]*toolDisclosureRecord, len(saved.historyContinuation.toolGroups))
		for i, record := range saved.historyContinuation.toolGroups {
			h.toolGroups[i] = tool(record)
		}
	}
	m.restoredDraft, m.restoredPersistence = nil, nil
	m.ed.setText("")
	m.clearTurnDock()
	for _, msg := range tail {
		m.rememberArtifactAttachments(msg)
		h.replay(msg)
	}
	h.flushTools()
	// Preserve a reasoning run across the checkpoint, rather than allocating a
	// new positional thought key merely because part of it became durable.
	m.historyContinuation = saved.historyContinuation
	m.projectDisplayCatalog(tail, saved.displayCatalog.clone(), true)
	m.historyContinuation = nil
	pending := make(map[string]bool)
	for _, tool := range m.displayCatalog.tools {
		if !tool.complete {
			pending[tool.key] = true
		}
	}
	for _, record := range m.toolDisclosures.all() {
		changed := false
		for i := range record.rows {
			row := &record.rows[i]
			if pending[row.sectionKey] && !row.settled {
				row.setPresentation(toolPresentation{outcome: toolOutcomeRunning})
				record.complete = false
				changed = true
			}
		}
		if changed {
			m.refreshToolDisclosureWithAnchor(record, false)
		}
	}
	return m
}
