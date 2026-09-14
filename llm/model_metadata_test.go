package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestProviderMetadataFixtures(t *testing.T) {
	cases := []struct {
		provider, list, detail, model string
		check                         func(*testing.T, ModelInfo)
	}{
		{"openai", `{"data":[{"id":"m","owned_by":"org"},{"id":"m"}]}`, `{"id":"m","owned_by":"org"}`, "m", func(t *testing.T, m ModelInfo) {
			if m.Tools != nil || m.InputModalities != nil || m.ContextTokens != nil {
				t.Fatal("invented OpenAI capabilities")
			}
		}},
		{"deepseek", `{"data":[{"id":"m","owned_by":"deepseek"}]}`, "", "m", func(t *testing.T, m ModelInfo) {
			if m.Tools != nil || m.ContextTokens != nil {
				t.Fatal("invented DeepSeek capabilities")
			}
		}},
		{"anthropic", `{"data":[{"id":"m"}],"has_more":true,"last_id":"m"}`, `{"id":"m","display_name":"Claude","max_input_tokens":200000,"max_tokens":64000,"capabilities":{"image_input":{"supported":false},"structured_outputs":{"supported":true},"thinking":{"supported":true,"types":{"adaptive":{"supported":true}}},"effort":{"supported":true,"low":{"supported":true},"high":{"supported":false}}}}`, "m", func(t *testing.T, m ModelInfo) {
			if m.ContextWindow() != 200000 || m.StructuredOutput == nil || !*m.StructuredOutput || !reflect.DeepEqual(m.InputModalities, []string{"text"}) || len(m.ReasoningEfforts) != 1 {
				t.Fatalf("bad Anthropic: %+v", m)
			}
		}},
		{"gemini", `{"models":[{"name":"models/m"}],"nextPageToken":"page2"}`, `{"name":"models/m","displayName":"Gemini","inputTokenLimit":1000000,"outputTokenLimit":64000,"supportedGenerationMethods":["generateContent"],"thinking":false,"temperature":1,"maxTemperature":2,"topK":64}`, "m", func(t *testing.T, m ModelInfo) {
			if m.ContextWindow() != 1000000 || m.Chat == nil || !*m.Chat || m.Reasoning == nil || *m.Reasoning || m.Sampling["topK"] != float64(64) {
				t.Fatalf("bad Gemini: %+v", m)
			}
		}},
		{"ollama", `{"models":[{"name":"m","size":123}]}`, `{"capabilities":["completion","tools"],"model_info":{"general.architecture":"llama","llama.context_length":131072},"parameters":"num_ctx 8192\ntemperature 0.7"}`, "m", func(t *testing.T, m ModelInfo) {
			if m.ContextWindow() != 8192 || *m.ContextTokens != 131072 || m.Tools == nil || !*m.Tools {
				t.Fatalf("bad Ollama: %+v", m)
			}
		}},
		{"huggingface", `{"data":[{"id":"org/m","architecture":{"input_modalities":["text"],"output_modalities":["text"]}}]}`, `{"id":"org/m","architecture":{"input_modalities":["text"],"output_modalities":["text"]},"providers":[{"provider":"host","status":"live","context_length":65536,"supports_tools":false,"pricing":{"input":0,"output":1.2},"throughput":44}]}`, "org/m", func(t *testing.T, m ModelInfo) {
			c := m.EffectiveCapabilities("host")
			if c.ContextWindow() != 65536 || c.Tools == nil || *c.Tools || m.Endpoints[0].PricingUnit != "USD per million tokens" || m.Endpoints[0].Pricing["input"] != float64(0) || !m.EndpointsComplete {
				t.Fatalf("bad HF: %+v", m)
			}
		}},
		{"openrouter", `{"data":[{"id":"org/m","context_length":1000000,"architecture":{"input_modalities":["text","image"],"output_modalities":["text"]},"supported_parameters":["tools","temperature"]}]}`, `{"data":{"endpoints":[{"tag":"host/turbo","provider_name":"Host","status":0,"context_length":32000,"max_prompt_tokens":30000,"supported_parameters":["temperature"],"pricing":{"prompt":"0.000002"},"latency_last_30m":0.2}]}}`, "org/m", func(t *testing.T, m ModelInfo) {
			c := m.EffectiveCapabilities("host/turbo")
			if c.ContextWindow() != 30000 || c.Tools == nil || *c.Tools || m.Endpoints[0].PricingUnit != "USD per token" || !m.EndpointsComplete {
				t.Fatalf("bad OR: %+v", m)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			var paths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.RequestURI())
				if !strings.HasPrefix(r.URL.Path, "/custom/") {
					t.Errorf("ignored custom base: %s", r.URL)
				}
				switch tc.provider {
				case "anthropic":
					if r.Header.Get("x-api-key") != "secret" || r.Header.Get("anthropic-version") == "" {
						t.Error("missing Anthropic auth")
					}
				case "gemini":
					if r.Header.Get("x-goog-api-key") != "secret" {
						t.Error("missing Gemini auth")
					}
				default:
					if r.Header.Get("Authorization") != "Bearer secret" {
						t.Error("missing auth")
					}
				}
				if r.URL.RawQuery != "" {
					if tc.provider == "anthropic" {
						fmt.Fprint(w, `{"data":[{"id":"m"},{"id":"z"}],"has_more":false}`)
					} else {
						fmt.Fprint(w, `{"models":[{"name":"models/m"},{"name":"models/z"}]}`)
					}
					return
				}
				if r.URL.Path == "/custom/models" || r.URL.Path == "/custom/api/tags" {
					fmt.Fprint(w, tc.list)
					return
				}
				if tc.provider == "ollama" {
					var b map[string]string
					_ = json.NewDecoder(r.Body).Decode(&b)
					if r.Method != "POST" || b["model"] != tc.model {
						t.Error("bad show request")
					}
				}
				fmt.Fprint(w, tc.detail)
			}))
			defer server.Close()
			m := NewMultiPass(map[string]string{tc.provider: "secret"})
			target := ModelTarget{Provider: tc.provider, BaseURL: server.URL + "/custom", Model: tc.model}
			cat, err := m.ListModels(context.Background(), target, false)
			if err != nil || len(cat.Models) == 0 {
				t.Fatalf("list: %+v %v", cat, err)
			}
			want := 1
			if tc.provider == "anthropic" || tc.provider == "gemini" {
				want = 2
			}
			if len(cat.Models) != want {
				t.Fatalf("pagination/dedupe: %d", len(cat.Models))
			}
			detail, err := m.LookupModel(context.Background(), target, false)
			if err != nil || len(detail.Models) != 1 {
				t.Fatalf("detail: %+v %v", detail, err)
			}
			tc.check(t, detail.Models[0])
			if len(paths) < 2 {
				t.Fatal("detail never fetched")
			}
		})
	}
}

