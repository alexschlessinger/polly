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
