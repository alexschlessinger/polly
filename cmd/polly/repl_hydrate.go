package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/artifacts"
	"github.com/alexschlessinger/pollytool/images"
	"github.com/alexschlessinger/pollytool/messages"
)

// History hydration: replaying a stored conversation into the transcript on resume.

const resumedTurnLimit = 5

// hydrateHistory replays a stored conversation into the transcript so a
// resumed context is honest about what the model already remembers: the last
// resumedTurnLimit user turns, each with its assistant prose, reasoning, tool
// exchanges folded into compact activity rows, and trailer, then the composer
// restore for an unanswered final prompt.
func (m *replModel) hydrateHistory(history []messages.ChatMessage, contextName string) {
	m.clearTurnDock()
	for _, msg := range history {
		m.rememberArtifactAttachments(msg)
	}
	start, totalTurns, showTurns := resumedHistoryWindow(history)
	if totalTurns == 0 {
		return
	}
	m.appendNoticeLine(resumedNotice(contextName, totalTurns, showTurns))
	h := historyHydrator{m: m}
	for _, msg := range history[start:] {
		h.replay(msg)
	}
	h.finish()
	m.followBottom = true
}

// resumedHistoryWindow counts the real user turns in history and returns the
// index of the first message to replay so that at most resumedTurnLimit of
// them show.
func resumedHistoryWindow(history []messages.ChatMessage) (start, totalTurns, showTurns int) {
	for _, msg := range history {
		if msg.Role == messages.MessageRoleUser && !agentSyntheticMessage(msg) {
			totalTurns++
		}
	}
	showTurns = min(totalTurns, resumedTurnLimit)
	skip := totalTurns - showTurns
	seen := 0
	for i, msg := range history {
		if msg.Role != messages.MessageRoleUser || agentSyntheticMessage(msg) {
			continue
		}
		if seen == skip {
			return i, totalTurns, showTurns
		}
		seen++
	}
	return 0, totalTurns, showTurns
}

func resumedNotice(contextName string, totalTurns, showTurns int) string {
	name := contextName
	if name == "" {
		name = "context"
	}
	if totalTurns > showTurns {
		return fmt.Sprintf("resumed %s · showing last %d of %d turns", name, showTurns, totalTurns)
	}
	turnWord := "turns"
	if totalTurns == 1 {
		turnWord = "turn"
	}
	return fmt.Sprintf("resumed %s · %d %s", name, totalTurns, turnWord)
}

// historyHydrator replays stored messages one at a time, carrying the state
// the role cases share: tool rows waiting for their disclosure, the open
// reasoning record, the turn's token totals, and what the last message was so
// the final user prompt can be settled or restored.
type historyHydrator struct {
	m *replModel

	toolRows   []toolDisclosureRow     // rows not yet folded into the turn's disclosure
	tools      *toolDisclosureRecord   // the turn's disclosure, once one exists
	toolGroups []*toolDisclosureRecord // prose-separated activity in this turn
	reasoning  *reasoningRecord
	turnInput  int
	turnOutput int

	lastRole            string
	lastUser            messages.ChatMessage // the newest user message, for the composer restore
	lastUserContent     string
	lastUserRestorable  bool
	lastUserContextOnly bool
}

func (h *historyHydrator) replay(msg messages.ChatMessage) {
	switch msg.Role {
	case messages.MessageRoleUser:
		h.user(msg)
	case messages.MessageRoleAssistant:
		h.assistant(msg)
	case messages.MessageRoleTool:
		h.tool(msg)
	case messages.MessageRoleInternal:
		h.internal(msg)
	}
}

func (h *historyHydrator) user(msg messages.ChatMessage) {
	if agentSyntheticMessage(msg) {
		return
	}
	m := h.m
	if h.lastRole != "" && h.lastRole != messages.MessageRoleUser {
		h.finishTurn()
	} else {
		h.flushTools()
		m.clearTurnDock()
	}
	h.tools = nil
	h.toolGroups = nil
	h.reasoning = nil
	h.turnInput, h.turnOutput = 0, 0
	m.appendTurnSeparator()
	content, restorable, contextOnly := historyUserSummary(msg)
	m.appendUserPrompt(content)
	// Only the final prompt can be restored, so finish builds the turn once
	// from whichever user message ends up last.
	h.lastUser, h.lastUserContent, h.lastUserRestorable = msg, content, restorable
	h.lastUserContextOnly = contextOnly
	h.lastRole = msg.Role
}

