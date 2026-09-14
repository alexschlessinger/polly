package gemini

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/alexschlessinger/pollytool/llm/internal/contract"
)

// Embed creates embeddings for req.Input with model through
// batchEmbedContents. The API reports no token usage. req.BaseURL is not
// applied; the public endpoint is always used.
func Embed(ctx context.Context, req *contract.EmbeddingRequest, model, apiKey string) (*contract.EmbeddingResponse, error) {
	client := NewClient(apiKey)
	var dimensions *int32
	if req.Dimensions > 0 {
		dim := int32(req.Dimensions)
		dimensions = &dim
	}
	// gemini-embedding-2 ignores the task_type field and expects task instructions
	// prepended to each input; older models keep using the taskType request field.
	var prefix, taskType string
	if tt := strings.TrimSpace(req.TaskType); tt != "" {
		if isGemini2EmbedModel(model) {
			p, err := gemini2TaskPrefix(tt)
			if err != nil {
				return nil, err
			}
			prefix = p
		} else {
			taskType = tt
		}
	}
	requests := make([]*EmbedContentRequest, len(req.Input))
	for i, text := range req.Input {
		requests[i] = &EmbedContentRequest{
			Content:              &Content{Parts: []*Part{{Text: prefix + text}}},
			TaskType:             taskType,
			OutputDimensionality: dimensions,
		}
	}
	resp, err := client.BatchEmbedContents(ctx, model, requests)
	if err != nil {
		return nil, fmt.Errorf("gemini embedding request failed: %w", err)
	}
	if len(resp.Embeddings) == 0 {
		return nil, fmt.Errorf("gemini embedding response returned no vectors")
	}
	// Gemini API only pre-normalizes outputs at the native 3072 dimension.
	// Other MRL truncations must be L2-normalized before cosine similarity is meaningful.
	needsNormalize := req.Dimensions > 0 && req.Dimensions != 3072
	embeddings := make([][]float64, len(resp.Embeddings))
	for i, item := range resp.Embeddings {
		vector := make([]float64, len(item.Values))
		for j, value := range item.Values {
			vector[j] = float64(value)
		}
		if needsNormalize {
			l2Normalize(vector)
		}
		embeddings[i] = vector
	}
	return &contract.EmbeddingResponse{Model: model, Embeddings: embeddings}, nil
}

// gemini2TaskPrefixes maps the gemini-embedding-001 task_type enum onto the
// prompt-prefix templates that gemini-embedding-2 expects. RETRIEVAL_DOCUMENT
// uses the title/text format with no title since the API takes a single string.
var gemini2TaskPrefixes = map[string]string{
	"RETRIEVAL_QUERY":      "task: search result | query: ",
	"RETRIEVAL_DOCUMENT":   "title: none | text: ",
	"SEMANTIC_SIMILARITY":  "task: sentence similarity | query: ",
	"CLASSIFICATION":       "task: classification | query: ",
	"CLUSTERING":           "task: clustering | query: ",
	"QUESTION_ANSWERING":   "task: question answering | query: ",
	"FACT_VERIFICATION":    "task: fact checking | query: ",
	"CODE_RETRIEVAL_QUERY": "task: code retrieval | query: ",
}

func isGemini2EmbedModel(model string) bool {
	return strings.HasPrefix(model, "gemini-embedding-2")
}

func gemini2TaskPrefix(taskType string) (string, error) {
	prefix, ok := gemini2TaskPrefixes[strings.ToUpper(taskType)]
	if !ok {
		return "", fmt.Errorf("unsupported task type %q for gemini-embedding-2", taskType)
	}
	return prefix, nil
}

func l2Normalize(v []float64) {
	var sumSq float64
	for _, x := range v {
		sumSq += x * x
	}
	if sumSq == 0 {
		return
	}
	norm := math.Sqrt(sumSq)
	for i := range v {
		v[i] /= norm
	}
}