func TestAutomaticCapabilitiesRequireCompleteCoverage(t *testing.T) {
	n := 100
	small := 50
	model := ModelInfo{Routed: true, EndpointsComplete: true, ModelCapabilities: ModelCapabilities{ContextTokens: &n, Tools: truth(true)}, Endpoints: []ModelEndpointInfo{
		{ID: "a", Status: "live", ModelCapabilities: ModelCapabilities{ContextTokens: &n, Tools: truth(false), Parameters: map[string]bool{"temperature": true}, ParametersComplete: true}},
		{ID: "b", Status: "live", ModelCapabilities: ModelCapabilities{ContextTokens: &small, Tools: truth(false), Parameters: map[string]bool{"tools": true}, ParametersComplete: true}},
	}}
	c := model.EffectiveCapabilities("")
	if c.ContextWindow() != 50 || c.Tools == nil || *c.Tools || c.ParametersComplete {
		t.Fatalf("common facts: %+v", c)
	}
	model.Endpoints[1].ContextTokens = nil
	model.Endpoints[1].Tools = nil
	c = model.EffectiveCapabilities("")
	if c.ContextWindow() != 0 || c.Tools != nil {
		t.Fatalf("incomplete route inferred: %+v", c)
	}
	model.EndpointsComplete = false
	if model.EffectiveCapabilities("").ContextWindow() != 0 {
		t.Fatal("partial endpoints clamped")
	}
	if model.EffectiveCapabilities("missing").Tools != nil {
		t.Fatal("unknown pin inferred")
	}
	// An explicit empty modalities list must survive cache serialization.
	model.InputModalities = []string{}
	copy := cloneCatalog(ModelCatalog{Models: []ModelInfo{model}})
	if copy.Models[0].InputModalities == nil {
		t.Fatal("empty and unknown collapsed")
	}
}