func (h *historyHydrator) assistant(msg messages.ChatMessage) {
	m := h.m
	h.flushTools()
	h.appendReasoning(msg.Reasoning, msg.ThinkingDuration())
	if tokens := msg.GetInputTokens(); tokens > h.turnInput {
		h.turnInput = tokens
	}
	h.turnOutput += msg.GetOutputTokens()
	if content := msg.GetContent(); content != "" {
		m.appendAssistant(content)
		m.finishAssistantBlock("")
		h.tools = nil
	}
	for _, call := range msg.ToolCalls {
		// The stored call keeps its arguments, so the row reads like it did
		// live: the tool name plus its argument summary.
		row := toolDisclosureRow{callID: call.ID, label: toolLabel(call)}
		row.setCall(call)
		if row.agent != nil {
			row.agent.active, row.agent.status = false, "unknown"
		}
		h.toolRows = append(h.toolRows, row)
	}
	h.lastRole = msg.Role
}

// tool settles the row a result belongs to: the row with its call ID, else
// the oldest row still waiting.
func (h *historyHydrator) tool(msg messages.ChatMessage) {
	inspectionImages := inspectionTranscriptImages(msg, h.m.artifactStore)
	if len(h.toolRows) == 0 {
		row := toolDisclosureRow{callID: msg.ToolCallID, label: toolDisplayName(msg.ToolName)}
		row.setCall(messages.ChatMessageToolCall{ID: msg.ToolCallID, Name: msg.ToolName})
		// Without the original arguments, success only proves launch success.
		if row.agent != nil {
			row.agent.background = true
		}
		h.toolRows = append(h.toolRows, row)
	}
	pick := -1
	for i := range h.toolRows {
		if h.toolRows[i].settled {
			continue
		}
		if h.toolRows[i].callID == msg.ToolCallID {
			pick = i
			break
		}
		if pick < 0 {
			pick = i
		}
	}
	if pick >= 0 {
		h.toolRows[pick].hydrateAgentResult(msg)
		h.toolRows[pick].line = hydratedToolLine(h.toolRows[pick].label, msg)
		h.toolRows[pick].inspectionImages = inspectionImages
		h.toolRows[pick].settled = true
	}
	h.lastRole = msg.Role
}

// internal applies a durable turn marker: the safe display metadata for the
// turn's reasoning and tool order, and the status that settles the turn.
func (h *historyHydrator) internal(msg messages.ChatMessage) {
	m := h.m
	h.flushTools()
	displayToolCalls := decodeDisplayToolCalls(msg.Metadata[messages.MetadataKeyDisplayToolCalls])
	if displayReasoning, _ := msg.Metadata[messages.MetadataKeyDisplayReasoning].(string); displayReasoning != "" {
		h.appendReasoning(displayReasoning, msg.ThinkingDuration())
	}
	h.applyToolOrder(displayToolCalls)
	status, _ := msg.Metadata[messages.MetadataKeyTurnStatus].(string)
	switch {
	case status == messages.TurnStatusToolDenied:
		if len(displayToolCalls) == 0 {
			// Compatibility with sessions written before safe tool display
			// metadata existed.
			m.appendLine("  " + styled("✗", "err", "bold") + " " + styled("tool request denied", "muted", ""))
		}
		// A durable internal completion marker settles the preceding user
		// turn without becoming model-visible assistant content.
		h.lastRole = messages.MessageRoleAssistant
	case status == messages.TurnStatusInterrupted:
		// Everything before the marker is durable completed work; the
		// turn ended early without a final response. Settle it so the
		// preceding user message isn't restored as an unsent draft.
		m.appendLine("  " + styled("turn interrupted · completed work retained", "muted", ""))
		h.lastRole = messages.MessageRoleAssistant
	case len(displayToolCalls) > 0:
		h.lastRole = messages.MessageRoleAssistant
	}
}

// flushTools folds the pending rows into the turn's disclosure, opening one
// on first use.
func (h *historyHydrator) flushTools() {
	if len(h.toolRows) == 0 {
		return
	}
	if h.tools == nil {
		h.tools = h.m.appendCompletedToolDisclosure(h.toolRows)
		h.toolGroups = append(h.toolGroups, h.tools)
	} else {
		for i := range h.toolRows {
			if h.toolRows[i].line == "" {
				h.toolRows[i].line = pendingToolLine(h.toolRows[i].label)
			}
		}
		h.tools.rows = append(h.tools.rows, h.toolRows...)
		h.m.refreshToolDisclosure(h.tools)
	}
	h.toolRows = nil
}

