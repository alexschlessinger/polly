package contract

import "time"

// EmbeddingRequest contains parameters for creating embeddings.
type EmbeddingRequest struct {
	APIKey     string
	BaseURL    string
	Timeout    time.Duration
	Model      string   // provider/model format, e.g. openai/text-embedding-3-large
	Input      []string // one or more texts
	Dimensions int      // optional output dimensions for supported providers
	TaskType   string   // optional, gemini-only; e.g. "RETRIEVAL_DOCUMENT", "RETRIEVAL_QUERY", "CLASSIFICATION"
}

// EmbeddingResponse is the provider-agnostic embeddings result.
type EmbeddingResponse struct {
	Model       string
	Embeddings  [][]float64
	InputTokens int
}
