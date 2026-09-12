package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"
)

// ModelTarget identifies an inference destination. APIKey is never serialized.
type ModelTarget struct {
	Provider string `json:"provider"`
	BaseURL  string `json:"baseURL,omitempty"`
	Model    string `json:"model,omitempty"`
	Host     string `json:"host,omitempty"`
	APIKey   string `json:"-"`
	// UseConfiguredKey bypasses the process override for a credential-clear preview.
	UseConfiguredKey bool `json:"-"`
}

// ModelCapabilities contains advertised facts; nil means unknown, not false.
// A non-nil modalities/parameters list is an authoritative complete list.
type ModelCapabilities struct {
	Chat                     *bool           `json:"chat,omitempty"`
	InputModalities          []string        `json:"inputModalities"`
	OutputModalities         []string        `json:"outputModalities"`
	Tools                    *bool           `json:"tools,omitempty"`
	StructuredOutput         *bool           `json:"structuredOutput,omitempty"`
	Reasoning                *bool           `json:"reasoning,omitempty"`
	ReasoningEfforts         []string        `json:"reasoningEfforts"`
	ReasoningEffortsComplete bool            `json:"reasoningEffortsComplete,omitempty"`
	Sampling                 map[string]any  `json:"sampling,omitempty"`
	ReasoningOptions         map[string]any  `json:"reasoningOptions,omitempty"`
	ImageConstraints         map[string]any  `json:"imageConstraints,omitempty"`
	Parameters               map[string]bool `json:"parameters,omitempty"`
	ParametersComplete       bool            `json:"parametersComplete,omitempty"`
	ContextTokens            *int            `json:"contextTokens,omitempty"`
	InputTokens              *int            `json:"inputTokens,omitempty"`
	OutputTokens             *int            `json:"outputTokens,omitempty"`
	RuntimeContextTokens     *int            `json:"runtimeContextTokens,omitempty"`
	UnlimitedLimits          map[string]bool `json:"unlimitedLimits,omitempty"`
	MaxImages                *int            `json:"maxImages,omitempty"`
}

// ModelPrice preserves the provider's amount and billing basis. An empty unit
// is unknown; zero is a valid advertised free price.
type ModelPrice struct {
	Item       string         `json:"item"`
	Amount     any            `json:"amount"`
	Currency   string         `json:"currency,omitempty"`
	Unit       string         `json:"unit,omitempty"`
	Conditions map[string]any `json:"conditions,omitempty"`
}

type ModelEndpointInfo struct {
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	Status string `json:"status,omitempty"`
	ModelCapabilities
	Prices      []ModelPrice    `json:"prices,omitempty"`
	Pricing     map[string]any  `json:"pricing,omitempty"`
	PricingUnit string          `json:"pricingUnit,omitempty"`
	Performance map[string]any  `json:"performance,omitempty"`
	Raw         json.RawMessage `json:"raw,omitempty"`
}

type ModelInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	ModelCapabilities
	Endpoints []ModelEndpointInfo `json:"endpoints,omitempty"`
	// Routed prevents a catalog's aggregate limits being mistaken for host guarantees.
	// LimitsApplyToAllRoutes marks an explicit model-wide limit, not a catalog maximum.
	LimitsApplyToAllRoutes bool            `json:"limitsApplyToAllRoutes,omitempty"`
	EndpointsComplete      bool            `json:"endpointsComplete,omitempty"`
	Routed                 bool            `json:"routed,omitempty"`
	Prices                 []ModelPrice    `json:"prices,omitempty"`
	Pricing                map[string]any  `json:"pricing,omitempty"`
	PricingUnit            string          `json:"pricingUnit,omitempty"`
	Lifecycle              map[string]any  `json:"lifecycle,omitempty"`
	Raw                    json.RawMessage `json:"raw,omitempty"`
}

type ModelCatalog struct {
	Models    []ModelInfo `json:"models"`
	Source    string      `json:"source"`
	FetchedAt time.Time   `json:"fetchedAt"`
	Partial   bool        `json:"partial,omitempty"`
	Stale     bool        `json:"-"`
	Error     string      `json:"-"`
}

// ModelMetadataProvider is optional; custom LLMs without it retain unknown capabilities.
type ModelMetadataProvider interface {
	GetModelInfo(context.Context, ModelTarget) (*ModelInfo, error)
}