// applyToolOrder reorders the turn's disclosure to the durable display
// order: each recorded call takes the existing row with its ID, else the
// first unused row with its name, else a fresh row; a denied call shows as
// denied whatever its row held. Rows the record does not mention keep their
// place at the end.
func (h *historyHydrator) applyToolOrder(order []durableDisplayToolCall) {
	if len(order) == 0 {
		return
	}
	// Safe display markers cover a whole turn. Apply known calls at their
	// original prose-separated group. Stripped denied calls have no transcript
	// position; keep them with the latest batch, as legacy hydration did.
	if len(h.toolGroups) > 0 {
		orders := make(map[*toolDisclosureRecord][]durableDisplayToolCall)
		used := make(map[*toolDisclosureRow]bool)
		for _, call := range order {
			target := h.tools
			if target == nil {
				target = h.toolGroups[len(h.toolGroups)-1]
			}
		search:
			for _, group := range h.toolGroups {
				for i := range group.rows {
					row := &group.rows[i]
					if !used[row] && ((call.ID != "" && row.callID == call.ID) ||
						((call.ID == "" || row.callID == "") && row.label == toolDisplayName(call.Name))) {
						target = group
						used[row] = true
						break search
					}
				}
			}
			orders[target] = append(orders[target], call)
		}
		for _, group := range h.toolGroups {
			localHydrator := historyHydrator{m: h.m, tools: group}
			localHydrator.applyToolOrder(orders[group])
		}
		if missing := orders[nil]; len(missing) > 0 {
			localHydrator := historyHydrator{m: h.m}
			localHydrator.applyToolOrder(missing)
			h.tools = localHydrator.tools
			h.toolGroups = append(h.toolGroups, h.tools)
		}
		return
	}
	var existing []toolDisclosureRow
	if h.tools != nil {
		existing = h.tools.rows
	}
	used := make([]bool, len(existing))
	ordered := make([]toolDisclosureRow, 0, max(len(order), len(existing)))
	for _, displayCall := range order {
		name := toolDisplayName(displayCall.Name)
		pick := -1
		for i := range existing {
			if !used[i] && displayCall.ID != "" && existing[i].callID == displayCall.ID {
				pick = i
				break
			}
		}
		if pick < 0 {
			for i := range existing {
				if !used[i] && existing[i].label == name {
					pick = i
					break
				}
			}
		}
		row := toolDisclosureRow{callID: displayCall.ID, label: name, settled: true}
		if pick >= 0 {
			used[pick] = true
			row = existing[pick]
		}
		row.setCall(messages.ChatMessageToolCall{ID: displayCall.ID, Name: displayCall.Name})
		if pick < 0 && row.agent != nil {
			row.agent.active, row.agent.status = false, "unknown"
		}
		if displayCall.Denied {
			row.finishAgentCall(messages.ChatMessageToolCall{}, true, nil)
			row.line = toolDeniedLine(name)
			row.images = nil
			row.settled = true
		} else if row.line == "" {
			row.line = pendingToolLine(name)
		}
		ordered = append(ordered, row)
	}
	for i, row := range existing {
		if !used[i] {
			ordered = append(ordered, row)
		}
	}
	if h.tools == nil {
		h.tools = h.m.appendCompletedToolDisclosure(ordered)
		h.toolGroups = append(h.toolGroups, h.tools)
		return
	}
	h.tools.rows = ordered
	h.tools.complete = true
	h.tools.expanded = false
	h.m.refreshToolDisclosure(h.tools)
}

// appendReasoning adds a stored reasoning segment to the turn's record. The
// elapsed time accumulates the way the live clock did: one record sums every
// segment it absorbed.
func (h *historyHydrator) appendReasoning(text string, elapsed time.Duration) {
	if strings.TrimSpace(text) == "" {
		return
	}
	if h.reasoning == nil {
		h.reasoning = h.m.newReasoningRecord(true)
	}
	h.m.appendReasoningTail(h.reasoning, text, len(h.reasoning.tail) > 0)
	h.reasoning.elapsed += elapsed
	h.m.refreshReasoningRecord(h.reasoning, 80)
}

func (h *historyHydrator) finishTurn() {
	h.flushTools()
	h.m.setHydratedTurnDock(h.reasoning, h.tools, h.turnInput, h.turnOutput)
	if len(h.toolGroups) > 0 {
		h.m.turnDock.toolIDs = nil
		for _, group := range h.toolGroups {
			h.m.turnDock.toolIDs = append(h.m.turnDock.toolIDs, group.id)
		}
	}
	h.m.attachTurnDockTrailer()
}

// finish settles the trailing turn and, when the conversation ends on an
// unanswered prompt, restores that prompt to the composer.
func (h *historyHydrator) finish() {
	m := h.m
	if h.lastRole != "" && h.lastRole != messages.MessageRoleUser {
		h.finishTurn()
	} else {
		h.flushTools()
	}
	if h.lastRole == messages.MessageRoleUser && !h.lastUserContextOnly {
		if turn, ok := restorableHistoryTurn(h.lastUser, h.lastUserContent, h.lastUserRestorable, m.artifactStore); ok {
			m.restoreTurnDraft(turn, newTurnPersistenceAck(true))
			m.appendLine("  " + styled("incomplete · restored to composer", "muted", ""))
		} else {
			m.appendLine("  " + styled("incomplete", "muted", ""))
		}
	}
}

