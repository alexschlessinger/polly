package messages

import (
	"encoding/json"
	"testing"
)

func TestCompactionMarkerSurvivesPersistence(t *testing.T) {
	for _, c := range []Compaction{
		{Summary: "the user asked for a parser", KeepsTurn: true},
		{ClearThrough: "call_7"},
	} {
		data, err := json.Marshal(c.Message())
		if err != nil {
			t.Fatal(err)
		}
		var saved ChatMessage
		if err := json.Unmarshal(data, &saved); err != nil {
			t.Fatal(err)
		}
		got, ok := saved.Compaction()
		if !ok || got != c {
			t.Fatalf("saved marker = %+v, %v; want %+v", got, ok, c)
		}
		if saved.IsUsageRecord() || len(ModelVisible([]ChatMessage{saved})) != 0 {
			t.Fatalf("marker is model-visible or a usage record: %+v", saved)
		}
	}
}

func TestCompactionIgnoresOtherMessages(t *testing.T) {
	for _, m := range []ChatMessage{
		{Role: MessageRoleUser, Metadata: map[string]any{MetadataKeyCompaction: map[string]any{"summary": "x"}}},
		{Role: MessageRoleInternal, Metadata: map[string]any{MetadataKeyCompaction: map[string]any{}}},
		{Role: MessageRoleInternal, Metadata: map[string]any{MetadataKeyUsageOnly: true}},
	} {
		if c, ok := m.Compaction(); ok {
			t.Fatalf("%+v read as compaction %+v", m, c)
		}
	}
}
