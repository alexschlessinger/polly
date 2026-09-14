package openrouter

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/alexschlessinger/pollytool/llm/openai"
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
)

type reasoningBlock struct {
	fields   map[string]json.RawMessage
	text     strings.Builder
	field    string
	textSeen bool
}

// ChatAdapter owns one Chat Completions response. Text/summary deltas join
// consecutive logical blocks, never a global index bucket: some providers
// repeat index 0 for every block. Encrypted and unknown blocks remain
// separate and opaque.
type ChatAdapter struct {
	*openai.ChatAdapter
	metadata        map[string]any
	blocks          []*reasoningBlock
	details         bool
	plaintext       bool
	completeDetails json.RawMessage
}

// NewChatAdapter returns an adapter recording replies as produced by
// endpoint for the requested model.
func NewChatAdapter(endpoint, model string) *ChatAdapter {
	return &ChatAdapter{
		ChatAdapter: openai.NewChatAdapter(),
		metadata:    map[string]any{"endpoint": Endpoint(endpoint), "requested_model": model},
	}
}

func (a *ChatAdapter) ProcessChunk(chunk any, state streaming.StreamStateInterface) error {
	if err := a.ChatAdapter.ProcessChunk(chunk, state); err != nil {
		return err
	}
	switch r := chunk.(type) {
	case *openai.ChatCompletionChunk:
		a.attribution(r.ID, r.Model, r.Provider)
		if len(r.Choices) > 0 {
			a.plaintext = a.plaintext || r.Choices[0].Delta.ReasoningText() != ""
			return a.addDetails(r.Choices[0].Delta.ReasoningDetails)
		}
	case *openai.ChatCompletion:
		a.attribution(r.ID, r.Model, r.Provider)
		if len(r.Choices) > 0 {
			a.plaintext = a.plaintext || r.Choices[0].Message.ReasoningText() != ""
			raw := r.Choices[0].Message.ReasoningDetails
			if len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				// A completed array already has block boundaries. Only streaming
				// fragments need reassembly; preserve the full response verbatim.
				var details []map[string]json.RawMessage
				if err := json.Unmarshal(raw, &details); err != nil {
					return err
				}
				a.completeDetails = raw
			}
		}
	}
	return nil
}

func (a *ChatAdapter) attribution(id, model, provider string) {
	if id != "" {
		a.metadata["response_id"] = id
	}
	if model != "" {
		a.metadata["model"] = model
	}
	if provider != "" {
		a.metadata["provider"] = provider
	}
}

func (a *ChatAdapter) addDetails(raw json.RawMessage) error {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var details []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &details); err != nil {
		return err
	}
	a.details = true
	for index, detail := range details {
		field := ""
		switch rawString(detail["type"]) {
		case "reasoning.text":
			field = "text"
		case "reasoning.summary":
			field = "summary"
		}
		var block *reasoningBlock
		if n := len(a.blocks); index == 0 && n > 0 && field != "" && a.blocks[n-1].field == field && compatibleReasoningFields(a.blocks[n-1].fields, detail, field) {
			block = a.blocks[n-1]
		} else {
			block = &reasoningBlock{fields: map[string]json.RawMessage{}, field: field}
			a.blocks = append(a.blocks, block)
		}
		for k, v := range detail {
			if field != "" && k == field {
				var text string
				if len(v) > 0 && v[0] == '"' && json.Unmarshal(v, &text) == nil {
					block.textSeen = true
					block.text.WriteString(text)
				} else {
					block.fields[k] = v
				}
			} else if old, exists := block.fields[k]; !exists || bytes.Equal(old, []byte("null")) || bytes.Equal(old, []byte(`""`)) {
				block.fields[k] = v
			}
		}
	}
	return nil
}

// Preserve conflicting identities/opaque fields as separate blocks rather
// than silently overwriting them. Late signatures and formats fill missing
// fields. A signature seals its text, and a changed index starts a new block.
// Repeated indices alone cannot establish identity.
func compatibleReasoningFields(a, b map[string]json.RawMessage, field string) bool {
	if rawString(a["signature"]) != "" && rawString(b[field]) != "" {
		return false
	}
	for k, v := range b {
		if k == field || bytes.Equal(v, []byte("null")) || bytes.Equal(v, []byte(`""`)) {
			continue
		}
		if old, ok := a[k]; ok && !bytes.Equal(old, []byte("null")) && !bytes.Equal(old, []byte(`""`)) && !bytes.Equal(old, v) {
			return false
		}
	}
	return true
}

