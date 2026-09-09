package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/messages"
)

// Inspection data is distinct from the compact, bounded inline disclosures.
// It never enters provider replay. All access is covered by replModel.mu.
type inspectionSource struct {
	tools    []inspectedTool
	thoughts []inspectedThought
	version  uint64
	epoch    uint64 // changes when saved history replaces a live catalogue
}

type inspectedTool struct {
	key                 string
	call                messages.ChatMessageToolCall
	result              messages.ChatMessage
	available, complete bool
	status              string
	started             time.Time
	duration            time.Duration
	version             uint64
}

type inspectedThought struct {
	key, text string
	writer    *strings.Builder
	complete  bool
	version   uint64
}

func (s *inspectionSource) startTool(call messages.ChatMessageToolCall) string {
	key := fmt.Sprintf("tool:%d:%s", len(s.tools)+1, call.ID)
	s.tools = append(s.tools, inspectedTool{key: key, call: call, status: "running", started: time.Now(), version: 1})
	s.version++
	return key
}

func (s *inspectionSource) toolForCall(id string) *inspectedTool {
	for i := len(s.tools) - 1; i >= 0; i-- {
		if s.tools[i].call.ID == id {
			return &s.tools[i]
		}
	}
	return nil
}

func (s *inspectionSource) finishTool(call messages.ChatMessageToolCall, result string, duration time.Duration, err error) {
	t := s.toolForCall(call.ID)
	if t == nil {
		s.startTool(call)
		t = &s.tools[len(s.tools)-1]
	}
	t.complete, t.available, t.duration = true, true, duration
	t.result = messages.ChatMessage{Role: messages.MessageRoleTool, ToolCallID: call.ID, ToolName: call.Name, Content: result}
	t.status = "completed"
	if toolWasDenied(result) {
		t.status = "denied"
	} else if err != nil {
		t.status = "failed"
	}
	t.version++
	s.version++
}

func (s *inspectionSource) setResult(call messages.ChatMessageToolCall, result messages.ChatMessage) {
	t := s.toolForCall(call.ID)
	if t == nil {
		s.startTool(call)
		t = &s.tools[len(s.tools)-1]
	}
	t.result, t.available, t.complete = cloneChatMessage(result), true, true
	if t.status == "running" {
		t.status = "completed"
	}
	t.version++
	s.version++
}

func (s *inspectionSource) appendThought(key *string, text string, segmentBreak bool, complete bool) {
	if *key == "" {
		*key = fmt.Sprintf("thought:%d", len(s.thoughts)+1)
		s.thoughts = append(s.thoughts, inspectedThought{key: *key})
	}
	for i := len(s.thoughts) - 1; i >= 0; i-- {
		t := &s.thoughts[i]
		if t.key != *key {
			continue
		}
		if t.writer == nil {
			t.writer = &strings.Builder{}
			t.writer.WriteString(t.text)
		}
		if segmentBreak && t.text != "" && !strings.HasSuffix(t.text, "\n") {
			t.writer.WriteByte('\n')
		}
		t.writer.WriteString(text)
		t.text = t.writer.String()
		t.complete = complete
		t.version++
		s.version++
		return
	}
}

func (s inspectionSource) clone() inspectionSource {
	s.tools = slices.Clone(s.tools)
	for i := range s.tools {
		s.tools[i].result = cloneChatMessage(s.tools[i].result)
	}
	s.thoughts = slices.Clone(s.thoughts)
	for i := range s.thoughts {
		s.thoughts[i].writer = nil
	}
	return s
}

// Headers and navigation need identities and status, not every result body.
// These snapshots remain cheap when an unrelated call or thought changes.
func (s inspectionSource) navigation() inspectionSource {
	n := inspectionSource{version: s.version, epoch: s.epoch}
	for _, t := range s.tools {
		n.tools = append(n.tools, inspectedTool{key: t.key,
			call:     messages.ChatMessageToolCall{ID: t.call.ID, Name: t.call.Name},
			complete: t.complete, status: t.status, started: t.started, duration: t.duration})
	}
	for _, t := range s.thoughts {
		n.thoughts = append(n.thoughts, inspectedThought{key: t.key, complete: t.complete})
	}
	return n
}

// Conservative accounting for these body-free navigation records, including
// spare slice capacity. Updating a counter must not remeasure rendered cells.
func (s inspectionSource) navigationBytes() int64 {
	n := int64(len(s.tools)+len(s.thoughts)) * 2048
	for _, t := range s.tools {
		n += int64(len(t.key) + len(t.call.ID) + len(t.call.Name) + len(t.status))
	}
	for _, t := range s.thoughts {
		n += int64(len(t.key))
	}
	return n
}

