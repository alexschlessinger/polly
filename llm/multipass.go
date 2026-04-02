package llm

import (
	"context"
	"fmt"
	"strings"

	"github.com/alexschlessinger/pollytool/messages"
)

type providerFactory func(apiKey, baseURL string) (LLM, error)

// MultiPass routes requests to different LLM providers based on model prefix.
type MultiPass struct {
	apiKeys   map[string]string
	factories map[string]providerFactory
}

// getEnvVarNameForProvider returns the environment variable name for the given provider
func getEnvVarNameForProvider(provider string) string {
	switch provider {
	case "openai":
		return "POLLYTOOL_OPENAIKEY"
	case "anthropic":
		return "POLLYTOOL_ANTHROPICKEY"
	case "gemini":
		return "POLLYTOOL_GEMINIKEY"
	case "ollama":
		return "POLLYTOOL_OLLAMAKEY"
	default:
		return fmt.Sprintf("POLLYTOOL_%sKEY", strings.ToUpper(provider))
	}
}

// NewMultiPass creates a new multi-provider router using a snapshot of the
// provided API keys.
func NewMultiPass(apiKeys map[string]string) *MultiPass {
	return newMultiPass(apiKeys, defaultProviderFactories())
}

func newMultiPass(apiKeys map[string]string, factories map[string]providerFactory) *MultiPass {
	return &MultiPass{
		apiKeys:   copyAPIKeys(apiKeys),
		factories: copyProviderFactories(factories),
	}
}

func defaultProviderFactories() map[string]providerFactory {
	return map[string]providerFactory{
		"openai": func(apiKey, baseURL string) (LLM, error) {
			return NewOpenAIClient(apiKey, baseURL), nil
		},
		"anthropic": func(apiKey, _ string) (LLM, error) {
			return NewAnthropicClient(apiKey), nil
		},
		"gemini": func(apiKey, _ string) (LLM, error) {
			return NewGeminiClient(apiKey)
		},
		"ollama": func(apiKey, baseURL string) (LLM, error) {
			return NewOllamaClient(baseURL, apiKey), nil
		},
	}
}

func copyAPIKeys(apiKeys map[string]string) map[string]string {
	out := make(map[string]string, len(apiKeys))
	for provider, key := range apiKeys {
		out[provider] = key
	}
	return out
}

func copyProviderFactories(factories map[string]providerFactory) map[string]providerFactory {
	out := make(map[string]providerFactory, len(factories))
	for provider, factory := range factories {
		out[provider] = factory
	}
	return out
}

// ChatCompletionStream routes the request to the appropriate provider using event-based streaming
func (m *MultiPass) ChatCompletionStream(ctx context.Context, req *CompletionRequest, processor EventStreamProcessor) <-chan *messages.StreamEvent {
	// Work on a copy so we don't mutate the caller's request
	localReq := *req
	req = &localReq

	// Parse the model string to extract provider and actual model name
	parts := strings.SplitN(req.Model, "/", 2)
	if len(parts) != 2 {
		err := fmt.Errorf("model must include provider prefix (e.g., 'openai/gpt-4.1', 'anthropic/claude-sonnet-4-20250514'). Got: %s", req.Model)
		return processor.ProcessMessagesToEvents(singleErrorMessage(err))
	}

	provider := strings.ToLower(parts[0])
	actualModel := parts[1]

	// Update the request with the actual model name (without prefix)
	req.Model = actualModel

	// Populate or validate API key (ollama can be keyless, openai with custom endpoint can be keyless)
	if req.APIKey == "" {
		if key := m.apiKeys[provider]; key != "" {
			req.APIKey = key
		} else if provider != "ollama" && !(provider == "openai" && req.BaseURL != "") {
			envVar := getEnvVarNameForProvider(provider)
			err := fmt.Errorf("missing API key for provider '%s'. Set the %s environment variable.", provider, envVar)
			return processor.ProcessMessagesToEvents(singleErrorMessage(err))
		}
	}

	// Resolve skill prompt injection if configured
	if req.Skills != nil && !req.Skills.IsEmpty() {
		req.Messages = req.ResolvedMessages()
		req.Skills = nil
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
	factory, ok := m.factories[provider]
	if !ok {
		return nil, fmt.Errorf("unknown provider '%s'. Valid providers: openai, anthropic, gemini, ollama", provider)
	}

	if provider == "ollama" && baseURL == "" {
		baseURL = "http://localhost:11434"
	}

	return factory(apiKey, baseURL)
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