// ModelMetadataCache stores opaque catalog records independently of conversations.
type ModelMetadataCache interface {
	GetModelCache(context.Context, string) ([]byte, error)
	PutModelCache(context.Context, string, []byte) error
}

var ErrModelMetadataUnknown = errors.New("model metadata is unavailable")

const modelMetadataTTL = time.Hour

type metadataEntry struct {
	catalog   ModelCatalog
	attempted time.Time
	err       error
}
type modelMetadataService struct {
	mu      sync.Mutex
	entries map[string]metadataEntry
	pending map[string]chan struct{}
	cache   ModelMetadataCache
	client  *http.Client
	work    chan struct{}
}

func newModelMetadataService() *modelMetadataService {
	return &modelMetadataService{entries: map[string]metadataEntry{}, pending: map[string]chan struct{}{}, work: make(chan struct{}, 4), client: &http.Client{Timeout: 10 * time.Second}}
}
func (m *MultiPass) SetModelMetadataCache(cache ModelMetadataCache) {
	m.metadata.mu.Lock()
	defer m.metadata.mu.Unlock()
	m.metadata.cache = cache
}
func (a *Agent) SetModelMetadataCache(cache ModelMetadataCache) {
	if m, ok := a.client.(*MultiPass); ok {
		m.SetModelMetadataCache(cache)
	}
}

