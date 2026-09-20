package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexschlessinger/pollytool/llm/openrouter"
	"github.com/alexschlessinger/pollytool/messages"
)

func TestOpenRouterThinkingWireAdaptsSavedPreferences(t *testing.T) {
	for _, tc := range []struct {
		effort, wire string
		caps         ModelCapabilities
		rejected     bool
	}{
		{"max", `null`, ModelCapabilities{}, false},
		{"max", `{"effort":"max"}`, ModelCapabilities{ReasoningEfforts: []string{"max"}, ReasoningEffortsComplete: true}, false},
		{"1234", `{"max_tokens":1234}`, ModelCapabilities{}, false},
		{"off", `{"enabled":false}`, ModelCapabilities{ReasoningMandatory: truth(false)}, false},
		{"off", `null`, ModelCapabilities{}, false},
		{"dynamic", `null`, ModelCapabilities{ReasoningMandatory: truth(true)}, false},
		{"medium", `null`, ModelCapabilities{ReasoningEfforts: []string{"low", "high", "max"}, ReasoningEffortsComplete: true}, false},
	} {
		t.Run(tc.effort+tc.wire, func(t *testing.T) {
			var called atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called.Store(true)
				var req openrouter.ChatRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				raw, _ := json.Marshal(req.Reasoning)
				if string(raw) != tc.wire || req.ReasoningEffort != "" {
					t.Errorf("wire controls: %s legacy=%s", raw, req.ReasoningEffort)
				}
				fmt.Fprint(w, `{"choices":[{"message":{"content":"done"},"finish_reason":"stop"}]}`)
			}))
			defer server.Close()
			effort, _ := ParseThinkingEffort(tc.effort)
			stream := false
			req := &CompletionRequest{Model: "openrouter/m", BaseURL: server.URL, ThinkingEffort: effort, Capabilities: &tc.caps, Stream: &stream}
			_, err := routerCompletion(context.Background(), NewMultiPass(map[string]string{"openrouter": "x"}), req)
			if tc.rejected {
				if err == nil || called.Load() {
					t.Fatalf("unsupported reached generation: %v", err)
				}
			} else if err != nil || !called.Load() {
				t.Fatalf("request: %v", err)
			}
			if req.ThinkingEffort != effort {
				t.Fatal("saved setting changed")
			}
		})
	}
}

func TestOpenRouterUnavailableMetadataUsesAvailablePolicy(t *testing.T) {
	for _, catalogOK := range []bool{false, true} {
		t.Run(fmt.Sprint(catalogOK), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/models" && catalogOK {
					fmt.Fprint(w, `{"data":[{"id":"m","reasoning":{"mandatory":true,"default_effort":"max"}}]}`)
					return
				}
				http.Error(w, "metadata unavailable", 503)
			}))
			defer server.Close()
			req := &CompletionRequest{Model: "openrouter/m", BaseURL: server.URL}
			caps := resolveRequestCapabilities(context.Background(), NewMultiPass(map[string]string{"openrouter": "x"}), req)
			if caps == nil {
				t.Fatal("unknown policy did not resolve")
			}
			prepared, notes, err := PrepareCapabilities(req, *caps, false)
			if err != nil || len(notes) != 1 || ResolveOpenRouterRequestThinking(prepared.ThinkingEffort, *caps).Request != nil {
				t.Fatalf("fallback: %v %v", notes, err)
			}
			want := "effective thinking unknown"
			if catalogOK {
				want = "minimum unknown"
			}
			if !strings.Contains(notes[0].Message, want) {
				t.Fatalf("fallback: %v", notes)
			}
		})
	}
}

