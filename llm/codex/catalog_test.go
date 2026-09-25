package codex

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
)

// catalogJSON is shaped like the backend's answer on 2026-09-25: a hidden
// row, a retiring model with its successor, reasoning levels beyond what
// api.openai.com takes, and service tiers on the rows that name them.
const catalogJSON = `{"models":[
  {"slug":"gpt-6-sol","display_name":"GPT-6-Sol","description":"Frontier.","context_window":272000,"max_output_tokens":128000,"supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"},{"effort":"xhigh"},{"effort":"max"},{"effort":"ultra"}],"default_reasoning_level":"medium","visibility":"list","minimal_client_version":"0.155.0","upgrade":null,"service_tiers":[{"id":"priority","name":"Fast","description":"1.5x speed"}],"additional_speed_tiers":["fast"]},
  {"slug":"gpt-5.5","display_name":"GPT-5.5","context_window":272000,"supported_reasoning_levels":["low","medium","high"],"input_modalities":["text","image"],"visibility":"list","upgrade":{"model":"gpt-5.6-sol","retirement_at":"2026-10-14T19:00:00Z"},"service_tiers":[],"additional_speed_tiers":[]},
  {"slug":"gpt-reserve","display_name":"GPT-Reserve","context_window":272000,"visibility":"hide"},
  "not a record"
]}`

