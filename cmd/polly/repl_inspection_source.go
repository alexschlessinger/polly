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
	pres                toolPresentation
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

// status is the word the header and the list show for the call's state.
func (t inspectedTool) status() string {
	switch {
	case !t.complete:
		return "running"
	case t.pres.outcome == toolOutcomeUnknown && !t.available:
		return "output unavailable"
	case t.pres.outcome == toolOutcomeOK || t.pres.outcome == toolOutcomeUnknown:
		return "completed"
	}
	return string(t.pres.outcome)
}

func (s *inspectionSource) startTool(call messages.ChatMessageToolCall) string {
	key := fmt.Sprintf("tool:%d:%s", len(s.tools)+1, call.ID)
	s.tools = append(s.tools, inspectedTool{key: key, call: call, pres: toolPresentation{outcome: toolOutcomeRunning}, started: time.Now(), version: 1})
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

// ensureTool is the tool for call, started now when no call started it.
func (s *inspectionSource) ensureTool(call messages.ChatMessageToolCall) *inspectedTool {
	if t := s.toolForCall(call.ID); t != nil {
		return t
	}
	s.startTool(call)
	return &s.tools[len(s.tools)-1]
}

func (s *inspectionSource) finishTool(call messages.ChatMessageToolCall, result string, duration time.Duration, err error) {
	t := s.ensureTool(call)
	t.complete, t.available, t.duration = true, true, duration
	t.result = liveToolResult(call, result, err)
	t.pres = newToolPresentation(toolPresentationInput{call: call, result: t.result, err: err, duration: duration, complete: true})
	t.version++
	s.version++
}

func (s *inspectionSource) setResult(call messages.ChatMessageToolCall, result messages.ChatMessage) {
	t := s.ensureTool(call)
	t.result, t.available, t.complete = result.Clone(), true, true
	// The durable result settles a call finishTool never saw; a call that
	// finished keeps its outcome and gains what the result reports changed.
	next := newToolPresentation(toolPresentationInput{call: call, result: t.result, duration: t.duration, complete: true})
	switch t.pres.outcome {
	case toolOutcomeRunning, toolOutcomeUnknown:
		t.pres = next
	case toolOutcomeOK:
		t.pres.changes, t.pres.counts = next.changes, next.counts
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
		s.tools[i].result = s.tools[i].result.Clone()
	}
	s.thoughts = slices.Clone(s.thoughts)
	for i := range s.thoughts {
		s.thoughts[i].writer = nil
	}
	return s
}

// Copy the whole catalogue, but only copy result payloads the user has opened.
func (s inspectionSource) toolListSnapshot(state viewState) inspectionSource {
	n := inspectionSource{version: s.version, epoch: s.epoch, tools: slices.Clone(s.tools)}
	for i := range n.tools {
		n.tools[i].result = messages.ChatMessage{}
		if state.toolItemExpanded(n.tools[i].key) {
			n.tools[i].result = s.tools[i].result.Clone()
		}
	}
	return n
}

// Headers and navigation need identities and status, not every result body.
// These snapshots remain cheap when an unrelated call or thought changes.
func (s inspectionSource) navigation() inspectionSource {
	n := inspectionSource{version: s.version, epoch: s.epoch}
	for _, t := range s.tools {
		n.tools = append(n.tools, inspectedTool{key: t.key,
			call:     messages.ChatMessageToolCall{ID: t.call.ID, Name: t.call.Name},
			complete: t.complete, pres: t.pres, started: t.started, duration: t.duration})
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
		n += int64(len(t.key) + len(t.call.ID) + len(t.call.Name) + len(t.pres.outcome))
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
	if s.thought != nil {
		fmt.Fprint(h, s.thought.text)
	}
	return fmt.Sprintf("item:%x", h.Sum(nil))
}

// Ignore assistant prose changes when refreshing a saved tool list.
func (s viewSource) toolListRevision() string {
	h := sha256.New()
	for _, tool := range s.model.inspections.tools {
		fmt.Fprintf(h, "%s:%t:%t:%s:%d:", tool.key, tool.available, tool.complete, tool.pres.outcome, tool.duration)
		_ = json.NewEncoder(h).Encode(tool.call)
		_ = json.NewEncoder(h).Encode(tool.result)
	}
	return fmt.Sprintf("tools:%x", h.Sum(nil))
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
				t.complete, t.pres, t.started = true, toolPresentation{}, time.Time{}
			}
		case messages.MessageRoleTool:
			t := source.ensureTool(messages.ChatMessageToolCall{ID: msg.ToolCallID, Name: msg.ToolName})
			source.setResult(t.call, msg)
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
				t := inspectedTool{call: messages.ChatMessageToolCall{ID: call.ID, Name: call.Name}, complete: true, version: 1}
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