func (m *MultiPass) metadataTarget(t ModelTarget) (ModelTarget, providerSpec, error) {
	if t.Provider == "" {
		t.Provider, t.Model, _ = strings.Cut(t.Model, "/")
	}
	t.Provider = strings.ToLower(t.Provider)
	spec, ok := m.providers[t.Provider]
	if !ok || spec.metadata == nil {
		return t, spec, ErrModelMetadataUnknown
	}
	if t.APIKey == "" {
		if t.UseConfiguredKey {
			m.apiKeyMu.RLock()
			t.APIKey = m.apiKeys[t.Provider]
			m.apiKeyMu.RUnlock()
		} else {
			t.APIKey = m.apiKey(t.Provider)
		}
	}
	if t.BaseURL == "" {
		if t.APIKey == "" && t.Provider != "huggingface" && t.Provider != "openrouter" && t.Provider != "ollama" {
			return t, spec, ErrModelMetadataUnknown
		}
		t.BaseURL = spec.defaultBaseURL
	}
	t.BaseURL = strings.TrimRight(t.BaseURL, "/")
	if t.Provider == "huggingface" {
		if model, host, ok := strings.Cut(t.Model, ":"); ok {
			t.Model = model
			t.Host = host
		}
	}
	return t, spec, nil
}
func (m *MultiPass) ListModels(ctx context.Context, t ModelTarget, refresh bool) (ModelCatalog, error) {
	t.Model = ""
	t.Host = ""
	return m.modelMetadata(ctx, t, refresh)
}
func (m *MultiPass) LookupModel(ctx context.Context, t ModelTarget, refresh bool) (ModelCatalog, error) {
	return m.modelMetadata(ctx, t, refresh)
}
func (a *Agent) ListModels(ctx context.Context, t ModelTarget, refresh bool) (ModelCatalog, error) {
	if m, ok := a.client.(*MultiPass); ok {
		return m.ListModels(ctx, t, refresh)
	}
	return ModelCatalog{}, ErrModelMetadataUnknown
}
func (a *Agent) LookupModel(ctx context.Context, t ModelTarget, refresh bool) (ModelCatalog, error) {
	if m, ok := a.client.(*MultiPass); ok {
		return m.LookupModel(ctx, t, refresh)
	}
	if m, ok := a.client.(ModelMetadataProvider); ok {
		info, err := m.GetModelInfo(ctx, t)
		if info != nil {
			return ModelCatalog{Models: []ModelInfo{*info}}, err
		}
		return ModelCatalog{}, err
	}
	return ModelCatalog{}, ErrModelMetadataUnknown
}
func (m *MultiPass) GetModelInfo(ctx context.Context, t ModelTarget) (*ModelInfo, error) {
	cat, err := m.LookupModel(ctx, t, false)
	if len(cat.Models) > 0 {
		info := cat.Models[0]
		return &info, nil
	}
	return nil, err
}
func cloneCatalog(c ModelCatalog) ModelCatalog {
	raw, _ := json.Marshal(c)
	var out ModelCatalog
	_ = json.Unmarshal(raw, &out)
	out.Stale = c.Stale
	out.Error = c.Error
	return out
}
func (m *MultiPass) modelMetadata(ctx context.Context, t ModelTarget, force bool) (ModelCatalog, error) {
	t, spec, err := m.metadataTarget(t)
	if err != nil {
		return ModelCatalog{}, err
	}
	keyBytes, _ := json.Marshal(t)
	hash := sha256.Sum256(append(append(keyBytes, 0), []byte(t.APIKey)...))
	key := hex.EncodeToString(hash[:])
	s := m.metadata
	for {
		s.mu.Lock()
		e, found := s.entries[key]
		cache := s.cache
		if !found && cache != nil {
			s.mu.Unlock()
			raw, readErr := cache.GetModelCache(ctx, key)
			s.mu.Lock()
			if _, exists := s.entries[key]; !exists && readErr == nil {
				var c ModelCatalog
				if len(raw) <= 16<<20 && json.Unmarshal(raw, &c) == nil && !c.FetchedAt.IsZero() {
					s.entries[key] = metadataEntry{catalog: c}
				}
			}
			e, found = s.entries[key]
		}
		fresh := found && !e.catalog.FetchedAt.IsZero() && time.Since(e.catalog.FetchedAt) < modelMetadataTTL
		cooldown := found && e.err != nil && time.Since(e.attempted) < time.Minute
		if !force && (fresh || cooldown) {
			s.mu.Unlock()
			c := cloneCatalog(e.catalog)
			c.Stale = !fresh
			if e.err != nil {
				c.Error = e.err.Error()
			}
			if len(c.Models) > 0 {
				return c, nil
			}
			return c, e.err
		}
		if done := s.pending[key]; done != nil {
			s.mu.Unlock()
			if !force && len(e.catalog.Models) > 0 {
				c := cloneCatalog(e.catalog)
				c.Stale = true
				return c, nil
			}
			select {
			case <-ctx.Done():
				return ModelCatalog{}, ctx.Err()
			case <-done:
				force = false
				continue
			}
		}
		done := make(chan struct{})
		s.pending[key] = done
		s.mu.Unlock()
		fetch := func(fetchCtx context.Context) (ModelCatalog, error) {
			bounded, cancel := context.WithTimeout(fetchCtx, 10*time.Second)
			defer cancel()
			var cat ModelCatalog
			var fetchErr error
			select {
			case s.work <- struct{}{}:
				cat, fetchErr = spec.metadata(bounded, s.client, t)
				<-s.work
			case <-bounded.Done():
				fetchErr = bounded.Err()
			}
			cat.Source = metadataSource(t.BaseURL)
			cat.FetchedAt = time.Now()
			s.mu.Lock()
			old := s.entries[key]
			if fetchErr != nil && len(old.catalog.Models) > 0 {
				cat = old.catalog
			} else if fetchErr != nil {
				cat.Partial = true
			}
			// Caller cancellation is not a cached discovery failure.
			if !errors.Is(fetchErr, context.Canceled) {
				s.entries[key] = metadataEntry{catalog: cat, attempted: time.Now(), err: fetchErr}
			}
			writeCache := s.cache
			s.mu.Unlock()
			if fetchErr == nil && writeCache != nil {
				if raw, e := json.Marshal(cat); e == nil {
					_ = writeCache.PutModelCache(bounded, key, raw)
				}
			}
			s.mu.Lock()
			delete(s.pending, key)
			close(done)
			s.mu.Unlock()
			if fetchErr != nil {
				cat.Error = fetchErr.Error()
				cat.Stale = len(cat.Models) > 0
				if len(cat.Models) > 0 {
					return cloneCatalog(cat), nil
				}
			}
			return cloneCatalog(cat), fetchErr
		}
		if !force && len(e.catalog.Models) > 0 {
			go func() { _, _ = fetch(context.WithoutCancel(ctx)) }()
			c := cloneCatalog(e.catalog)
			c.Stale = true
			return c, nil
		}
		return fetch(ctx)
	}
}

