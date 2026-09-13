package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
)

func TestOpenRouterThinkingSettingsUseCachedResolution(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/models" {
			fmt.Fprint(w, `{"data":[{"id":"m","reasoning":{"mandatory":true,"supported_efforts":["max","low","high"],"default_effort":"max"}}]}`)
			return
		}
		fmt.Fprint(w, `{"data":{"id":"m","endpoints":[{"tag":"host","supported_parameters":["reasoning"]}]}}`)
	}))
	defer server.Close()
	agent := llm.NewAgent(llm.NewMultiPass(map[string]string{"openrouter": "fixture"}), nil, llm.AgentConfig{})
	defer agent.Close()
	settings := Settings{Model: "openrouter/m", ThinkingEffort: "off"}
	ctx := &replCommandContext{settings: &settings, config: &Config{BaseURL: server.URL}, state: &conversationState{agent: agent}}
	if value, _ := replSettingValue(ctx, "thinking"); !strings.Contains(value, "unknown") {
		t.Fatal(value)
	}
	if calls.Load() != 0 {
		t.Fatal("settings fetched metadata")
	}
	if _, err := agent.LookupModel(context.Background(), llm.ModelTarget{Provider: "openrouter", Model: "m", BaseURL: server.URL}, false); err != nil {
		t.Fatal(err)
	}
	if value, _ := replSettingValue(ctx, "thinking"); value != "off → low (required)" {
		t.Fatal(value)
	}
	got := completeSetCommand(ctx, []string{"/set", "thinking"}, "")
	if !reflect.DeepEqual(got, []string{"dynamic", "high", "low", "max", "off"}) {
		t.Fatalf("hints: %v", got)
	}
	if _, err := applyAndPersistSetting(ctx, "thinking", "medium"); err == nil || !strings.Contains(err.Error(), "valid choices") {
		t.Fatalf("unsupported: %v", err)
	}
	if settings.ThinkingEffort != "off" {
		t.Fatal("unsupported preference saved")
	}
	if calls.Load() != 2 {
		t.Fatal("display/hints fetched metadata")
	}
	settings.Model = "openrouter/unknown"
	if value, _ := replSettingValue(ctx, "thinking"); !strings.Contains(value, "unknown") {
		t.Fatal("model switch reused policy: " + value)
	}
	settings.Model = "openai/gpt-5.4"
	if value, _ := replSettingValue(ctx, "thinking"); value != "off" {
		t.Fatal("other provider changed: " + value)
	}
}