type fixtureMetadataCache struct {
	mu      sync.Mutex
	records map[string][]byte
}

func (c *fixtureMetadataCache) GetModelCache(_ context.Context, k string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.records[k]...), nil
}
func (c *fixtureMetadataCache) PutModelCache(_ context.Context, k string, v []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records[k] = append([]byte(nil), v...)
	return nil
}

func TestMetadataCacheScopePersistenceAndCoalescing(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	started := make(chan struct{}, 20)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		started <- struct{}{}
		<-release
		fmt.Fprint(w, `{"id":"m"}`)
	}))
	defer server.Close()
	cache := &fixtureMetadataCache{records: map[string][]byte{}}
	m := NewMultiPass(map[string]string{"openai": "first"})
	m.SetModelMetadataCache(cache)
	target := ModelTarget{Provider: "openai", Model: "m", BaseURL: server.URL}
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			_, err := m.LookupModel(context.Background(), target, false)
			if err != nil {
				t.Error(err)
			}
		})
	}
	<-started
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("not coalesced: %d", calls.Load())
	}
	next := NewMultiPass(map[string]string{"openai": "first"})
	next.SetModelMetadataCache(cache)
	if _, err := next.LookupModel(context.Background(), target, false); err != nil || calls.Load() != 1 {
		t.Fatal("persistent cache missed")
	}
	for _, raw := range cache.records {
		if strings.Contains(string(raw), "first") {
			t.Fatal("credential persisted")
		}
	}
	next.SetAPIKey("openai", "second")
	_, _ = next.LookupModel(context.Background(), target, false)
	target.Host = "different"
	_, _ = next.LookupModel(context.Background(), target, false)
	target.BaseURL = server.URL + "/other"
	_, _ = next.LookupModel(context.Background(), target, false)
	if calls.Load() != 4 {
		t.Fatalf("scope contamination: %d", calls.Load())
	}
}

func TestMetadataStaleRefreshFailureAndCancellation(t *testing.T) {
	var calls atomic.Int32
	var fail atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if fail.Load() {
			http.Error(w, "secret error", 503)
			return
		}
		fmt.Fprint(w, `{"id":"m"}`)
	}))
	defer server.Close()
	m := NewMultiPass(nil)
	target := ModelTarget{Provider: "openai", Model: "m", BaseURL: server.URL}
	cat, err := m.LookupModel(context.Background(), target, false)
	if err != nil {
		t.Fatal(err)
	}
	m.metadata.mu.Lock()
	for k, e := range m.metadata.entries {
		e.catalog.FetchedAt = time.Now().Add(-2 * time.Hour)
		m.metadata.entries[k] = e
	}
	m.metadata.mu.Unlock()
	fail.Store(true)
	cat, err = m.LookupModel(context.Background(), target, false)
	if err != nil || !cat.Stale || len(cat.Models) != 1 {
		t.Fatalf("stale read: %+v %v", cat, err)
	}
	cat, err = m.LookupModel(context.Background(), target, true)
	if err != nil || cat.Error == "" || !cat.Stale {
		t.Fatalf("stale failure: %+v %v", cat, err)
	}
	before := calls.Load()
	_, _ = m.LookupModel(context.Background(), target, false)
	if calls.Load() != before {
		t.Fatal("failure cooldown bypassed")
	}
	if strings.Contains(cat.Error, "secret") {
		t.Fatal("server error leaked")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	target.Model = "cancelled"
	if _, err := m.LookupModel(ctx, target, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}

func TestMetadataFailureRetriesAfterCooldown(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `{"id":"m","name":"recovered"}`)
	}))
	defer server.Close()
	cache := &fixtureMetadataCache{records: map[string][]byte{}}
	m := NewMultiPass(nil)
	m.SetModelMetadataCache(cache)
	target := ModelTarget{Provider: "openai", Model: "m", BaseURL: server.URL}
	for range 2 {
		cat, err := m.LookupModel(context.Background(), target, false)
		if err == nil || !cat.FetchedAt.IsZero() || len(cat.Models) != 0 || !cat.Partial {
			t.Fatalf("failed discovery became fresh: %+v, %v", cat, err)
		}
	}
	if calls.Load() != 1 || len(cache.records) != 0 {
		t.Fatal("failed discovery bypassed cooldown or entered persistent cache")
	}
	m.metadata.mu.Lock()
	for key, entry := range m.metadata.entries {
		entry.attempted = time.Now().Add(-2 * time.Minute)
		m.metadata.entries[key] = entry
	}
	m.metadata.mu.Unlock()
	for range 2 {
		cat, err := m.LookupModel(context.Background(), target, false)
		if err != nil || cat.Stale || cat.Error != "" || cat.FetchedAt.IsZero() || len(cat.Models) != 1 || cat.Models[0].Name != "recovered" {
			t.Fatalf("discovery did not recover: %+v, %v", cat, err)
		}
	}
	if calls.Load() != 2 || len(cache.records) != 1 {
		t.Fatal("recovered discovery was not cached")
	}
}

