// Package gemini is a minimal REST client for the Gemini Developer API
// (generativelanguage.googleapis.com) covering the slice polly uses:
// generateContent, streamGenerateContent over SSE, and batchEmbedContents,
// authenticated by API key. It replaces google.golang.org/genai, whose
// transitive closure (grpc, protobuf, opentelemetry, cloud.google.com/*)
// polly never exercised.
//
// Struct fields mirror the wire format one-to-one; names and enum values
// match the JSON the API documents. []byte fields (thought signatures,
// inline image data) rely on encoding/json's base64 encoding, which is the
// wire encoding.
package gemini

// Content is one turn of conversation history. Role is "user" or "model";
// system instructions travel outside Contents and leave Role empty.
type Content struct {
	Parts []*Part `json:"parts,omitempty"`
	Role  string  `json:"role,omitempty"`
}

// Part is a single piece of a Content. Exactly one of the payload fields is
// normally set.
type Part struct {
	Text string `json:"text,omitempty"`
	// Thought marks text as a thought summary rather than answer content.
	Thought bool `json:"thought,omitempty"`
	// ThoughtSignature is an opaque token that must be replayed with the
	// function-call part it arrived on, or multi-turn tool calling degrades
	// on thinking models.
	ThoughtSignature []byte            `json:"thoughtSignature,omitempty"`
	InlineData       *Blob             `json:"inlineData,omitempty"`
	FunctionCall     *FunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *FunctionResponse `json:"functionResponse,omitempty"`
}

// Blob is inline binary data, e.g. an image.
type Blob struct {
	MIMEType string `json:"mimeType,omitempty"`
	Data     []byte `json:"data,omitempty"`
}

// FunctionCall is a tool invocation requested by the model. The API assigns
// no call IDs; callers correlate calls positionally.
type FunctionCall struct {
	// ID is set by the API on some models; when present it must be echoed
	// back on the matching FunctionResponse.
	ID   string         `json:"id,omitempty"`
	Name string         `json:"name,omitempty"`
	Args map[string]any `json:"args,omitempty"`
}

// FunctionResponse returns a tool result to the model. Response must be an
// object; wrap bare values before sending.
type FunctionResponse struct {
	ID       string         `json:"id,omitempty"`
	Name     string         `json:"name,omitempty"`
	Response map[string]any `json:"response,omitempty"`
}

// Tool declares functions the model may call.
type Tool struct {
	FunctionDeclarations []*FunctionDeclaration `json:"functionDeclarations,omitempty"`
}

// FunctionDeclaration describes one callable function.
// ParametersJsonSchema takes a raw JSON Schema object, which the API accepts
// directly (unlike the typed Schema required for structured output).
type FunctionDeclaration struct {
	Name                 string `json:"name,omitempty"`
	Description          string `json:"description,omitempty"`
	ParametersJsonSchema any    `json:"parametersJsonSchema,omitempty"`
}

// ThinkingLevel selects reasoning depth on Gemini 3.x models.
type ThinkingLevel string

const (
	ThinkingLevelMinimal ThinkingLevel = "MINIMAL"
	ThinkingLevelLow     ThinkingLevel = "LOW"
	ThinkingLevelMedium  ThinkingLevel = "MEDIUM"
	ThinkingLevelHigh    ThinkingLevel = "HIGH"
)

// ThinkingConfig controls reasoning. 3.x models take ThinkingLevel; 2.5-era
// models take ThinkingBudget in tokens, where -1 means model-managed and the
// pointer distinguishes an explicit 0 (thinking off) from unset.
type ThinkingConfig struct {
	IncludeThoughts bool          `json:"includeThoughts,omitempty"`
	ThinkingBudget  *int32        `json:"thinkingBudget,omitempty"`
	ThinkingLevel   ThinkingLevel `json:"thinkingLevel,omitempty"`
}

// Type is a schema type in the API's OpenAPI-flavored enum form. Values are
// uppercase on the wire, unlike JSON Schema's lowercase.
type Type string

const (
	TypeString  Type = "STRING"
	TypeNumber  Type = "NUMBER"
	TypeInteger Type = "INTEGER"
	TypeBoolean Type = "BOOLEAN"
	TypeArray   Type = "ARRAY"
	TypeObject  Type = "OBJECT"
	TypeNULL    Type = "NULL"
)

