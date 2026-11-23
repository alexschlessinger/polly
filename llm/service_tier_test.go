package llm_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/alexschlessinger/pollytool/llm/internal/contract"
)

func TestServiceTierValidationAndPreparation(t *testing.T) {
	const model = "openai/gpt-6.2-astra"
	ruledOut := &llm.ModelCapabilities{
		Parameters:   map[string]bool{"service_tier": false},
		ServiceTiers: []string{llm.ServiceTierPriority, llm.ServiceTierUltrafast},
	}
	noTiers := &llm.ModelCapabilities{ServiceTiers: []string{}}
	priorityOnly := &llm.ModelCapabilities{ServiceTiers: []string{llm.ServiceTierPriority}}
	ultrafastOnly := &llm.ModelCapabilities{ServiceTiers: []string{llm.ServiceTierUltrafast}}
	for _, tc := range []struct {
		model, tier, baseURL string
		caps                 *llm.ModelCapabilities
		ok                   bool
	}{
		{"model-without-a-tier", "", "", nil, true},
		{model, llm.ServiceTierUltrafast, "", nil, true},
		{"openai/gpt-6.2-sol", llm.ServiceTierUltrafast, "", nil, true},
		{"openai/gpt-6-astra", llm.ServiceTierUltrafast, "", nil, true},
		{"openai/gpt-6.1-sol", llm.ServiceTierUltrafast, "", nil, true},
		{model, llm.ServiceTierPriority, "https://custom.invalid", nil, true},
		{"codex/gpt-5.5", llm.ServiceTierPriority, "", nil, true},
		{model, llm.ServiceTierUltrafast, "https://custom.invalid", nil, false},
		{"openai/gpt-6-sol", llm.ServiceTierUltrafast, "", nil, false},
		{"openai/gpt-6-astra-preview", llm.ServiceTierUltrafast, "", nil, false},
		{"codex/gpt-6.2-astra", llm.ServiceTierUltrafast, "", nil, false},
		{"openrouter/openai/gpt-6.2-astra", llm.ServiceTierUltrafast, "", nil, false},
		{"custom/gpt-6.2-astra", llm.ServiceTierUltrafast, "", nil, false},
		{model, "ultra", "", nil, false},
		{model, llm.ServiceTierUltrafast, "", ruledOut, false},
		{model, llm.ServiceTierPriority, "", ruledOut, false},
		{model, llm.ServiceTierPriority, "", noTiers, false},
		{model, llm.ServiceTierPriority, "https://custom.invalid", priorityOnly, true},
		{model, llm.ServiceTierUltrafast, "", priorityOnly, false},
		{model, llm.ServiceTierPriority, "", ultrafastOnly, false},
		{model, llm.ServiceTierUltrafast, "", ultrafastOnly, true},
		{model, llm.ServiceTierUltrafast, "https://custom.invalid", ultrafastOnly, false},
		{"openai/gpt-6-sol", llm.ServiceTierUltrafast, "", ultrafastOnly, false},
	} {
		req := &llm.CompletionRequest{Model: tc.model, ServiceTier: tc.tier, BaseURL: tc.baseURL, Capabilities: tc.caps, Fast: tc.tier != ""}
		err := llm.ServiceTierFor(tc.model, tc.baseURL, req.KnownCapabilities(), tc.tier)
		if (err == nil) != tc.ok {
			t.Errorf("validate %s tier=%q URL=%q caps=%+v: %v", tc.model, tc.tier, tc.baseURL, tc.caps, err)
		}
		prepared, notes, err := llm.Prepare(context.Background(), nil, req, false)
		if err != nil {
			t.Fatal(err)
		}
		if tc.ok {
			if prepared.ServiceTier != tc.tier || len(notes) != 0 {
				t.Errorf("%s: wanted tier %q; got %q notes=%v", tc.model, tc.tier, prepared.ServiceTier, notes)
			}
		} else if prepared.ServiceTier != "" || prepared.Fast || !slices.ContainsFunc(notes, func(note llm.RequestAdaptation) bool {
			return note.Feature == "fast" && note.Count == 1 && strings.Contains(note.Message, "omitted")
		}) {
			t.Errorf("%s: did not omit %q with a note: tier=%q fast=%t notes=%v", tc.model, tc.tier, prepared.ServiceTier, prepared.Fast, notes)
		}
		if req.ServiceTier != tc.tier || req.Fast != (tc.tier != "") {
			t.Error("preparation mutated the caller's tier")
		}
	}
}

