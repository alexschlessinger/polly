package llm

import (
	"context"
	"fmt"
	"strings"

	"github.com/alexschlessinger/pollytool/messages"
)

// MultiPass routes requests to different LLM providers based on model prefix
type MultiPass struct {
	apiKeys map[string]string
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

// NewMultiPass creates a new multi-provider router
func NewMultiPass(apiKeys map[string]string) *MultiPass {
	return &MultiPass{
		apiKeys: apiKeys,
	}
}

// ChatCompletionStream routes the request to the appropriate provider using event-based streaming
func (m *MultiPass) ChatCompletionStream(ctx context.Context, req *CompletionRequest, processor EventStreamProcessor) <-chan *messages.StreamEvent {
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

	// Route to the appropriate provider
	var llm LLM
	switch provider {
	case "openai":
		llm = NewOpenAIClient(req.APIKey, req.BaseURL)
	case "anthropic":
		llm = NewAnthropicClient(req.APIKey)
	case "gemini":
		llm = NewGeminiClient(req.APIKey)
	case "ollama":
		baseURL := req.BaseURL
		if baseURL == "" {
			baseURL = "http://localhost:11434"
		}
		llm = NewOllamaClient(baseURL, req.APIKey)
	default:
		err := fmt.Errorf("unknown provider '%s'. Valid providers: openai, anthropic, gemini, ollama", provider)
		return processor.ProcessMessagesToEvents(singleErrorMessage(err))
	}

	// Delegate to the selected provider
	return llm.ChatCompletionStream(ctx, req, processor)
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
