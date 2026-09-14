package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
)

// ModelMetadataProvider is optional; custom LLMs without it retain unknown capabilities.
type ModelMetadataProvider interface {
	GetModelInfo(context.Context, ModelTarget) (*ModelInfo, error)
}

// ModelMetadataCache stores opaque catalog records independently of conversations.
type ModelMetadataCache interface {
	GetModelCache(context.Context, string) ([]byte, error)
	PutModelCache(context.Context, string, []byte) error
}

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
	t, _, err := m.metadataTarget(t)
	if err != nil {
		return ModelCatalog{}, err
	}
	detail, detailErr := m.modelMetadata(ctx, t, refresh)
	if t.Provider != "openrouter" || t.Model == "" {
		return detail, detailErr
	}
	// These are separate cached reads, outside the fetch semaphore. Endpoint
	// responses omit model-wide policy; absence must not erase catalog facts.
	catalog, _ := m.ListModels(ctx, t, refresh)
	return mergeOpenRouterCatalog(detail, catalog, t.Model), openRouterDetailError(detail, catalog, t.Model, detailErr)
}

func openRouterDetailError(detail, catalog ModelCatalog, model string, err error) error {
	if len(detail.Models) > 0 {
		return err
	}
	for _, info := range catalog.Models {
		if info.ID == model {
			return nil
		}
	}
	return err
}

func mergeOpenRouterCatalog(detail, catalog ModelCatalog, model string) ModelCatalog {
	for _, info := range catalog.Models {
		if info.ID != model {
			continue
		}
		if len(detail.Models) == 0 {
			catalog.Models = []ModelInfo{info}
			catalog.Partial = true
			return catalog
		}
		// Catalog policy provides defaults; explicit endpoint policy wins.
		policy := info.ModelCapabilities
		for i := range detail.Models {
			contract.MergeReasoningPolicy(&detail.Models[i].ModelCapabilities, policy)
		}
		detail.Stale = detail.Stale || catalog.Stale
		break
	}
	return detail
}

// CachedModelInfo reads only in-memory metadata. It neither waits for a fetch
// nor accesses storage/network, so completion and settings rendering stay fast.
func (a *Agent) CachedModelInfo(t ModelTarget) *ModelInfo {
	m, ok := a.client.(*MultiPass)
	if !ok {
		return nil
	}
	t, _, err := m.metadataTarget(t)
	if err != nil {
		return nil
	}
	read := func(target ModelTarget) ModelCatalog {
		m.metadata.mu.Lock()
		entry := m.metadata.entries[metadataKey(target)]
		m.metadata.mu.Unlock()
		cat := entry.catalog
		cat.Models = nil
		for _, info := range entry.catalog.Models {
			if info.ID == t.Model {
				cat.Models = []ModelInfo{info}
				break
			}
		}
		return cloneCatalog(cat)
	}
	detail := read(t)
	if t.Provider == "openrouter" {
		catalogTarget := t
		catalogTarget.Model, catalogTarget.Host = "", ""
		detail = mergeOpenRouterCatalog(detail, read(catalogTarget), t.Model)
	}
	if len(detail.Models) == 0 {
		return nil
	}
	return &detail.Models[0]
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
	key := metadataKey(t)
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
		fresh := found && e.err == nil && !e.catalog.FetchedAt.IsZero() && time.Since(e.catalog.FetchedAt) < modelMetadataTTL
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
			if fetchErr == nil {
				cat.FetchedAt = time.Now()
			}
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

func metadataKey(t ModelTarget) string {
	keyBytes, _ := json.Marshal(t)
	if t.Provider == "openrouter" {
		keyBytes = append(keyBytes, []byte("reasoning-policy-v2")...)
	}
	hash := sha256.Sum256(append(append(keyBytes, 0), []byte(t.APIKey)...))
	return hex.EncodeToString(hash[:])
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
	return metadataKey(t)
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