func TestOpenRouterThinkingResolution(t *testing.T) {
	defaultEffort := "max"
	for _, tc := range []struct {
		name, effort       string
		caps               ModelCapabilities
		wire, display, err string
	}{
		{"required off", "off", ModelCapabilities{ReasoningMandatory: truth(true), ReasoningEfforts: []string{"max", "high", "low"}, ReasoningEffortsComplete: true}, `{"effort":"low"}`, "off → low (required)", ""},
		{"required unknown minimum", "off", ModelCapabilities{ReasoningMandatory: truth(true)}, `null`, "minimum unknown", ""},
		{"required default", "off", ModelCapabilities{ReasoningMandatory: truth(true), ReasoningDefaultEffort: &defaultEffort}, `null`, "max (provider default)", ""},
		{"required unrestricted", "off", ModelCapabilities{ReasoningMandatory: truth(true), ReasoningEffortsComplete: true}, `{"effort":"minimal"}`, "minimal (required)", ""},
		{"required future levels", "off", ModelCapabilities{ReasoningMandatory: truth(true), ReasoningEfforts: []string{"future"}, ReasoningEffortsComplete: true}, `null`, "minimum unknown", ""},
		{"unsupported", "medium", ModelCapabilities{ReasoningEfforts: []string{"low", "high", "max"}, ReasoningEffortsComplete: true}, "", "", "valid choices: low, high, max"},
		{"no named levels", "high", ModelCapabilities{ReasoningEfforts: []string{}, ReasoningEffortsComplete: true}, "", "", "no named efforts"},
		{"outside the gateway vocabulary", "max", ModelCapabilities{}, "", "", "valid choices: minimal, low, medium, high, xhigh"},
		{"model advertises its own", "max", ModelCapabilities{ReasoningEfforts: []string{"max"}, ReasoningEffortsComplete: true}, `{"effort":"max"}`, "max", ""},
		{"unknown support", "high", ModelCapabilities{}, `{"effort":"high"}`, "high", ""},
		{"partial list", "medium", ModelCapabilities{ReasoningEfforts: []string{"low"}}, `{"effort":"medium"}`, "medium", ""},
		{"optional off", "off", ModelCapabilities{ReasoningMandatory: truth(false)}, `{"enabled":false}`, "off", ""},
		{"unknown off", "off", ModelCapabilities{}, `null`, "effective thinking unknown", ""},
		{"dynamic", "dynamic", ModelCapabilities{ReasoningDefaultEffort: &defaultEffort}, `null`, "max (provider default)", ""},
		{"default disabled", "dynamic", ModelCapabilities{ReasoningDefaultEffort: &defaultEffort, ReasoningDefaultEnabled: truth(false)}, `null`, "off (provider default)", ""},
		{"budget", "1234", ModelCapabilities{}, `{"max_tokens":1234}`, "1234", ""},
		{"budget unsupported", "1234", ModelCapabilities{ReasoningMaxTokens: truth(false)}, "", "", "does not support a reasoning token budget"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			effort, err := ParseThinkingEffort(tc.effort)
			if err != nil {
				t.Fatal(err)
			}
			result, err := ResolveOpenRouterThinking(effort, tc.caps)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("error: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(result.Request)
			if string(raw) != tc.wire || !strings.Contains(result.Display, tc.display) {
				t.Fatalf("result: %s %+v", raw, result)
			}
			// The provider resolves from the capabilities Prepare records, so
			// the wire form must agree with the explicit resolution.
			req := &CompletionRequest{Model: "openrouter/m", ThinkingEffort: effort, Capabilities: &tc.caps}
			prepared, _, err := Prepare(context.Background(), nil, req, false)
			if err != nil || req.ThinkingEffort != effort || prepared.ThinkingEffort != effort || !reflect.DeepEqual(ResolveOpenRouterRequestThinking(prepared.ThinkingEffort, prepared.KnownCapabilities()), result) {
				t.Fatalf("execution disagrees: %+v %v", prepared, err)
			}
		})
	}
}

