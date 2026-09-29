package openai

import (
	"strings"

	"github.com/alexschlessinger/pollytool/messages"
)

// orderResponseInput orders references to the current normalized history.
// Reasoning that cannot cross a model/gateway boundary and calls removed by
// capability projection are absent from items and therefore cannot reappear.
func orderResponseInput(msg messages.ChatMessage, messageIndex int, items []ResponseInputItem) []ResponseInputItem {
	if !msg.HasTextBlocks() {
		return items
	}
	var order []string
	switch value := msg.Metadata[ResponsesOutputOrderKey].(type) {
	case []string:
		order = value
	case []any:
		for _, entry := range value {
			if key, ok := entry.(string); ok {
				order = append(order, key)
			}
		}
	}
	if len(order) == 0 {
		return items
	}
	byKey := make(map[string]int, len(items))
	for i, item := range items {
		key := item.Type + ":" + item.ID
		if item.Type == "message" {
			key = "message:" + strings.TrimPrefix(item.ID, responseReplayMessageID(messageIndex)+"_")
		}
		if item.Type == "function_call" {
			key = "function_call:" + item.CallID
		}
		// A gateway's reasoning can be a verbatim Raw item.
		if item.Raw != nil {
			var raw ResponseOutputItem
			if err := raw.UnmarshalJSON(item.Raw); err == nil {
				key = raw.Type + ":" + raw.ID
			}
		}
		byKey[key] = i
	}
	used := make([]bool, len(items))
	out := make([]ResponseInputItem, 0, len(items))
	for _, key := range order {
		if i, ok := byKey[key]; ok && !used[i] {
			out = append(out, items[i])
			used[i] = true
		}
	}
	for i, item := range items {
		if !used[i] {
			out = append(out, item)
		}
	}
	return out
}