// Schema is the typed schema used for structured output (responseSchema).
type Schema struct {
	Type        Type               `json:"type,omitempty"`
	Format      string             `json:"format,omitempty"`
	Title       string             `json:"title,omitempty"`
	Description string             `json:"description,omitempty"`
	Enum        []string           `json:"enum,omitempty"`
	Items       *Schema            `json:"items,omitempty"`
	Properties  map[string]*Schema `json:"properties,omitempty"`
	Required    []string           `json:"required,omitempty"`
}

// GenerationConfig holds per-request sampling and output settings.
type GenerationConfig struct {
	MaxOutputTokens  int32           `json:"maxOutputTokens,omitempty"`
	Temperature      *float32        `json:"temperature,omitempty"`
	ResponseMIMEType string          `json:"responseMimeType,omitempty"`
	ResponseSchema   *Schema         `json:"responseSchema,omitempty"`
	ThinkingConfig   *ThinkingConfig `json:"thinkingConfig,omitempty"`
}

// GenerateContentRequest is the body for generateContent and
// streamGenerateContent. SystemInstruction and Tools sit at the top level,
// not inside GenerationConfig.
type GenerateContentRequest struct {
	Contents          []*Content        `json:"contents,omitempty"`
	SystemInstruction *Content          `json:"systemInstruction,omitempty"`
	Tools             []*Tool           `json:"tools,omitempty"`
	GenerationConfig  *GenerationConfig `json:"generationConfig,omitempty"`
}

// FinishReason reports why a candidate stopped.
type FinishReason string

const (
	FinishReasonStop                   FinishReason = "STOP"
	FinishReasonMaxTokens              FinishReason = "MAX_TOKENS"
	FinishReasonSafety                 FinishReason = "SAFETY"
	FinishReasonRecitation             FinishReason = "RECITATION"
	FinishReasonBlocklist              FinishReason = "BLOCKLIST"
	FinishReasonProhibitedContent      FinishReason = "PROHIBITED_CONTENT"
	FinishReasonSPII                   FinishReason = "SPII"
	FinishReasonMalformedFunctionCall  FinishReason = "MALFORMED_FUNCTION_CALL"
	FinishReasonImageSafety            FinishReason = "IMAGE_SAFETY"
	FinishReasonImageProhibitedContent FinishReason = "IMAGE_PROHIBITED_CONTENT"
)

// Candidate is one generated completion. Only the first is requested.
type Candidate struct {
	Content      *Content     `json:"content,omitempty"`
	FinishReason FinishReason `json:"finishReason,omitempty"`
}

// UsageMetadata carries token accounting. On streams it appears on every
// chunk with cumulative values; the last chunk is authoritative.
// CandidatesTokenCount excludes thinking tokens, which the API reports
// separately as ThoughtsTokenCount.
type UsageMetadata struct {
	PromptTokenCount     int32 `json:"promptTokenCount,omitempty"`
	CandidatesTokenCount int32 `json:"candidatesTokenCount,omitempty"`
	ThoughtsTokenCount   int32 `json:"thoughtsTokenCount,omitempty"`
	TotalTokenCount      int32 `json:"totalTokenCount,omitempty"`
}

// GenerateContentResponse is a full response or one SSE chunk; the shapes
// are identical.
type GenerateContentResponse struct {
	Candidates    []*Candidate   `json:"candidates,omitempty"`
	UsageMetadata *UsageMetadata `json:"usageMetadata,omitempty"`
}

// EmbedContentRequest is one entry of a batchEmbedContents call. Model is
// duplicated into every entry (in "models/<name>" form) as the API requires;
// BatchEmbedContents fills it when left empty.
type EmbedContentRequest struct {
	Model                string   `json:"model,omitempty"`
	Content              *Content `json:"content,omitempty"`
	TaskType             string   `json:"taskType,omitempty"`
	OutputDimensionality *int32   `json:"outputDimensionality,omitempty"`
}

// BatchEmbedContentsRequest is the body for batchEmbedContents.
type BatchEmbedContentsRequest struct {
	Requests []*EmbedContentRequest `json:"requests"`
}

// ContentEmbedding is one embedding vector.
type ContentEmbedding struct {
	Values []float32 `json:"values,omitempty"`
}

// BatchEmbedContentsResponse mirrors the batchEmbedContents reply. The API
// returns no token usage on this endpoint.
type BatchEmbedContentsResponse struct {
	Embeddings []*ContentEmbedding `json:"embeddings,omitempty"`
}
