package openrouter

import (
	"encoding/json"
	"net/url"
	"strings"

	"github.com/alexschlessinger/pollytool/messages"
)

// MetadataKey is where a reply records the gateway identity, the requested
// model, upstream attribution and the reasoning a later request may replay:
// reasoning_details from Chat Completions, or reasoning_items from the
// Responses API.
const MetadataKey = "openrouter"

// Endpoint identifies the gateway, not an automatically selected upstream.
// Credentials, query parameters and fragments never enter history.
func Endpoint(endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		endpoint = DefaultBaseURL
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

// attributed returns a reply's gateway metadata when it was produced by this
// gateway for this requested model and completed. The actual upstream
// provider is deliberately irrelevant.
func attributed(msg messages.ChatMessage, endpoint, model string) map[string]any {
	if msg.Role != messages.MessageRoleAssistant || endpoint == "" || model == "" ||
		(msg.StopReason != messages.StopReasonEndTurn && msg.StopReason != messages.StopReasonToolUse) {
		return nil
	}
	m, ok := msg.Metadata[MetadataKey].(map[string]any)
	if !ok || m["endpoint"] != endpoint || m["requested_model"] != model || m["incomplete"] == true {
		return nil
	}
	return m
}

// recordedArray returns the JSON array stored under key, or nil. Null is
// not a recorded array; an explicit [] is.
func recordedArray(m map[string]any, key string) json.RawMessage {
	value, present := m[key]
	if !present {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil || len(raw) == 0 || raw[0] != '[' {
		return nil
	}
	return raw
}

// ChatReplay selects the Chat Completions reasoning a newly attributed reply
// can replay: the structured details when they were recorded, else the plain
// text. A reply made over the Responses API has neither.
func ChatReplay(msg messages.ChatMessage, endpoint, model string) (string, json.RawMessage) {
	m := attributed(msg, endpoint, model)
	if m == nil {
		return "", nil
	}
	if _, present := m["reasoning_details"]; present {
		return "", recordedArray(m, "reasoning_details")
	}
	if _, present := m["reasoning_items"]; present {
		return "", nil
	}
	return msg.Reasoning, nil
}

// ResponsesReplay selects the Responses reasoning items a newly attributed
// reply recorded, verbatim, or nil.
func ResponsesReplay(msg messages.ChatMessage, endpoint, model string) json.RawMessage {
	m := attributed(msg, endpoint, model)
	if m == nil {
		return nil
	}
	return recordedArray(m, "reasoning_items")
}

// Replay reports what a newly attributed reply replays to this gateway and
// model over whichever dialect recorded it: the structured payload (details
// or items) when one was recorded, else the plain text. Accounting and
// request fingerprints use it; the request builders use the dialect forms.
func Replay(msg messages.ChatMessage, endpoint, model string) (string, json.RawMessage) {
	if items := ResponsesReplay(msg, endpoint, model); items != nil {
		return "", items
	}
	return ChatReplay(msg, endpoint, model)
}