func agentSyntheticMessage(msg messages.ChatMessage) bool {
	synthetic, _ := msg.Metadata[messages.MetadataKeyAgentSynthetic].(bool)
	return synthetic
}

func restorableHistoryTurn(msg messages.ChatMessage, display string, simpleContent bool, store artifacts.Store) (managedTurnInput, bool) {
	contextOnly, _ := msg.Metadata[messages.MetadataKeyContextImport].(bool)
	if contextOnly || msg.Role != messages.MessageRoleUser {
		return managedTurnInput{}, false
	}
	if simpleContent {
		return cloneManagedTurn(managedTurnInput{displayText: display, userMessage: msg}), true
	}
	imageCount := 0
	for _, part := range msg.Parts {
		switch part.Type {
		case "text":
			if part.FileName != "" {
				return managedTurnInput{}, false
			}
		case "image_base64":
			// A normalized upgrade is portable by construction (NormalizeForModel
			// bounds the edge and the bytes), so only its kind needs checking.
			if upgraded, err := portableImagePart(part); err != nil || upgraded.Type != "image_base64" {
				return managedTurnInput{}, false
			}
			imageCount++
		case "image_artifact":
			if !availableImageArtifact(store, part.Artifact) {
				return managedTurnInput{}, false
			}
			imageCount++
		default:
			return managedTurnInput{}, false
		}
		if imageCount > maxPromptAttachments {
			return managedTurnInput{}, false
		}
	}
	if imageCount == 0 {
		return managedTurnInput{}, false
	}
	return cloneManagedTurn(managedTurnInput{displayText: display, userMessage: msg}), true
}

func portablePersistedImagePart(part messages.ContentPart) bool {
	return validatePortablePersistedImagePart(part) == nil
}

func validatePortablePersistedImagePart(part messages.ContentPart) error {
	if len(part.ImageData) > maxPortableEncodedImageBytes {
		return fmt.Errorf("encoded image uses %d bytes; per-image portable limit is 10,000,000 bytes (10 MB)", len(part.ImageData))
	}
	data, err := base64.StdEncoding.DecodeString(part.ImageData)
	if err != nil || len(data) == 0 {
		return fmt.Errorf("invalid or empty base64 data")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return fmt.Errorf("invalid raster image data")
	}
	if max(config.Width, config.Height) > uploadMaxLongEdge ||
		int64(config.Width)*int64(config.Height) > maxLocalImagePixels {
		return fmt.Errorf("image dimensions %dx%d exceed the prepared-image bounds", config.Width, config.Height)
	}
	if _, decodedFormat, err := image.Decode(bytes.NewReader(data)); err != nil || decodedFormat != format {
		return fmt.Errorf("invalid %s image data", format)
	}
	wantMIME, ok := images.PortableMIMEType(format)
	if !ok {
		return fmt.Errorf("unsupported image format %q", format)
	}
	if part.MimeType != wantMIME {
		return fmt.Errorf("image MIME %q does not match %q bytes", part.MimeType, format)
	}
	return nil
}

func historyUserSummary(msg messages.ChatMessage) (display string, restorable, contextOnly bool) {
	display = msg.Content
	contextOnly, _ = msg.Metadata[messages.MetadataKeyContextImport].(bool)
	if contextOnly && len(msg.Parts) == 0 {
		return "[context added]", false, true
	}
	var attachments []string
	if display == "" {
		for _, part := range msg.Parts {
			if part.Type == "text" && part.FileName == "" {
				display += part.Text
			}
		}
	}
	for _, part := range msg.Parts {
		if part.FileName != "" {
			attachments = append(attachments, part.FileName)
		} else if part.Type != "text" {
			attachments = append(attachments, "attachment")
		}
	}
	if len(attachments) > 0 {
		label := compactToolNames(attachments)
		if display == "" {
			display = "[attached: " + label + "]"
		} else {
			display += " [attached: " + label + "]"
		}
	}
	if display == "" {
		display = "[empty message]"
	}
	// This summary only identifies simple Content drafts. restorableHistoryTurn
	// separately recognizes persisted image_base64 parts while rejecting text
	// file bodies and context imports that cannot be reconstructed safely.
	restorable = len(msg.Parts) == 0 && msg.Content != ""
	return display, restorable, contextOnly
}

func compactToolNames(names []string) string {
	const visible = 3
	shown := names
	if len(shown) > visible {
		shown = shown[:visible]
	}
	text := strings.Join(shown, ", ")
	if len(names) > visible {
		text += fmt.Sprintf(" +%d", len(names)-visible)
	}
	return truncate(text, 120)
}
