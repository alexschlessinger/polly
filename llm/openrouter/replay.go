package openrouter

import (
	"encoding/json"
	"net/url"
	"strings"

	"github.com/alexschlessinger/pollytool/messages"
)

// MetadataKey is where a reply records the gateway identity, the requested
// model, upstream attribution and the reasoning a later request may replay.
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

// Replay selects the Chat Completions reasoning a newly attributed reply can
// replay: the structured details when they were recorded (an explicit []
// counts), else the plain text.
func Replay(msg messages.ChatMessage, endpoint, model string) (string, json.RawMessage) {
	m := attributed(msg, endpoint, model)
	if m == nil {
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