func TestMetadataFailedRefreshRetainsSuccessUntilRetry(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			fmt.Fprint(w, `{"id":"m","name":"original"}`)
		case 2:
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		default:
			fmt.Fprint(w, `{"id":"m","name":"refreshed"}`)
		}
	}))
	defer server.Close()
	m := NewMultiPass(nil)
	target := ModelTarget{Provider: "openai", Model: "m", BaseURL: server.URL}
	original, err := m.LookupModel(context.Background(), target, false)
	if err != nil {
		t.Fatal(err)
	}
	// A forced refresh can fail while the previous successful catalog is
	// still inside its one-hour TTL. It must use the failure cooldown too.
	for _, force := range []bool{true, false} {
		cat, err := m.LookupModel(context.Background(), target, force)
		if err != nil || !cat.Stale || cat.Error == "" || !cat.FetchedAt.Equal(original.FetchedAt) || len(cat.Models) != 1 || cat.Models[0].Name != "original" {
			t.Fatalf("failed refresh lost stale success: %+v, %v", cat, err)
		}
	}
	if calls.Load() != 2 {
		t.Fatal("failed refresh bypassed cooldown")
	}
	m.metadata.mu.Lock()
	for key, entry := range m.metadata.entries {
		entry.attempted = time.Now().Add(-2 * time.Minute)
		m.metadata.entries[key] = entry
	}
	m.metadata.mu.Unlock()
	cat, err := m.LookupModel(context.Background(), target, false)
	if err != nil || !cat.Stale || len(cat.Models) != 1 || cat.Models[0].Name != "original" {
		t.Fatalf("background retry lost stale success: %+v, %v", cat, err)
	}
	m.metadata.mu.Lock()
	var pending []chan struct{}
	for _, done := range m.metadata.pending {
		pending = append(pending, done)
	}
	m.metadata.mu.Unlock()
	for _, done := range pending {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("metadata retry did not finish")
		}
	}
	cat, err = m.LookupModel(context.Background(), target, false)
	if err != nil || cat.Stale || cat.Error != "" || len(cat.Models) != 1 || cat.Models[0].Name != "refreshed" || calls.Load() != 3 {
		t.Fatalf("failed refresh did not retry after cooldown: %+v, %v, calls=%d", cat, err, calls.Load())
	}
}

func TestMetadataCancellationDoesNotDelayRetry(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
			<-r.Context().Done()
			return
		}
		fmt.Fprint(w, `{"id":"m"}`)
	}))
	defer server.Close()
	m := NewMultiPass(nil)
	target := ModelTarget{Provider: "openai", Model: "m", BaseURL: server.URL}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := m.LookupModel(ctx, target, false)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("metadata request did not start")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled discovery: %v", err)
	}
	cat, err := m.LookupModel(context.Background(), target, false)
	if err != nil || cat.Stale || len(cat.Models) != 1 || calls.Load() != 2 {
		t.Fatalf("cancellation delayed retry: %+v, %v, calls=%d", cat, err, calls.Load())
	}
}

func TestMetadataPaginationFailureReturnsPartial(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			http.Error(w, "failed", 503)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"m"}],"has_more":true,"last_id":"m"}`)
	}))
	defer server.Close()
	m := NewMultiPass(nil)
	cat, _ := m.ListModels(context.Background(), ModelTarget{Provider: "anthropic", BaseURL: server.URL}, false)
	if len(cat.Models) != 1 || !cat.Partial || cat.Error == "" {
		t.Fatalf("lost partial data: %+v", cat)
	}
}
