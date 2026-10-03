package messages

import (
	"encoding/json"
	"testing"
)

func TestUsageRecordSurvivesPersistenceWithoutReplayState(t *testing.T) {
	response := ChatMessage{
		Role: MessageRoleAssistant, Content: "the summary", Reasoning: "private reasoning",
		ToolCalls: []ChatMessageToolCall{{ID: "call", Name: "fetch", Arguments: `{}`}},
		Parts:     []ContentPart{{Type: "text", Text: "summary part"}},
		Metadata:  map[string]any{"provider_replay": "opaque state"},
	}
	response.SetTokenUsage(100, 20)
	response.SetPromptCacheUsage(50, 10)
	response.SetReportedCost(0.25)
	data, err := json.Marshal(response.UsageRecord())
	if err != nil {
		t.Fatal(err)
	}
	var saved ChatMessage
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if !saved.IsUsageRecord() || saved.Content != "" || saved.Reasoning != "" || len(saved.ToolCalls) != 0 || len(saved.Parts) != 0 || saved.Metadata["provider_replay"] != nil {
		t.Fatalf("saved usage retained replay state: %+v", saved)
	}
	if saved.GetInputTokens() != 100 || saved.GetOutputTokens() != 20 || saved.GetCacheReadInputTokens() != 50 || saved.GetCacheWriteInputTokens() != 10 {
		t.Fatalf("saved usage = %+v", saved.Metadata)
	}
	if cost, ok := saved.GetReportedCost(); !ok || cost != 0.25 {
		t.Fatalf("saved cost = %v, %v", cost, ok)
	}
	unknown := (ChatMessage{}).UsageRecord()
	if len(unknown.Metadata) != 1 {
		t.Fatalf("unreported usage became reported zero: %+v", unknown.Metadata)
	}
	if response.Content != "the summary" || len(response.Metadata) != 6 {
		t.Fatal("usage record changed its source response")
	}
}
