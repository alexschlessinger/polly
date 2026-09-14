package llm

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
)

const defaultEmbeddingTimeout = 120 * time.Second

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

	provider, model, ok := strings.Cut(req.Model, "/")
	if !ok {
		return nil, fmt.Errorf("embedding model must include provider prefix (e.g., 'openai/text-embedding-3-large'). got: %s", req.Model)
	}

	provider = strings.ToLower(provider)
	if model == "" {
		return nil, fmt.Errorf("embedding model name cannot be empty for provider %q", provider)
	}
	spec := providerFor(provider)
	if spec.embed == nil {
		return nil, fmt.Errorf("unsupported embedding provider %q", provider)
	}
	if req.TaskType != "" && !spec.embedTaskTypes {
		slog.Warn("embedding_task_type_ignored", "provider", provider, "task_type", req.TaskType)
	}
	apiKey, err := resolveEmbeddingAPIKey(provider, req.APIKey, req.BaseURL)
	if err != nil {
		return nil, err
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = defaultEmbeddingTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return spec.embed(ctx, req, model, apiKey)
}

// resolveEmbeddingAPIKey applies the provider table's credential rule, so an
// endpoint that is keyless for chat is keyless for embeddings too.
func resolveEmbeddingAPIKey(provider, explicit, baseURL string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if spec := providerFor(provider); spec.new != nil && !spec.requiresKey(baseURL) {
		return "", nil
	}
	envVar := getEnvVarNameForProvider(provider)
	key := os.Getenv(envVar)
	if key == "" {
		return "", fmt.Errorf("missing API key for provider '%s'. set the %s environment variable", provider, envVar)
	}
	return key, nil
}
