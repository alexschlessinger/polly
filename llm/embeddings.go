package llm

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strings"
	"time"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"google.golang.org/genai"
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
	TaskType   string   // optional, gemini-only; e.g. "RETRIEVAL_DOCUMENT", "RETRIEVAL_QUERY", "CLASSIFICATION"
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
	if req.TaskType != "" && provider != "gemini" {
		slog.Warn("embedding_task_type_ignored", "provider", provider, "task_type", req.TaskType)
	}

	switch provider {
	case "openai":
		apiKey, err := resolveEmbeddingAPIKey(provider, req.APIKey, req.BaseURL)
		if err != nil {
			return nil, err
		}
		return embedOpenAI(ctx, req, model, apiKey)
	case "gemini":
		apiKey, err := resolveEmbeddingAPIKey(provider, req.APIKey, req.BaseURL)
		if err != nil {
			return nil, err
		}
		return embedGemini(ctx, req, model, apiKey)
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
	baseURL := defaultOpenAIBaseURL
	if strings.TrimSpace(req.BaseURL) != "" {
		baseURL = strings.TrimSpace(req.BaseURL)
	}
	client := openai.NewClient(
		option.WithAPIKey(apiKey),
		option.WithBaseURL(baseURL),
	)

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = defaultEmbeddingTimeout
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	openAIReq := openai.EmbeddingNewParams{
		Input: openai.EmbeddingNewParamsInputUnion{
			OfArrayOfStrings: req.Input,
		},
		Model: openai.EmbeddingModel(model),
	}
	if req.Dimensions > 0 {
		openAIReq.Dimensions = param.NewOpt(int64(req.Dimensions))
	}

	resp, err := client.Embeddings.New(requestCtx, openAIReq)
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
		InputTokens: int(resp.Usage.TotalTokens),
	}, nil
}

func embedGemini(ctx context.Context, req *EmbeddingRequest, model, apiKey string) (*EmbeddingResponse, error) {
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = defaultEmbeddingTimeout
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client, err := genai.NewClient(requestCtx, &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, fmt.Errorf("creating gemini client: %w", err)
	}

	config := &genai.EmbedContentConfig{}
	if req.Dimensions > 0 {
		dim := int32(req.Dimensions)
		config.OutputDimensionality = &dim
	}

	// gemini-embedding-2 ignores the task_type field and expects task instructions
	// prepended to each input; older models keep using the SDK's TaskType config.
	var prefix string
	if taskType := strings.TrimSpace(req.TaskType); taskType != "" {
		if isGemini2EmbedModel(model) {
			p, err := gemini2TaskPrefix(taskType)
			if err != nil {
				return nil, err
			}
			prefix = p
		} else {
			config.TaskType = taskType
		}
	}

	contents := make([]*genai.Content, len(req.Input))
	for i, text := range req.Input {
		contents[i] = &genai.Content{
			Parts: []*genai.Part{{Text: prefix + text}},
		}
	}

	resp, err := client.Models.EmbedContent(requestCtx, model, contents, config)
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

	return &EmbeddingResponse{
		Model:      model,
		Embeddings: embeddings,
	}, nil
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
