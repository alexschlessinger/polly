package llm

import (
	"context"
	"fmt"
	"github.com/alexschlessinger/pollytool/llm/anthropic"
	"github.com/alexschlessinger/pollytool/llm/deepseek"
	"github.com/alexschlessinger/pollytool/llm/gemini"
	"github.com/alexschlessinger/pollytool/llm/ollama"
	"github.com/alexschlessinger/pollytool/llm/openai"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/alexschlessinger/pollytool/messages"
)

type providerFactory func(apiKey, baseURL string) (LLM, error)

// metadataFetcher reads a provider's model catalog, or one model's record
// when the target names a model. Each provider package exports one.
type metadataFetcher func(context.Context, *http.Client, ModelTarget) (ModelCatalog, error)

// providerSpec is everything the package knows about one provider prefix:
// how to build its chat client, the endpoint used when a request names
// none, whether a request may go without a credential, and the optional
// capabilities only some providers offer. Chat routing, embeddings, and
// context-window discovery all read this one table, so a provider is
// described in exactly one place.
type providerSpec struct {
	metadata metadataFetcher
	new      providerFactory
	// defaultBaseURL fills an empty request base URL before new runs. Empty
	// lets the provider client choose its own endpoint and API mode.
	defaultBaseURL string
	// catalogBaseURL is where the model catalog lives when defaultBaseURL is
	// empty.
	catalogBaseURL string
	// nativeEndpoint marks a provider served only by its own API: a global
	// OpenAI-compatible base URL never applies to it.
	nativeEndpoint bool
	// keyless reports whether a request with the given (undefaulted) base
	// URL may proceed without an API key. nil means a key is always required.
	keyless func(baseURL string) bool
	// keylessCatalog reports that the model catalog is public.
	keylessCatalog bool
	// hostRouting accepts CompletionRequest.ModelHost to pin one route.
	hostRouting bool
	// splitHost separates a model id from the route suffix it may carry.
	splitHost func(model string) (name, host string)
	// routedCatalog marks a catalog whose named lookups return per-route
	// endpoints that omit model-wide policy, which the router merges back
	// from the listing. catalogVersion salts its cache key when the decoded
	// shape changes.
	routedCatalog  bool
	catalogVersion string
	// embed serves embedding requests; nil when the provider has none.
	// embedTaskTypes reports that the embedding API accepts a task type.
	embed          func(ctx context.Context, req *EmbeddingRequest, model, apiKey string) (*EmbeddingResponse, error)
	embedTaskTypes bool
}

// requiresKey reports whether a request against baseURL needs a credential.
func (p providerSpec) requiresKey(baseURL string) bool {
	return p.keyless == nil || !p.keyless(baseURL)
}

// scopeBaseURL drops a caller's base URL for providers it cannot apply to.
func (p providerSpec) scopeBaseURL(baseURL string) string {
	if p.nativeEndpoint {
		return ""
	}
	return baseURL
}

// routeHost returns the route a model id pins through its suffix, or "".
func (p providerSpec) routeHost(model string) string {
	if p.splitHost == nil {
		return ""
	}
	_, host := p.splitHost(model)
	return host
}

// providerTable is the default provider table for paths that have no
// MultiPass in hand: request targets, route hosts, and embeddings.
var providerTable = sync.OnceValue(defaultProviders)

// providerFor looks a provider prefix up in the default table.
func providerFor(provider string) providerSpec {
	return providerTable()[strings.ToLower(provider)]
}

// MultiPass routes requests to different LLM providers based on model prefix.
type MultiPass struct {
	apiKeyMu       sync.RWMutex
	apiKeys        map[string]string
	runtimeAPIKeys map[string]string
	providers      map[string]providerSpec
	metadata       *modelMetadataService
}

// getEnvVarNameForProvider returns the environment variable name for the given provider
func getEnvVarNameForProvider(provider string) string {
	return "POLLYTOOL_" + strings.ToUpper(provider) + "KEY"
}

// NewMultiPass creates a new multi-provider router using a snapshot of the
// provided API keys.
func NewMultiPass(apiKeys map[string]string) *MultiPass {
	return newMultiPass(apiKeys, defaultProviders())
}