func (v *viewInstance) setNavigation(source inspectionSource) {
	next := source.navigation()
	v.bytes += next.navigationBytes() - v.model.inspections.navigationBytes()
	v.model.inspections = next
}

func (s inspectionSource) selected(target viewTarget) (tool *inspectedTool, thought *inspectedThought) {
	if target.kind == toolViewKind {
		for n := range s.tools {
			if s.tools[n].key == target.item {
				return &s.tools[n], nil
			}
		}
	} else if target.kind == thoughtViewKind {
		for n := range s.thoughts {
			if s.thoughts[n].key == target.item {
				return nil, &s.thoughts[n]
			}
		}
	}
	return nil, nil
}

// Saved histories have no live per-item version counter. Hash only the
// selected payload on the worker, so unrelated saved turns do not reformat it.
func (s viewSource) itemRevision() string {
	h := sha256.New()
	if s.tool != nil {
		t := s.tool
		fmt.Fprintf(h, "%t:%t:%s:%d:", t.available, t.complete, t.status, t.duration)
		_ = json.NewEncoder(h).Encode(t.call)
		_ = json.NewEncoder(h).Encode(t.result)
	} else if s.thought != nil {
		fmt.Fprint(h, s.thought.text)
	}
	return fmt.Sprintf("item:%x", h.Sum(nil))
}

// hydrateInspections covers the whole saved conversation, independently of
// the transcript's five-turn display window. Missing legacy bodies stay absent.
func (m *replModel) hydrateInspections(history []messages.ChatMessage) {
	source := inspectionSource{epoch: m.inspections.epoch + 1}
	thoughtKey := ""
	turnTools := 0
	for _, msg := range history {
		switch msg.Role {
		case messages.MessageRoleUser:
			if !agentSyntheticMessage(msg) {
				thoughtKey = ""
				turnTools = len(source.tools)
			}
		case messages.MessageRoleAssistant:
			if msg.Reasoning != "" {
				source.appendThought(&thoughtKey, msg.Reasoning, true, true)
			}
			if msg.GetContent() != "" {
				thoughtKey = ""
			}
			for _, call := range msg.ToolCalls {
				source.startTool(call)
				t := &source.tools[len(source.tools)-1]
				t.complete, t.status, t.started = true, "output unavailable", time.Time{}
			}
		case messages.MessageRoleTool:
			t := source.toolForCall(msg.ToolCallID)
			if t == nil {
				source.startTool(messages.ChatMessageToolCall{ID: msg.ToolCallID, Name: msg.ToolName})
				t = &source.tools[len(source.tools)-1]
			}
			source.setResult(t.call, msg)
			t.status = "completed"
			if toolWasDenied(msg.GetContent()) {
				t.status = "denied"
			} else if success, known := msg.ToolSucceeded(); known && !success {
				t.status = "failed"
			}
			t.duration = msg.ToolDuration()
		case messages.MessageRoleInternal:
			if thought, _ := msg.Metadata[messages.MetadataKeyDisplayReasoning].(string); thought != "" {
				source.appendThought(&thoughtKey, thought, true, true)
			}
			// Safe turn markers also retain launches denied before execution.
			order := decodeDisplayToolCalls(msg.Metadata[messages.MetadataKeyDisplayToolCalls])
			if len(order) == 0 {
				continue
			}
			existing := slices.Clone(source.tools[turnTools:])
			ordered := make([]inspectedTool, 0, len(existing)+len(order))
			used := make([]bool, len(existing))
			for _, call := range order {
				pick := -1
				for i, t := range existing {
					if !used[i] && call.ID != "" && t.call.ID == call.ID {
						pick = i
						break
					}
				}
				t := inspectedTool{call: messages.ChatMessageToolCall{ID: call.ID, Name: call.Name}, complete: true, status: "output unavailable", version: 1}
				if pick >= 0 {
					t = existing[pick]
					used[pick] = true
				}
				if call.Denied {
					t.status = "denied"
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
		source.tools[i].key = fmt.Sprintf("tool:%d:%s", i+1, source.tools[i].call.ID)
	}
	m.inspections = source
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
				if source.tools[k].call.ID == row.callID && source.tools[k].call.Name == row.toolName {
					row.inspectionKey = source.tools[k].key
					cursor = k - 1
					break
				}
			}
		}
	}
	cursor = len(source.thoughts) - 1
	for i := len(m.reasoningOrder) - 1; i >= 0 && cursor >= 0; i-- {
		if record := m.reasoningRecords.get(m.reasoningOrder[i]); record != nil {
			record.inspectionKey = source.thoughts[cursor].key
			cursor--
		}
	}
}