// Providers enforce tier restrictions even when callers bypass Prepare.
func TestServiceTierOnTheWireWithoutPrepare(t *testing.T) {
	const model = "openai/gpt-6.2-astra"
	for _, tc := range []struct {
		name, model, baseURL, tier string
		fast                       bool
		want, endpoint             string
	}{
		{"default", model, "", "", false, "", "/responses"},
		{"legacy", model, "", "", true, "priority", "/responses"},
		{"priority", model, "", "priority", false, "priority", "/responses"},
		{"ultrafast", model, "", "ultrafast", true, "ultrafast", "/responses"},
		{"older model", "openai/gpt-6-sol", "", "ultrafast", true, "", "/responses"},
		{"unknown tier", model, "", "express", true, "", "/responses"},
		{"custom endpoint", model, "https://example.invalid", "ultrafast", true, "", "/chat/completions"},
		{"compatible priority", model, "https://example.invalid", "priority", false, "priority", "/chat/completions"},
		{"chat gateway ultrafast", "huggingface/gpt-6.2-astra", "", "ultrafast", true, "", "/chat/completions"},
		{"chat gateway priority", "huggingface/gpt-6.2-astra", "", "priority", true, "", "/chat/completions"},
		{"chat gateway legacy Fast", "openrouter/openai/gpt-6.2-astra", "", "", true, "", "/chat/completions"},
		{"codex priority", "codex/gpt-6.2-astra", "", "priority", false, "priority", "/responses"},
		{"codex ultrafast without catalog", "codex/gpt-6.2-astra", "", "ultrafast", true, "", "/responses"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider, slug, _ := strings.Cut(tc.model, "/")
			calls := 0
			client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, tc.endpoint) {
					return nil, fmt.Errorf("unexpected request: %s %s", req.Method, req.URL)
				}
				checkServiceTierOnWire(t, req, provider, slug, tc.want)
				if provider == "codex" {
					return jsonResponse(codexSSE), nil
				}
				if tc.endpoint == "/responses" {
					return jsonResponse(responseJSON), nil
				}
				return jsonResponse(chatJSON), nil
			})}
			router := llm.NewMultiPass(map[string]string{provider: "key"}, llm.WithHTTPClient(client), llm.WithLogin("codex", staticLogin{}))
			req := &llm.CompletionRequest{Model: tc.model, BaseURL: tc.baseURL, Fast: tc.fast, ServiceTier: tc.tier, StreamMode: llm.Buffered}
			if _, err := contract.Complete(t.Context(), router, req); err != nil || calls != 1 {
				t.Fatalf("HTTP calls=%d err=%v", calls, err)
			}
			if req.Model != tc.model || req.BaseURL != tc.baseURL || req.APIKey != "" || req.Fast != tc.fast || req.ServiceTier != tc.tier {
				t.Fatal("routing mutated the caller's request")
			}
		})
	}
}

