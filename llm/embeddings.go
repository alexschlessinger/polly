package llm

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	ai "github.com/sashabaranov/go-openai"
)

const defaultEmbeddingTimeout = 120 * time.Second

// EmbeddingRequest contains parameters for creating embeddings.
type EmbeddingRequest struct {
	APIKey     string
	BaseURL    string
	Timeout    time.Duration
	Model      string   // provider/model format, e.g. openai/text-embedding-3-large
	Input      []string // one or more texts
	Dimensions int      // optional output dimensions for supported providers
}

// EmbeddingResponse is the provider-agnostic embeddings result.
type EmbeddingResponse struct {
	Model       string
	Embeddings  [][]float64
	InputTokens int
}

// QuickEmbed performs a one-shot embedding request.
func QuickEmbed(ctx context.Context, model string, input []string, dimensions int) (*EmbeddingResponse, error) {
	return Embed(ctx, &EmbeddingRequest{
		Model:      model,
		Input:      input,
		Dimensions: dimensions,
		Timeout:    defaultEmbeddingTimeout,
	})
}

// QuickEmbedOne embeds a single string and returns the first vector.
func QuickEmbedOne(ctx context.Context, model string, input string, dimensions int) ([]float64, int, error) {
	resp, err := QuickEmbed(ctx, model, []string{input}, dimensions)
	if err != nil {
		return nil, 0, err
	}
	if len(resp.Embeddings) == 0 {
		return nil, resp.InputTokens, fmt.Errorf("no embeddings returned")
	}
	return resp.Embeddings[0], resp.InputTokens, nil
}

// Embed routes an embedding request to the provider selected by Model prefix.
func Embed(ctx context.Context, req *EmbeddingRequest) (*EmbeddingResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("embedding request is required")
	}
	if strings.TrimSpace(req.Model) == "" {
		return nil, fmt.Errorf("embedding request model is required")
	}
	if len(req.Input) == 0 {
		return nil, fmt.Errorf("embedding request input is required")
	}

	parts := strings.SplitN(req.Model, "/", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("embedding model must include provider prefix (e.g., 'openai/text-embedding-3-large'). got: %s", req.Model)
	}

	provider := strings.ToLower(parts[0])
	model := parts[1]
	if model == "" {
		return nil, fmt.Errorf("embedding model name cannot be empty for provider %q", provider)
	}

	switch provider {
	case "openai":
		apiKey, err := resolveEmbeddingAPIKey(provider, req.APIKey, req.BaseURL)
		if err != nil {
			return nil, err
		}
		return embedOpenAI(ctx, req, model, apiKey)
	default:
		return nil, fmt.Errorf("unsupported embedding provider %q", provider)
	}
}

func resolveEmbeddingAPIKey(provider, explicit, baseURL string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	// Keep behavior aligned with chat routing: OpenAI-compatible endpoints can be keyless.
	if provider == "openai" && strings.TrimSpace(baseURL) != "" {
		return "", nil
	}
	envVar := getEnvVarNameForProvider(provider)
	key := os.Getenv(envVar)
	if key == "" {
		return "", fmt.Errorf("missing API key for provider '%s'. set the %s environment variable", provider, envVar)
	}
	return key, nil
}

func embedOpenAI(ctx context.Context, req *EmbeddingRequest, model, apiKey string) (*EmbeddingResponse, error) {
	cfg := ai.DefaultConfig(apiKey)
	if req.BaseURL != "" {
		cfg.BaseURL = req.BaseURL
	}
	client := ai.NewClientWithConfig(cfg)

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = defaultEmbeddingTimeout
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	openAIReq := &ai.EmbeddingRequest{
		Input: req.Input,
		Model: ai.EmbeddingModel(model),
	}
	if req.Dimensions > 0 {
		openAIReq.Dimensions = req.Dimensions
	}

	resp, err := client.CreateEmbeddings(requestCtx, openAIReq)
	if err != nil {
		return nil, fmt.Errorf("openai embedding request failed: %w", err)
	}
	if len(resp.Data) == 0 {
		return nil, fmt.Errorf("openai embedding response returned no vectors")
	}

	embeddings := make([][]float64, len(resp.Data))
	for i, item := range resp.Data {
		vector := make([]float64, len(item.Embedding))
		for j, value := range item.Embedding {
			vector[j] = float64(value)
		}
		embeddings[i] = vector
	}

	return &EmbeddingResponse{
		Model:       string(resp.Model),
		Embeddings:  embeddings,
		InputTokens: resp.Usage.TotalTokens,
	}, nil
}