func newMultiPass(apiKeys map[string]string, providers map[string]providerSpec) *MultiPass {
	return &MultiPass{
		apiKeys:        maps.Clone(apiKeys),
		runtimeAPIKeys: make(map[string]string),
		providers:      maps.Clone(providers),
		metadata:       newModelMetadataService(),
	}
}

// SetAPIKey installs a process-local credential override. Runtime keys are
// deliberately kept out of configuration, environment variables, and session
// metadata; callers that want persistence should use an OS credential store.
func (m *MultiPass) SetAPIKey(provider, apiKey string) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" || apiKey == "" {
		return
	}
	m.apiKeyMu.Lock()
	m.runtimeAPIKeys[provider] = apiKey
	m.apiKeyMu.Unlock()
}

// ClearAPIKey removes only the process-local override, revealing any key that
// was supplied when MultiPass was constructed.
func (m *MultiPass) ClearAPIKey(provider string) {
	m.apiKeyMu.Lock()
	delete(m.runtimeAPIKeys, strings.ToLower(strings.TrimSpace(provider)))
	m.apiKeyMu.Unlock()
}

// APIKeySource reports where the effective key comes from without exposing it.
// The empty string means no key is configured.
func (m *MultiPass) APIKeySource(provider string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	m.apiKeyMu.RLock()
	defer m.apiKeyMu.RUnlock()
	if m.runtimeAPIKeys[provider] != "" {
		return "session"
	}
	if m.apiKeys[provider] != "" {
		return "environment"
	}
	return ""
}

func (m *MultiPass) apiKey(provider string) string {
	m.apiKeyMu.RLock()
	defer m.apiKeyMu.RUnlock()
	if key := m.runtimeAPIKeys[provider]; key != "" {
		return key
	}
	return m.apiKeys[provider]
}

const (
	defaultHuggingFaceBaseURL = "https://router.huggingface.co/v1"
	defaultOpenRouterBaseURL  = "https://openrouter.ai/api/v1"
)

// customEndpointKeyless lets an OpenAI-compatible request against a
// user-supplied endpoint run without a credential.
func customEndpointKeyless(baseURL string) bool { return strings.TrimSpace(baseURL) != "" }

func alwaysKeyless(string) bool { return true }

// splitHuggingFaceModel separates the ":provider" route suffix a Hugging
// Face model id may carry.
func splitHuggingFaceModel(model string) (string, string) {
	if name, host, ok := strings.Cut(model, ":"); ok {
		return name, host
	}
	return model, ""
}

// defaultProviders is the provider table. Add a provider here and every
// router in the package knows it; nothing else in the package names one.
func defaultProviders() map[string]providerSpec {
	return map[string]providerSpec{
		"openai": {
			metadata: openai.ListModels,
			// An empty base URL keeps the native Responses API; any other
			// endpoint is Chat Completions.
			catalogBaseURL: "https://api.openai.com/v1",
			new:            func(apiKey, baseURL string) (LLM, error) { return openai.NewProvider(apiKey, baseURL), nil },
			keyless:        customEndpointKeyless,
			embed:          openai.Embed,
		},
		"anthropic": {
			metadata:       anthropic.ListModels,
			defaultBaseURL: "https://api.anthropic.com/v1",
			nativeEndpoint: true,
			new:            func(apiKey, baseURL string) (LLM, error) { return anthropic.NewProvider(apiKey, baseURL), nil },
		},
		"gemini": {
			metadata:       gemini.ListModels,
			defaultBaseURL: "https://generativelanguage.googleapis.com/v1beta",
			nativeEndpoint: true,
			new:            func(apiKey, baseURL string) (LLM, error) { return gemini.NewProvider(apiKey, baseURL) },
			embed:          gemini.Embed,
			embedTaskTypes: true,
		},
		"ollama": {
			metadata:       ollama.ListModels,
			new:            func(apiKey, baseURL string) (LLM, error) { return ollama.NewProvider(baseURL, apiKey), nil },
			defaultBaseURL: ollama.DefaultBaseURL,
			keyless:        alwaysKeyless,
			keylessCatalog: true,
		},
		"huggingface": {
			metadata:       openai.ListHuggingFaceModels,
			new:            func(apiKey, baseURL string) (LLM, error) { return openai.NewProvider(apiKey, baseURL), nil },
			defaultBaseURL: defaultHuggingFaceBaseURL,
			keylessCatalog: true,
			splitHost:      splitHuggingFaceModel,
		},
		"deepseek": {
			metadata:       deepseek.ListModels,
			new:            func(apiKey, baseURL string) (LLM, error) { return deepseek.NewProvider(apiKey, baseURL), nil },
			defaultBaseURL: deepseek.DefaultBaseURL,
		},
		"openrouter": {
			metadata:       openai.ListOpenRouterModels,
			new:            func(apiKey, baseURL string) (LLM, error) { return openai.NewOpenRouterProvider(apiKey, baseURL), nil },
			defaultBaseURL: defaultOpenRouterBaseURL,
			keylessCatalog: true,
			hostRouting:    true,
			routedCatalog:  true,
			catalogVersion: "reasoning-policy-v2",
		},
	}
}

