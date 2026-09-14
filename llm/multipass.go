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
	"slices"
	"strings"
	"sync"

	"github.com/alexschlessinger/pollytool/messages"
)

type providerFactory func(apiKey, baseURL string) (LLM, error)

// providerSpec is everything the package knows about one provider prefix:
// how to build its chat client, the endpoint used when a request names
// none, whether a request may go without a credential, and the optional
// capabilities only some providers offer. Chat routing, embeddings, and
// context-window discovery all read this one table, so a provider is
// described in exactly one place.
type providerSpec struct {
	metadata metadataFetcher
	new      providerFactory
	// defaultBaseURL fills an empty request base URL before new runs.
	defaultBaseURL string
	// keyless reports whether a request with the given (undefaulted) base
	// URL may proceed without an API key. nil means a key is always required.
	keyless func(baseURL string) bool
	// embed serves embedding requests; nil when the provider has none.
	embed func(ctx context.Context, req *EmbeddingRequest, model, apiKey string) (*EmbeddingResponse, error)
}

// requiresKey reports whether a request against baseURL needs a credential.
func (p providerSpec) requiresKey(baseURL string) bool {
	return p.keyless == nil || !p.keyless(baseURL)
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

// defaultProviders is the provider table. Add a provider here and every
// router in the package knows it.
func defaultProviders() map[string]providerSpec {
	return map[string]providerSpec{
		"openai": {
			metadata:       fetchProviderMetadata,
			defaultBaseURL: "https://api.openai.com/v1",
			new:            func(apiKey, baseURL string) (LLM, error) { return openai.NewProvider(apiKey, baseURL), nil },
			keyless:        customEndpointKeyless,
			embed:          embedOpenAI,
		},
		"anthropic": {
			metadata:       fetchProviderMetadata,
			defaultBaseURL: "https://api.anthropic.com/v1",
			new:            func(apiKey, baseURL string) (LLM, error) { return anthropic.NewProvider(apiKey, baseURL), nil },
		},
		"gemini": {
			metadata:       fetchProviderMetadata,
			defaultBaseURL: "https://generativelanguage.googleapis.com/v1beta",
			new:            func(apiKey, baseURL string) (LLM, error) { return gemini.NewProvider(apiKey, baseURL) },
			embed:          embedGemini,
		},
		"ollama": {
			metadata:       fetchProviderMetadata,
			new:            func(apiKey, baseURL string) (LLM, error) { return ollama.NewProvider(baseURL, apiKey), nil },
			defaultBaseURL: ollama.DefaultBaseURL,
			keyless:        alwaysKeyless,
		},
		"huggingface": {
			metadata:       fetchProviderMetadata,
			new:            func(apiKey, baseURL string) (LLM, error) { return openai.NewProvider(apiKey, baseURL), nil },
			defaultBaseURL: defaultHuggingFaceBaseURL,
		},
		"deepseek": {
			metadata:       fetchProviderMetadata,
			new:            func(apiKey, baseURL string) (LLM, error) { return deepseek.NewProvider(apiKey, baseURL), nil },
			defaultBaseURL: deepseek.DefaultBaseURL,
		},
		"openrouter": {
			metadata:       fetchProviderMetadata,
			new:            func(apiKey, baseURL string) (LLM, error) { return openai.NewOpenRouterProvider(apiKey, baseURL), nil },
			defaultBaseURL: defaultOpenRouterBaseURL,
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
	req.BaseURL = requestBaseURL(provider, req.BaseURL)

	if req.ModelHost != "" && provider != "openrouter" {
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
	baseURL = requestBaseURL(provider, baseURL)
	if baseURL == "" && provider != "openai" {
		baseURL = spec.defaultBaseURL
	}
	return spec.new(apiKey, baseURL)
}

func requestBaseURL(provider, baseURL string) string {
	if strings.EqualFold(provider, "anthropic") || strings.EqualFold(provider, "gemini") {
		return ""
	}
	return baseURL
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