func TestCodexServiceTierFromSignedInCatalog(t *testing.T) {
	// The catalog can enable a Codex-only slug or omit ultrafast for a slug
	// that is on api.openai.com's ultrafast allowlist.
	const catalog = `{"models":[
		{"slug":"codex-exclusive","service_tiers":[{"id":"ultrafast"}]},
		{"slug":"gpt-6-astra","service_tiers":[{"id":"priority"}]},
		{"slug":"gpt-6.2-astra"},
		{"slug":"codex-no-tiers","service_tiers":null},
		{"slug":"codex-legacy","service_tiers":null,"additional_speed_tiers":["fast"]}
	]}`
	for _, tc := range []struct {
		name, slug, requested, want string
		advertised                  []string
	}{
		{"advertised ultrafast", "codex-exclusive", "ultrafast", "ultrafast", []string{"ultrafast"}},
		{"ultrafast does not imply priority", "codex-exclusive", "priority", "", []string{"ultrafast"}},
		{"advertised priority", "gpt-6-astra", "priority", "priority", []string{"priority"}},
		{"priority is not a fallback", "gpt-6-astra", "ultrafast", "", []string{"priority"}},
		{"unknown tier", "gpt-6-astra", "express", "", []string{"priority"}},
		{"missing tiers cannot enable ultrafast", "gpt-6.2-astra", "ultrafast", "", nil},
		{"missing tiers keep legacy priority", "gpt-6.2-astra", "priority", "priority", nil},
		{"null tiers disable priority", "codex-no-tiers", "priority", "", []string{}},
		{"legacy speeds can enable priority with null tiers", "codex-legacy", "priority", "priority", []string{"priority"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var models, responses int
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("chatgpt-account-id") != "acct_1" {
					t.Errorf("unsigned Codex request: %s", r.URL)
				}
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/backend-api/codex/models":
					models++
					return jsonResponse(catalog), nil
				case r.Method == http.MethodPost && r.URL.Path == "/backend-api/codex/responses":
					responses++
					checkServiceTierOnWire(t, r, "codex", tc.slug, tc.want)
					return jsonResponse(codexSSE), nil
				default:
					return nil, fmt.Errorf("unexpected request: %s %s", r.Method, r.URL)
				}
			})}
			router := llm.NewMultiPass(nil, llm.WithHTTPClient(client), llm.WithLogin("codex", staticLogin{}))
			req := &llm.CompletionRequest{Model: "codex/" + tc.slug, ServiceTier: tc.requested, Fast: true, StreamMode: llm.Buffered}
			prepared, notes, err := llm.Prepare(t.Context(), router, req, false)
			if err != nil || models != 1 || prepared.Capabilities == nil {
				t.Fatalf("Prepare: catalog reads=%d, err=%v, request=%+v", models, err, prepared)
			}
			if tiers := prepared.Capabilities.ServiceTiers; !slices.Equal(tiers, tc.advertised) || (tiers == nil) != (tc.advertised == nil) {
				t.Errorf("catalog service tiers = %#v, want %#v", tiers, tc.advertised)
			}
			if prepared.ServiceTier != tc.want || (tc.want == "" && prepared.Fast) {
				t.Errorf("prepared tier=%q, Fast=%v; want tier=%q", prepared.ServiceTier, prepared.Fast, tc.want)
			}
			if (tc.want == "") != slices.ContainsFunc(notes, func(n llm.RequestAdaptation) bool { return n.Feature == "fast" }) {
				t.Errorf("requested %q, want %q; adaptation notes=%v", tc.requested, tc.want, notes)
			}
			if _, err := contract.Complete(t.Context(), router, prepared); err != nil || responses != 1 {
				t.Fatalf("responses requests=%d, err=%v", responses, err)
			}
		})
	}
}

func checkServiceTierOnWire(t *testing.T, req *http.Request, provider, slug, want string) {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		t.Error(err)
		return
	}
	tier, present := body["service_tier"]
	if present != (want != "") || (present && tier != want) || body["model"] != slug {
		t.Errorf("POST %s body=%v; want model=%q tier=%q", req.URL, body, slug, want)
	}
	if provider == "codex" {
		hint := "model=" + slug
		if want != "" {
			hint += ";tier=" + want
		}
		if got := req.Header.Get("x-codex-routing-hint"); got != hint {
			t.Errorf("routing hint = %q, want %q", got, hint)
		}
	}
}