func rawString(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

// reasoningDisplay concatenates the readable text of reasoning details, for
// responses whose reasoning arrived only in structured form.
func reasoningDisplay(details []map[string]json.RawMessage) string {
	var display strings.Builder
	for _, d := range details {
		switch rawString(d["type"]) {
		case "reasoning.text":
			display.WriteString(rawString(d["text"]))
		case "reasoning.summary":
			display.WriteString(rawString(d["summary"]))
		}
	}
	return display.String()
}

func (a *ChatAdapter) EnrichFinalMessage(msg *messages.ChatMessage, state streaming.StreamStateInterface) {
	if msg.StopReason != messages.StopReasonEndTurn && msg.StopReason != messages.StopReasonToolUse {
		a.metadata["incomplete"] = true
	}
	var details []map[string]json.RawMessage
	if a.details {
		details = make([]map[string]json.RawMessage, 0, len(a.blocks))
		for _, block := range a.blocks {
			if block.textSeen {
				block.fields[block.field], _ = json.Marshal(block.text.String())
			}
			details = append(details, block.fields)
		}
		a.metadata["reasoning_details"] = details
	}
	if a.completeDetails != nil {
		a.metadata["reasoning_details"] = a.completeDetails
		_ = json.Unmarshal(a.completeDetails, &details)
	}
	// Plaintext is already streamed. Only use structured display when that
	// channel was absent, so a dual-channel response appears once.
	if details != nil && !a.plaintext {
		msg.Reasoning = reasoningDisplay(details)
	}
	if msg.Metadata == nil {
		msg.Metadata = map[string]any{}
	}
	msg.Metadata[MetadataKey] = a.metadata
}

// ResponsesAdapter owns one Responses API reply. It records the gateway
// attribution and every reasoning item exactly as sent, whatever fields the
// upstream model's format carries, so the next request can pass them back
// untouched.
type ResponsesAdapter struct {
	*openai.ResponsesAdapter
	metadata map[string]any
	items    []json.RawMessage
	index    map[string]int
}

// NewResponsesAdapter returns an adapter recording replies as produced by
// endpoint for the requested model.
func NewResponsesAdapter(endpoint, model string) *ResponsesAdapter {
	return &ResponsesAdapter{
		ResponsesAdapter: openai.NewResponsesAdapter(model),
		metadata:         map[string]any{"endpoint": Endpoint(endpoint), "requested_model": model},
		index:            map[string]int{},
	}
}

func (a *ResponsesAdapter) ProcessChunk(chunk any, state streaming.StreamStateInterface) error {
	if err := a.ResponsesAdapter.ProcessChunk(chunk, state); err != nil {
		return err
	}
	switch r := chunk.(type) {
	case *openai.ResponseStreamEvent:
		switch r.Type {
		case "response.output_item.done":
			a.record(r.Item)
		case "response.completed", "response.incomplete", "response.failed":
			a.recordResponse(r.Response)
		}
	case *openai.Response:
		a.recordResponse(r)
	}
	return nil
}

// recordResponse takes the finished response as authoritative: its items
// replace whatever the stream delivered piecemeal.
func (a *ResponsesAdapter) recordResponse(resp *openai.Response) {
	if resp == nil {
		return
	}
	if resp.ID != "" {
		a.metadata["response_id"] = resp.ID
	}
	if resp.Model != "" {
		a.metadata["model"] = resp.Model
	}
	a.items, a.index = nil, map[string]int{}
	for i := range resp.Output {
		a.record(&resp.Output[i])
	}
}

// record keeps a reasoning item verbatim; a repeated id replaces the earlier
// copy, which an output_item.added event delivers before its content.
func (a *ResponsesAdapter) record(item *openai.ResponseOutputItem) {
	if item == nil || item.Type != "reasoning" || item.Raw == nil {
		return
	}
	if i, seen := a.index[item.ID]; seen && item.ID != "" {
		a.items[i] = item.Raw
		return
	}
	if item.ID != "" {
		a.index[item.ID] = len(a.items)
	}
	a.items = append(a.items, item.Raw)
}

func (a *ResponsesAdapter) EnrichFinalMessage(msg *messages.ChatMessage, state streaming.StreamStateInterface) {
	a.ResponsesAdapter.EnrichFinalMessage(msg, state)
	if msg.Metadata == nil {
		msg.Metadata = map[string]any{}
	}
	// The gateway's items are replayed through this package's metadata, not
	// OpenAI's encrypted-item replay.
	delete(msg.Metadata, openai.ResponsesReasoningItemsKey)
	delete(msg.Metadata, openai.ResponsesReasoningModelKey)
	if msg.StopReason != messages.StopReasonEndTurn && msg.StopReason != messages.StopReasonToolUse {
		a.metadata["incomplete"] = true
	}
	if a.items != nil {
		a.metadata["reasoning_items"] = a.items
	}
	msg.Metadata[MetadataKey] = a.metadata
}
