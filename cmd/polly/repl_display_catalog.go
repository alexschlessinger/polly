package main

import (
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
)

// Section identities preserve inline disclosure state across projections.
// Tool summaries also supply change totals for legacy sessions. Full tool
// results and reasoning text are not retained here. Access is covered by replModel.mu.
type displayCatalog struct {
	tools    []displayTool
	thoughts []displayThought
	version  uint64
	epoch    uint64 // changes when saved history replaces a live catalogue
}

type displayTool struct {
	key                 string
	call                messages.ChatMessageToolCall
	available, complete bool
	pres                toolPresentation
	started             time.Time
	duration            time.Duration
	version             uint64
}

type displayThought struct {
	key      string
	complete bool
	version  uint64
}

func displayToolKey(call messages.ChatMessageToolCall, ordinal int) string {
	if call.DisplayID != "" {
		return "tool:" + call.DisplayID
	}
	return fmt.Sprintf("tool:%d:%s", ordinal, call.ID)
}

func (s *displayCatalog) startTool(call messages.ChatMessageToolCall) string {
	key := displayToolKey(call, len(s.tools)+1)
	s.tools = append(s.tools, displayTool{key: key, call: call, pres: toolPresentation{outcome: toolOutcomeRunning}, started: time.Now(), version: 1})
	s.version++
	return key
}

func (s *displayCatalog) toolForCall(id string) *displayTool {
	for i := len(s.tools) - 1; i >= 0; i-- {
		if s.tools[i].call.ID == id {
			return &s.tools[i]
		}
	}
	return nil
}

// ensureTool is the tool for call, started now when no call started it.
func (s *displayCatalog) ensureTool(call messages.ChatMessageToolCall) *displayTool {
	if call.DisplayID != "" {
		for i := len(s.tools) - 1; i >= 0; i-- {
			if s.tools[i].call.DisplayID == call.DisplayID {
				return &s.tools[i]
			}
		}
	} else if t := s.toolForCall(call.ID); t != nil {
		return t
	}
	s.startTool(call)
	return &s.tools[len(s.tools)-1]
}

func (s *displayCatalog) finishTool(call messages.ChatMessageToolCall, result string, duration time.Duration, err error) {
	t := s.ensureTool(call)
	t.complete, t.available, t.duration = true, true, duration
	t.pres = newToolPresentation(toolPresentationInput{call: call, result: liveToolResult(call, result, err), err: err, duration: duration, complete: true})
	t.version++
	s.version++
}

func (s *displayCatalog) setResult(call messages.ChatMessageToolCall, result messages.ChatMessage) {
	t := s.ensureTool(call)
	t.available, t.complete = true, true
	// The durable result settles a call finishTool never saw; a call that
	// finished keeps its outcome and gains what the result reports changed.
	next := newToolPresentation(toolPresentationInput{call: call, result: result, duration: t.duration, complete: true})
	switch t.pres.outcome {
	case toolOutcomeRunning, toolOutcomeUnknown:
		t.pres = next
	case toolOutcomeOK:
		t.pres.changes, t.pres.counts = next.changes, next.counts
	}
	t.version++
	s.version++
}

func (s *displayCatalog) thoughtKey(key *string) {
	if *key == "" {
		*key = fmt.Sprintf("thought:%d", len(s.thoughts)+1)
		s.thoughts = append(s.thoughts, displayThought{key: *key})
	}
}

func (s *displayCatalog) messageThoughtKey(key *string, displayID string) {
	if *key == "" && displayID != "" {
		*key = "thought:" + displayID
		s.thoughts = append(s.thoughts, displayThought{key: *key})
		return
	}
	s.thoughtKey(key)
}

func (s displayCatalog) clone() displayCatalog {
	s.tools = slices.Clone(s.tools)
	s.thoughts = slices.Clone(s.thoughts)
	return s
}

// Hydrate section identities and change totals from the whole saved conversation.
func (m *replModel) hydrateDisplayCatalog(history []messages.ChatMessage) {
	m.projectDisplayCatalog(history, displayCatalog{epoch: m.displayCatalog.epoch + 1}, false)
}

