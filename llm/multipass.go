package llm

import (
	"context"
	"fmt"
	"strings"

	"github.com/alexschlessinger/pollytool/messages"
	"go.uber.org/zap"
)

// MultiPass routes requests to different LLM providers based on model prefix.
// Provider clients are created eagerly at construction time and reused.
type MultiPass struct {
	apiKeys map[string]string
	clients map[string]LLM
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

// NewMultiPass creates a new multi-provider router with eagerly initialized clients.
func NewMultiPass(apiKeys map[string]string) *MultiPass {
	clients := make(map[string]LLM)
	for provider, key := range apiKeys {
		if key == "" {
			continue
		}
		switch provider {
		case "openai":
			clients["openai"] = NewOpenAIClient(key, "")
		case "anthropic":
			clients["anthropic"] = NewAnthropicClient(key)
		case "gemini":
			c, err := NewGeminiClient(key)
			if err != nil {
				zap.S().Warnw("skipping gemini client", "error", err)
				continue
			}
			clients["gemini"] = c
		case "ollama":
			clients["ollama"] = NewOllamaClient("http://localhost:11434", key)
		}
	}
	return &MultiPass{
		apiKeys: apiKeys,
		clients: clients,
	}
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

	// Use pre-built client if key/baseURL match defaults, otherwise create one-off
	client, err := m.clientFor(provider, req.APIKey, req.BaseURL)
	if err != nil {
		return processor.ProcessMessagesToEvents(singleErrorMessage(err))
	}

	return client.ChatCompletionStream(ctx, req, processor)
}

// clientFor returns the pre-built client for a provider when the request uses
// default credentials, or creates a one-off client for overridden keys/baseURLs.
func (m *MultiPass) clientFor(provider, apiKey, baseURL string) (LLM, error) {
	// Use the pre-built client when key matches the default and no custom baseURL
	if c, ok := m.clients[provider]; ok && apiKey == m.apiKeys[provider] && baseURL == "" {
		return c, nil
	}

	switch provider {
	case "openai":
		return NewOpenAIClient(apiKey, baseURL), nil
	case "anthropic":
		return NewAnthropicClient(apiKey), nil
	case "gemini":
		return NewGeminiClient(apiKey)
	case "ollama":
		if baseURL == "" {
			baseURL = "http://localhost:11434"
		}
		return NewOllamaClient(baseURL, apiKey), nil
	default:
		return nil, fmt.Errorf("unknown provider '%s'. Valid providers: openai, anthropic, gemini, ollama", provider)
	}
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