// ChatCompletionStream routes the request to the provider named by its model
// prefix: it scopes the base URL, fills the API key, strips the prefix and
// delegates. It does not adapt the request to the model; Agent.Run does that
// each iteration, and direct callers use Prepare.
func (m *MultiPass) ChatCompletionStream(ctx context.Context, req *CompletionRequest, processor EventStreamProcessor) <-chan *messages.StreamEvent {
	// Work on a copy so we don't mutate the caller's request
	localReq := *req
	req = &localReq

	// Parse the model string to extract provider and actual model name
	provider, actualModel, ok := strings.Cut(req.Model, "/")
	if !ok {
		err := fmt.Errorf("model must include provider prefix (e.g., 'openai/gpt-5.4', 'anthropic/claude-sonnet-4-6'). Got: %s", req.Model)
		return processor.ProcessMessagesToEvents(singleErrorMessage(err))
	}

	provider = strings.ToLower(provider)
	spec := m.providers[provider]
	req.BaseURL = spec.scopeBaseURL(req.BaseURL)

	if req.ModelHost != "" && !spec.hostRouting {
		return processor.ProcessMessagesToEvents(singleErrorMessage(fmt.Errorf("modelhost is supported only for OpenRouter")))
	}
	// Update the request with the actual model name (without prefix)
	req.Model = actualModel

	// Populate or validate the API key; the provider table says which
	// requests may go without one.
	if req.APIKey == "" {
		if key := m.apiKey(provider); key != "" {
			req.APIKey = key
		} else if spec, ok := m.providers[provider]; ok && spec.requiresKey(req.BaseURL) {
			envVar := getEnvVarNameForProvider(provider)
			err := fmt.Errorf("missing API key for provider '%s'. Set the %s environment variable.", provider, envVar)
			return processor.ProcessMessagesToEvents(singleErrorMessage(err))
		}
	}

	// Create a provider client for this request.
	client, err := m.clientFor(provider, req.APIKey, req.BaseURL)
	if err != nil {
		return processor.ProcessMessagesToEvents(singleErrorMessage(err))
	}

	return client.ChatCompletionStream(ctx, req, processor)
}

// clientFor creates a provider client for the current request.
func (m *MultiPass) clientFor(provider, apiKey, baseURL string) (LLM, error) {
	spec, ok := m.providers[provider]
	if !ok {
		return nil, fmt.Errorf("unknown provider '%s'. Valid providers: %s", provider, strings.Join(slices.Sorted(maps.Keys(m.providers)), ", "))
	}
	baseURL = spec.scopeBaseURL(baseURL)
	if baseURL == "" {
		baseURL = spec.defaultBaseURL
	}
	return spec.new(apiKey, baseURL)
}

func singleErrorMessage(err error) <-chan messages.ChatMessage {
	errorChan := make(chan messages.ChatMessage, 1)

	msg := messages.ChatMessage{
		Role:    messages.MessageRoleAssistant,
		Content: fmt.Sprintf("Error: %v", err),
	}
	msg.SetError(err)

	errorChan <- msg
	close(errorChan)
	return errorChan
}
