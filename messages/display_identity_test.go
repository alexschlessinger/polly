package messages

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestDisplayIdentityCloneAndJSON(t *testing.T) {
	original := ChatMessage{
		Role:       MessageRoleAssistant,
		TextBlocks: []AssistantText{{ID: "provider-text", Text: "checking"}},
		ToolCalls:  []ChatMessageToolCall{{ID: "provider-call", Name: "work", Arguments: `{}`, DisplayID: "host-call"}},
		Metadata:   map[string]any{MetadataKeyDisplayID: "host-message"},
	}
	clone := original.Clone()
	if !reflect.DeepEqual(clone, original) {
		t.Fatalf("clone lost identity: %#v", clone)
	}
	clone.Metadata[MetadataKeyDisplayID] = "changed-message"
	clone.ToolCalls[0].DisplayID = "changed-call"
	if original.DisplayID() != "host-message" || original.ToolCalls[0].DisplayID != "host-call" {
		t.Fatal("clone shared display identity containers")
	}
	for _, msg := range []ChatMessage{original, {
		Role: MessageRoleTool, ToolCallID: "provider-call", ToolName: "work", Content: "result",
		Metadata: map[string]any{MetadataKeyDisplayID: "host-call"},
	}} {
		data, err := json.Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		var reloaded ChatMessage
		if err := json.Unmarshal(data, &reloaded); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(reloaded, msg) {
			t.Fatalf("JSON round trip changed identity or protocol fields: %#v != %#v", reloaded, msg)
		}
	}
}

func TestDisplayIdentityIsOptional(t *testing.T) {
	for _, metadata := range []map[string]any{nil, {}, {MetadataKeyDisplayID: nil}, {MetadataKeyDisplayID: 1}} {
		if id := (ChatMessage{Metadata: metadata}).DisplayID(); id != "" {
			t.Fatalf("invalid display identity = %q", id)
		}
	}
	data, err := json.Marshal(ChatMessageToolCall{ID: "legacy", Name: "work"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "display_id") {
		t.Fatalf("absent display identity was serialized: %s", data)
	}
}
