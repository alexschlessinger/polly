package adapters

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strings"

	"github.com/alexschlessinger/pollytool/llm/openai"
	"github.com/alexschlessinger/pollytool/llm/streaming"
	"github.com/alexschlessinger/pollytool/messages"
)

const OpenRouterMetadataKey = "openrouter"

// OpenRouterEndpoint identifies the gateway, not an automatically selected
// upstream. Credentials, query parameters and fragments never enter history.
func OpenRouterEndpoint(endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		endpoint = "https://openrouter.ai/api/v1"
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || u.Scheme == "" {
		return ""
	}
	u.User, u.RawQuery, u.Fragment, u.RawFragment = nil, "", "", ""
	u.ForceQuery = false
	u.Scheme, u.Host = strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = strings.TrimRight(u.RawPath, "/")
	return u.String()
}

// OpenRouterReplay selects only a newly attributed response for this gateway
// and requested model. The actual upstream provider is deliberately irrelevant.
func OpenRouterReplay(msg messages.ChatMessage, endpoint, model string) (string, json.RawMessage) {
	if msg.Role != messages.MessageRoleAssistant || endpoint == "" || model == "" {
		return "", nil
	}
	m, ok := msg.Metadata[OpenRouterMetadataKey].(map[string]any)
	if !ok || m["endpoint"] != endpoint || m["requested_model"] != model || m["incomplete"] == true {
		return "", nil
	}
	if details, present := m["reasoning_details"]; present {
		raw, err := json.Marshal(details)
		// Null is not a recorded details array. An explicit [] is.
		if err == nil && len(raw) > 0 && raw[0] == '[' {
			return "", raw
		}
		return "", nil
	}
	return msg.Reasoning, nil
}

type reasoningBlock struct {
	fields   map[string]json.RawMessage
	text     strings.Builder
	field    string
	textSeen bool
}

// OpenRouterAdapter owns one response. Text/summary deltas join consecutive
// logical blocks, never a global index bucket: some providers repeat index 0
// for every block. Encrypted and unknown blocks remain separate and opaque.
type OpenRouterAdapter struct {
	*OpenAIAdapter
	metadata        map[string]any
	blocks          []*reasoningBlock
	details         bool
	plaintext       bool
	completeDetails json.RawMessage
}

func NewOpenRouterAdapter(endpoint, model string) *OpenRouterAdapter {
	return &OpenRouterAdapter{
		OpenAIAdapter: NewOpenAIAdapter(),
		metadata:      map[string]any{"endpoint": OpenRouterEndpoint(endpoint), "requested_model": model},
	}
}

func (a *OpenRouterAdapter) ProcessChunk(chunk any, state streaming.StreamStateInterface) error {
	if r, ok := chunk.(openai.ChatCompletionChunk); ok {
		chunk = &r
	}
	if err := a.OpenAIAdapter.ProcessChunk(chunk, state); err != nil {
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

func (a *OpenRouterAdapter) attribution(id, model, provider string) {
	for k, v := range map[string]string{"response_id": id, "model": model, "provider": provider} {
		if v != "" {
			a.metadata[k] = v
		}
	}
}

func (a *OpenRouterAdapter) addDetails(raw json.RawMessage) error {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var details []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &details); err != nil {
		return err
	}
	a.details = true
	for _, detail := range details {
		field := ""
		switch rawString(detail["type"]) {
		case "reasoning.text":
			field = "text"
		case "reasoning.summary":
			field = "summary"
		}
		var block *reasoningBlock
		if n := len(a.blocks); n > 0 && field != "" && a.blocks[n-1].field == field && compatibleReasoningFields(a.blocks[n-1].fields, detail, field) {
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
// fields. Streaming indices are advisory and cannot establish identity.
func compatibleReasoningFields(a, b map[string]json.RawMessage, field string) bool {
	for k, v := range b {
		if k == field || k == "index" || bytes.Equal(v, []byte("null")) || bytes.Equal(v, []byte(`""`)) {
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

func (a *OpenRouterAdapter) EnrichFinalMessage(msg *messages.ChatMessage, state streaming.StreamStateInterface) {
	if a.details {
		details := make([]map[string]json.RawMessage, 0, len(a.blocks))
		var display strings.Builder
		for _, block := range a.blocks {
			if block.textSeen {
				block.fields[block.field], _ = json.Marshal(block.text.String())
				display.WriteString(block.text.String())
			}
			details = append(details, block.fields)
		}
		a.metadata["reasoning_details"] = details
		// Plaintext is already streamed. Only use structured display when
		// that channel was absent, so a dual-channel response appears once.
		if !a.plaintext {
			msg.Reasoning = display.String()
		}
	}
	if a.completeDetails != nil {
		a.metadata["reasoning_details"] = a.completeDetails
		if !a.plaintext {
			var details []map[string]json.RawMessage
			_ = json.Unmarshal(a.completeDetails, &details)
			var display strings.Builder
			for _, d := range details {
				switch rawString(d["type"]) {
				case "reasoning.text":
					display.WriteString(rawString(d["text"]))
				case "reasoning.summary":
					display.WriteString(rawString(d["summary"]))
				}
			}
			msg.Reasoning = display.String()
		}
	}
	if msg.Metadata == nil {
		msg.Metadata = map[string]any{}
	}
	msg.Metadata[OpenRouterMetadataKey] = a.metadata
}
