package openai

import (
	"encoding/json"
	"testing"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
)

func TestResponsesReasoningRequest(t *testing.T) {
	yes, no := true, false
	for _, caps := range []struct {
		name  string
		value *contract.ModelCapabilities
	}{
		{"reasoning", &contract.ModelCapabilities{Reasoning: &yes}},
		{"nonreasoning", &contract.ModelCapabilities{Reasoning: &no}},
		{"unknown", &contract.ModelCapabilities{}},
		{"nil", nil},
	} {
		for _, effort := range []struct {
			name  string
			value contract.ThinkingEffort
			wire  string
		}{
			{"default", contract.ThinkingEffort{}, ""},
			{"off", contract.EffortOff(), ""},
			{"dynamic", contract.EffortDynamic(), ""},
			{"high", contract.EffortLevel(contract.LevelHigh), "high"},
			{"budget", contract.EffortBudget(4096), "low"},
		} {
			t.Run(caps.name+"/"+effort.name, func(t *testing.T) {
				req := &contract.CompletionRequest{Model: "model", Capabilities: caps.value, ThinkingEffort: effort.value}
				body, err := json.Marshal(BuildResponsesRequest(req))
				if err != nil {
					t.Fatal(err)
				}
				var wire map[string]json.RawMessage
				if err := json.Unmarshal(body, &wire); err != nil {
					t.Fatal(err)
				}
				wantBlock := caps.name == "reasoning" || (caps.name != "nonreasoning" && effort.value.IsEnabled())
				if !wantBlock {
					if block, ok := wire["reasoning"]; ok {
						t.Fatalf("unexpected reasoning block: %s", block)
					}
					return
				}
				var reasoning map[string]string
				if err := json.Unmarshal(wire["reasoning"], &reasoning); err != nil {
					t.Fatalf("reasoning block in %s: %v", body, err)
				}
				if reasoning["summary"] != "auto" || reasoning["effort"] != effort.wire {
					t.Fatalf("reasoning = %v, want summary:auto effort:%q", reasoning, effort.wire)
				}
				if _, present := reasoning["effort"]; present != (effort.wire != "") {
					t.Fatalf("effort must be omitted for provider defaults: %s", wire["reasoning"])
				}
			})
		}
	}
}