// projectDisplayCatalog can extend an immutable saved base with an uncommitted
// tail. Only the tail is scanned; old text and results are never retained here.
func (m *replModel) projectDisplayCatalog(history []messages.ChatMessage, source displayCatalog, live bool) {
	thoughtKey := ""
	if live && m.historyContinuation != nil && m.historyContinuation.reasoning != nil {
		thoughtKey = m.historyContinuation.reasoning.sectionKey
	}
	turnTools := len(source.tools)
	for _, msg := range history {
		switch msg.Role {
		case messages.MessageRoleUser:
			if !agentSyntheticMessage(msg) {
				thoughtKey = ""
				turnTools = len(source.tools)
			}
		case messages.MessageRoleAssistant:
			if msg.Reasoning != "" {
				source.messageThoughtKey(&thoughtKey, msg.DisplayID())
			}
			if m.assistantDisplayText(msg) != "" {
				thoughtKey = ""
			}
			for _, call := range msg.ToolCalls {
				source.startTool(call)
				t := &source.tools[len(source.tools)-1]
				if !live {
					t.complete, t.pres, t.started = true, toolPresentation{}, time.Time{}
				}
			}
		case messages.MessageRoleTool:
			t := source.ensureTool(messages.ChatMessageToolCall{ID: msg.ToolCallID, DisplayID: msg.DisplayID(), Name: msg.ToolName})
			source.setResult(t.call, msg)
			t.duration = msg.ToolDuration()
		case messages.MessageRoleInternal:
			if thought, _ := msg.Metadata[messages.MetadataKeyDisplayReasoning].(string); thought != "" {
				source.messageThoughtKey(&thoughtKey, msg.DisplayID())
			}
			// Safe turn markers also retain launches denied before execution.
			order := decodeDisplayToolCalls(msg.Metadata[messages.MetadataKeyDisplayToolCalls])
			if len(order) == 0 {
				continue
			}
			existing := slices.Clone(source.tools[turnTools:])
			ordered := make([]displayTool, 0, len(existing)+len(order))
			used := make([]bool, len(existing))
			for _, call := range order {
				pick := -1
				for i, t := range existing {
					if !used[i] && (call.DisplayID != "" || call.ID != "") && displayToolIdentity(t.call.ID, t.call.DisplayID) == displayToolIdentity(call.ID, call.DisplayID) {
						pick = i
						break
					}
				}
				t := displayTool{call: messages.ChatMessageToolCall{ID: call.ID, DisplayID: call.DisplayID, Name: call.Name}, complete: true, version: 1}
				if pick >= 0 {
					t = existing[pick]
					used[pick] = true
				}
				if call.Denied {
					t.pres = toolPresentation{outcome: toolOutcomeDenied}
				}
				ordered = append(ordered, t)
			}
			for i, t := range existing {
				if !used[i] {
					ordered = append(ordered, t)
				}
			}
			source.tools = append(source.tools[:turnTools], ordered...)
		}
	}
	for i := range source.tools {
		source.tools[i].key = displayToolKey(source.tools[i].call, i+1)
	}
	m.displayCatalog = source
	// Match from the end: visible history is a suffix, and call IDs can recur
	// in different turns. Never identify a call by tool name alone.
	var records []*toolDisclosureRecord
	for _, r := range m.toolDisclosures.all() {
		records = append(records, r)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].transcriptIndex < records[j].transcriptIndex })
	cursor := len(source.tools) - 1
	for i := len(records) - 1; i >= 0; i-- {
		for j := len(records[i].rows) - 1; j >= 0; j-- {
			row := &records[i].rows[j]
			for k := cursor; k >= 0; k-- {
				if displayToolIdentity(source.tools[k].call.ID, source.tools[k].call.DisplayID) == displayToolIdentity(row.callID, row.displayID) && source.tools[k].call.Name == row.toolName {
					row.sectionKey = source.tools[k].key
					cursor = k - 1
					break
				}
			}
		}
		m.refreshToolDisclosureWithAnchor(records[i], false)
	}
	cursor = len(source.thoughts) - 1
	for i := len(m.reasoningOrder) - 1; i >= 0 && cursor >= 0; i-- {
		if record := m.reasoningRecords.get(m.reasoningOrder[i]); record != nil {
			record.sectionKey = source.thoughts[cursor].key
			cursor--
		}
	}
}
