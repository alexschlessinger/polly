package openai

import (
	"context"
	"fmt"
	"strings"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
)

// Embed creates embeddings for req.Input with model at req.BaseURL (the
// public API when empty), reporting the total tokens the API billed.
func Embed(ctx context.Context, req *contract.EmbeddingRequest, model, apiKey string) (*contract.EmbeddingResponse, error) {
	client := NewClient(apiKey, strings.TrimSpace(req.BaseURL))
	wire := &EmbeddingRequest{Model: model, Input: req.Input}
	if req.Dimensions > 0 {
		dim := int64(req.Dimensions)
		wire.Dimensions = &dim
	}
	resp, err := client.CreateEmbeddings(ctx, wire)
	if err != nil {
		return nil, fmt.Errorf("openai embedding request failed: %w", err)
	}
	if len(resp.Data) == 0 {
		return nil, fmt.Errorf("openai embedding response returned no vectors")
	}
	embeddings := make([][]float64, len(resp.Data))
	for i, item := range resp.Data {
		embeddings[i] = item.Embedding
	}
	inputTokens := 0
	if resp.Usage != nil {
		inputTokens = int(resp.Usage.TotalTokens)
	}
	return &contract.EmbeddingResponse{Model: resp.Model, Embeddings: embeddings, InputTokens: inputTokens}, nil
}