// catalogServer answers the catalog endpoint, checking the request is
// signed, and records how many times it was asked.
func catalogServer(t *testing.T, status int, body string) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/backend-api/codex/models" || r.URL.Query().Get("client_version") != ProtocolVersion {
			t.Errorf("request %s", r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer tok-1" || r.Header.Get("chatgpt-account-id") != "acct_1" || r.Header.Get("originator") != "polly" || r.Header.Get("Accept") != "application/json" {
			t.Errorf("headers %v", r.Header)
		}
		if _, sent := r.Header["Session-Id"]; sent {
			t.Error("catalog request carries a session id")
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

func TestListModelsDecodesTheBackendCatalog(t *testing.T) {
	server, calls := catalogServer(t, 200, catalogJSON)
	target := contract.ModelTarget{Provider: "codex", BaseURL: server.URL + "/backend-api/codex"}
	cat, err := ListModels(context.Background(), server.Client(), target, newFakeLogin())
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 1 || len(cat.Models) != 2 || !cat.Partial {
		t.Fatalf("catalog = %+v after %d calls", cat, *calls)
	}
	five, sol := cat.Models[0], cat.Models[1]
	if five.ID != "gpt-5.5" || sol.ID != "gpt-6-sol" {
		t.Fatalf("order = %s, %s", five.ID, sol.ID)
	}
	if sol.Name != "GPT-6-Sol" || sol.Description != "Frontier." || *sol.ContextTokens != 272000 || *sol.OutputTokens != 128000 || !*sol.Chat || !*sol.Tools || !*sol.Reasoning {
		t.Fatalf("sol = %+v", sol)
	}
	if strings.Join(sol.ReasoningEfforts, ",") != "low,medium,high,xhigh,max,ultra" || !sol.ReasoningEffortsComplete || *sol.ReasoningDefaultEffort != "medium" {
		t.Fatalf("sol reasoning = %+v", sol.ModelCapabilities)
	}
	if five.Lifecycle["shutdown_date"] != "2026-10-14T19:00:00Z" || five.Lifecycle["successor"] != "gpt-5.6-sol" || len(sol.Lifecycle) != 0 {
		t.Fatalf("lifecycle: 5.5 %v, sol %v", five.Lifecycle, sol.Lifecycle)
	}
	if strings.Join(sol.InputModalities, ",") != "image,text" || strings.Join(five.InputModalities, ",") != "image,text" || five.OutputTokens != nil {
		t.Fatalf("modalities: sol %v, 5.5 %v (%v)", sol.InputModalities, five.InputModalities, five.OutputTokens)
	}
	if strings.Join(five.ReasoningEfforts, ",") != "low,medium,high" {
		t.Fatalf("5.5 reasoning = %v", five.ReasoningEfforts)
	}
	if !sol.Parameters[contract.ParameterServiceTier] || len(sol.Parameters) != 1 || five.Parameters == nil || five.Parameters[contract.ParameterServiceTier] || sol.ParametersComplete {
		t.Fatalf("fast tier: sol %v, 5.5 %v", sol.Parameters, five.Parameters)
	}

	target.Model = "gpt-5.5"
	cat, err = ListModels(context.Background(), server.Client(), target, newFakeLogin())
	if err != nil || len(cat.Models) != 1 || cat.Models[0].ID != "gpt-5.5" {
		t.Fatalf("named lookup = %+v, %v", cat, err)
	}
	target.Model = "gpt-4"
	if _, err := ListModels(context.Background(), server.Client(), target, newFakeLogin()); !errors.Is(err, contract.ErrModelMetadataUnknown) {
		t.Fatalf("unknown model: %v", err)
	}
	// A hidden model is left off the listing but answers to its name.
	target.Model = "gpt-reserve"
	if cat, err := ListModels(context.Background(), server.Client(), target, newFakeLogin()); err != nil || len(cat.Models) != 1 || cat.Models[0].ID != "gpt-reserve" || cat.Models[0].Parameters != nil {
		t.Fatalf("hidden model by name = %+v, %v", cat, err)
	}
}

func TestFastTierReadsBothCatalogShapes(t *testing.T) {
	for _, tc := range []struct {
		record      map[string]any
		fast, known bool
	}{
		{map[string]any{"service_tiers": []any{map[string]any{"id": "priority"}}}, true, true},
		{map[string]any{"service_tiers": []any{map[string]any{"id": "flex"}}}, false, true},
		{map[string]any{"additional_speed_tiers": []any{"fast"}}, true, true},
		{map[string]any{"service_tiers": []any{}, "additional_speed_tiers": []any{}}, false, true},
		{map[string]any{"slug": "gpt-5.5"}, false, false},
	} {
		if fast, known := fastTier(tc.record); fast != tc.fast || known != tc.known {
			t.Errorf("fastTier(%v) = %v, %v; want %v, %v", tc.record, fast, known, tc.fast, tc.known)
		}
	}
}

func TestListModelsFallsBackToKnownModels(t *testing.T) {
	server, _ := catalogServer(t, 500, "down")
	target := contract.ModelTarget{Provider: "codex", BaseURL: server.URL + "/backend-api/codex"}
	cat, err := ListModels(context.Background(), server.Client(), target, newFakeLogin())
	if err == nil || errors.Is(err, contract.ErrModelMetadataUnknown) || !cat.Partial || len(cat.Models) != len(knownModels) {
		t.Fatalf("catalog = %+v, err = %v", cat, err)
	}
	for i, m := range knownModels {
		if cat.Models[i].ID != m.id || *cat.Models[i].ContextTokens != knownContextTokens || cat.Models[i].OutputTokens != nil {
			t.Fatalf("model %d = %+v, want %+v", i, cat.Models[i], m)
		}
	}
	target.Model = "gpt-5.5"
	cat, err = ListModels(context.Background(), server.Client(), target, newFakeLogin())
	if err == nil || len(cat.Models) != 1 || cat.Models[0].ID != "gpt-5.5" || !cat.Partial {
		t.Fatalf("named fallback = %+v, %v", cat, err)
	}
	target.Model = "gpt-4"
	if _, err := ListModels(context.Background(), server.Client(), target, newFakeLogin()); !errors.Is(err, contract.ErrModelMetadataUnknown) {
		t.Fatalf("unknown model while down: %v", err)
	}

	// A catalog shaped differently from what this package reads is the
	// same as none.
	odd, _ := catalogServer(t, 200, `{"data":[{"id":"gpt-5.5"}]}`)
	target = contract.ModelTarget{Provider: "codex", BaseURL: odd.URL + "/backend-api/codex"}
	if cat, err := ListModels(context.Background(), odd.Client(), target, newFakeLogin()); err == nil || !cat.Partial || len(cat.Models) != len(knownModels) {
		t.Fatalf("odd catalog = %+v, %v", cat, err)
	}
}

func TestListModelsNeedsASignIn(t *testing.T) {
	server, calls := catalogServer(t, 200, catalogJSON)
	target := contract.ModelTarget{Provider: "codex", BaseURL: server.URL + "/backend-api/codex"}
	login := newFakeLogin()
	login.signedOut = true
	if _, err := ListModels(context.Background(), server.Client(), target, login); !errors.Is(err, contract.ErrModelMetadataUnknown) {
		t.Fatalf("signed out: %v", err)
	}
	if _, err := ListModels(context.Background(), server.Client(), target, nil); !errors.Is(err, contract.ErrModelMetadataUnknown) {
		t.Fatalf("no login: %v", err)
	}
	if *calls != 0 {
		t.Fatalf("backend asked %d times without a sign-in", *calls)
	}
}