// EffectiveCapabilities resolves only facts valid for the selected route.
func (m ModelInfo) EffectiveCapabilities(host string) ModelCapabilities {
	if !m.Routed {
		return m.ModelCapabilities
	}
	var candidates []ModelCapabilities
	for _, e := range m.Endpoints {
		if host != "" {
			if e.ID == host {
				return overlayCapabilities(m.ModelCapabilities, e.ModelCapabilities, m.LimitsApplyToAllRoutes)
			}
			continue
		}
		if e.Status != "" && e.Status != "live" && e.Status != "0" {
			continue
		}
		candidates = append(candidates, overlayCapabilities(m.ModelCapabilities, e.ModelCapabilities, m.LimitsApplyToAllRoutes))
	}
	if host != "" {
		return ModelCapabilities{}
	}
	if !m.EndpointsComplete || len(candidates) == 0 {
		// Architecture is a model fact. Aggregate route capabilities and limits
		// are not guarantees when the eligible endpoint set is unknown.
		out := ModelCapabilities{Chat: m.Chat, InputModalities: m.InputModalities, OutputModalities: m.OutputModalities}
		if m.LimitsApplyToAllRoutes {
			out.ContextTokens = m.ContextTokens
			out.InputTokens = m.InputTokens
			out.OutputTokens = m.OutputTokens
			out.UnlimitedLimits = m.UnlimitedLimits
		}
		return out
	}
	out := candidates[0]
	out.UnlimitedLimits = maps.Clone(out.UnlimitedLimits)
	for _, c := range candidates[1:] {
		a, b := reflect.ValueOf(&out).Elem(), reflect.ValueOf(c)
		for i := 0; i < a.NumField(); i++ {
			x, y := a.Field(i), b.Field(i)
			key := strings.Split(a.Type().Field(i).Tag.Get("json"), ",")[0]
			if key == "unlimitedLimits" {
				continue
			}
			if x.Type() == reflect.TypeFor[*int]() {
				n, unlimited := commonModelLimit(x.Interface().(*int), y.Interface().(*int), out.UnlimitedLimits[key], c.UnlimitedLimits[key])
				x.Set(reflect.ValueOf(n))
				if unlimited {
					if out.UnlimitedLimits == nil {
						out.UnlimitedLimits = map[string]bool{}
					}
					out.UnlimitedLimits[key] = true
				} else {
					delete(out.UnlimitedLimits, key)
				}
			} else if !reflect.DeepEqual(x.Interface(), y.Interface()) {
				x.SetZero()
			}
		}
	}
	// Different complete parameter sets are uncertain, not an empty supported set.
	if out.Parameters == nil {
		out.ParametersComplete = false
	}
	if out.ReasoningEfforts == nil {
		out.ReasoningEffortsComplete = false
	}
	return out
}
func overlayCapabilities(model, endpoint ModelCapabilities, sharedLimits bool) ModelCapabilities {
	// Routed catalog limits and parameter unions describe available options,
	// not every endpoint. Route-specific declarations must establish them.
	if !sharedLimits {
		model.UnlimitedLimits = nil
		model.ContextTokens = nil
		model.InputTokens = nil
		model.OutputTokens = nil
		model.RuntimeContextTokens = nil
	}
	model.Tools = nil
	model.StructuredOutput = nil
	model.Reasoning = nil
	model.Parameters = nil
	model.ParametersComplete = false
	model.ReasoningEfforts = nil
	model.ReasoningEffortsComplete = false
	a, b := reflect.ValueOf(&model).Elem(), reflect.ValueOf(endpoint)
	for i := 0; i < a.NumField(); i++ {
		x := b.Field(i)
		if !x.IsZero() {
			a.Field(i).Set(x)
		}
	}
	return model
}
func (c ModelCapabilities) ContextWindow() int {
	n := 0
	for _, p := range []*int{c.ContextTokens, c.InputTokens, c.RuntimeContextTokens} {
		if p != nil && *p > 0 && (n == 0 || *p < n) {
			n = *p
		}
	}
	return n
}

// ModelMetadataIdentity is an opaque scope token used to fence asynchronous UI reads.
func (a *Agent) ModelMetadataIdentity(t ModelTarget) string {
	m, ok := a.client.(*MultiPass)
	if !ok {
		return ""
	}
	t, _, err := m.metadataTarget(t)
	if err != nil {
		return ""
	}
	raw, _ := json.Marshal(t)
	sum := sha256.Sum256(append(append(raw, 0), []byte(t.APIKey)...))
	return hex.EncodeToString(sum[:])
}

func metadataSource(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		return "custom endpoint"
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func commonModelLimit(a, b *int, au, bu bool) (*int, bool) {
	if au && bu {
		return nil, true
	}
	if au {
		return b, false
	}
	if bu {
		return a, false
	}
	if a == nil || b == nil {
		return nil, false
	}
	n := min(*a, *b)
	return &n, false
}