// The vocabulary a provider accepts is a fact of the provider table, so the
// forms and completions that offer effort words read it from there.
func TestThinkingEffortWordsFollowTheProvider(t *testing.T) {
	for _, tc := range []struct {
		name, model string
		caps        ModelCapabilities
		want        []string
	}{
		{"native providers clamp instead of rejecting", "anthropic/claude-sonnet-4-6", ModelCapabilities{},
			[]string{"off", "dynamic", "minimal", "low", "medium", "high", "xhigh", "max"}},
		{"the gateway has no max", "openrouter/org/model", ModelCapabilities{},
			[]string{"off", "dynamic", "minimal", "low", "medium", "high", "xhigh"}},
		{"a complete model list wins", "openrouter/org/model", ModelCapabilities{ReasoningEfforts: []string{"low", "max"}, ReasoningEffortsComplete: true},
			[]string{"off", "dynamic", "low", "max"}},
		{"a partial model list narrows nothing", "openrouter/org/model", ModelCapabilities{ReasoningEfforts: []string{"low"}},
			[]string{"off", "dynamic", "minimal", "low", "medium", "high", "xhigh"}},
		{"a model without reasoning keeps the preferences", "openrouter/org/model", ModelCapabilities{Reasoning: truth(false)},
			[]string{"off", "dynamic"}},
		{"an unknown provider offers everything", "made-up/model", ModelCapabilities{},
			[]string{"off", "dynamic", "minimal", "low", "medium", "high", "xhigh", "max"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ThinkingEffortWordsFor(tc.model, tc.caps); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("words = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestOpenRouterCatalogPolicyMerge(t *testing.T) {
	var catalogCalls, detailCalls atomic.Int32
	var offline atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if offline.Load() {
			http.Error(w, "offline", 503)
			return
		}
		if r.URL.Path == "/models" {
			catalogCalls.Add(1)
			fmt.Fprint(w, `{"data":[{"id":"org/required","reasoning":{"mandatory":true,"default_enabled":true,"default_effort":"max","supported_efforts":["max","high","low"]}},{"id":"org/optional","reasoning":{"mandatory":false,"supported_efforts":null}}]}`)
			return
		}
		detailCalls.Add(1)
		fmt.Fprint(w, `{"data":{"endpoints":[{"tag":"host","status":0,"supported_parameters":["tools","reasoning"],"reasoning":{"supports_max_tokens":false}},{"tag":"limited","status":0,"supported_parameters":["reasoning"],"reasoning":{"supported_efforts":["low","high"]}}]}}`)
	}))
	defer server.Close()
	m := NewMultiPass(map[string]string{"openrouter": "x"})
	agent := NewAgent(m, nil, AgentConfig{})
	defer agent.Close()
	target := ModelTarget{Provider: "openrouter", Model: "org/required", BaseURL: server.URL}
	info, err := m.GetModelInfo(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"host", "limited", "", "unknown"} {
		caps := info.EffectiveCapabilities(host)
		if caps.ReasoningMandatory == nil || !*caps.ReasoningMandatory || caps.ReasoningDefaultEffort == nil || *caps.ReasoningDefaultEffort != "max" {
			t.Fatalf("lost model policy on %s: %+v", host, caps)
		}
		if host == "host" && (caps.Tools == nil || !*caps.Tools || !reflect.DeepEqual(caps.ReasoningEfforts, []string{"high", "low", "max"}) || caps.ReasoningMaxTokens == nil || *caps.ReasoningMaxTokens) {
			t.Fatalf("host: %+v", caps)
		}
		if host == "limited" && (caps.Tools == nil || *caps.Tools || !reflect.DeepEqual(caps.ReasoningEfforts, []string{"high", "low"})) {
			t.Fatalf("limited: %+v", caps)
		}
		if host == "" && caps.ReasoningEffortsComplete {
			t.Fatal("different route effort lists treated as unrestricted")
		}
	}
	if catalogCalls.Load() != 1 || detailCalls.Load() != 1 {
		t.Fatal("unexpected catalog reads")
	}
	// Rendering can use stale in-memory facts without initiating network work.
	offline.Store(true)
	if cached := agent.CachedModelInfo(target); cached == nil || !*cached.ReasoningMandatory {
		t.Fatal("cached policy unavailable")
	}
	if catalogCalls.Load() != 1 || detailCalls.Load() != 1 {
		t.Fatal("cached read fetched")
	}
	for key, entry := range m.metadata.entries {
		entry.catalog.FetchedAt = time.Now().Add(-2 * modelMetadataTTL)
		m.metadata.entries[key] = entry
	}
	stale, err := m.LookupModel(context.Background(), target, true)
	if err != nil || len(stale.Models) != 1 || !stale.Stale || stale.Models[0].ReasoningMandatory == nil || !*stale.Models[0].ReasoningMandatory {
		t.Fatalf("stale fallback: %+v %v", stale, err)
	}
	other := target
	other.APIKey = "different-key"
	if agent.CachedModelInfo(other) != nil {
		t.Fatal("credential cache crossed scopes")
	}
	offline.Store(false)
	target.Model = "org/optional"
	optional, err := m.GetModelInfo(context.Background(), target)
	if err != nil || optional.ReasoningMandatory == nil || *optional.ReasoningMandatory || optional.ReasoningEfforts != nil || !optional.ReasoningEffortsComplete {
		t.Fatalf("unrestricted: %+v %v", optional, err)
	}
}

func TestOpenRouterContextAndRequestFingerprint(t *testing.T) {
	endpoint := "https://openrouter.ai/api/v1"
	details := json.RawMessage(`[{"type":"reasoning.encrypted","data":"` + strings.Repeat("x", 6000) + `"}]`)
	msg := messages.ChatMessage{Role: messages.MessageRoleAssistant, StopReason: messages.StopReasonToolUse, Reasoning: strings.Repeat("duplicate", 1000), ToolCalls: []messages.ChatMessageToolCall{{ID: "c", Name: "lookup", Arguments: "{}"}}, Metadata: map[string]any{"openrouter": map[string]any{"endpoint": endpoint, "requested_model": "m", "reasoning_details": details}}}
	history := []messages.ChatMessage{{Role: messages.MessageRoleUser, Content: "test"}, msg, {Role: messages.MessageRoleTool, ToolCallID: "c", Content: "result"}}
	req := &CompletionRequest{Model: "openrouter/m", Messages: history}
	state := &runState{projection: &projectionCache{}}
	projected, stats, err := projectCompletionRequest(context.Background(), req, nil, projectionTools{}, state)
	if err != nil {
		t.Fatal(err)
	}
	want := 0
	for _, m := range history {
		want += estimateProjectedMessageTokens(m)
	}
	want += estimatedStringTokens(string(details)) - estimatedStringTokens(msg.Reasoning)
	if stats.EstimatedTokens != want || !reflect.DeepEqual(projected, history) {
		t.Fatalf("accounting/projection: %d want %d %v", stats.EstimatedTokens, want, err)
	}
	key, _ := derivePromptCacheKey(req, history, nil)
	req.Messages = cloneMessages(history)
	req.Messages[1].Reasoning = "changed display only"
	same, _ := derivePromptCacheKey(req, req.Messages, nil)
	if key != same {
		t.Fatal("display duplicate in replay fingerprint")
	}
	req.Messages[1] = req.Messages[1].Clone()
	req.Messages[1].Metadata = map[string]any{"openrouter": map[string]any{"endpoint": endpoint, "requested_model": "m", "reasoning_details": json.RawMessage(`[]`)}}
	changed, _ := derivePromptCacheKey(req, req.Messages, nil)
	if key == changed {
		t.Fatal("replay missing from request fingerprint")
	}
	req.Model = "openrouter/other"
	_, stats, err = projectCompletionRequest(context.Background(), req, nil, projectionTools{}, state)
	if err != nil {
		t.Fatal(err)
	}
	withoutReasoning := 0
	for _, m := range req.Messages {
		withoutReasoning += estimateProjectedMessageTokens(m) - estimatedStringTokens(m.Reasoning)
	}
	if stats.EstimatedTokens != withoutReasoning {
		t.Fatalf("foreign reasoning counted: %d want %d", stats.EstimatedTokens, withoutReasoning)
	}
	if history[1].Reasoning != msg.Reasoning {
		t.Fatal("historical message changed")
	}
}
