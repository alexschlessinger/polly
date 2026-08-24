package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// newTestClient points a client at a test server.
func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := NewClient("test-key")
	client.baseURL = server.URL
	return client
}

// TestGenerateContentGoldenRequest pins the exact JSON sent to the API for a
// request exercising every field polly sets: system instruction and tools at
// the top level, sampling/thinking/schema inside generationConfig, and a
// history with a function call (thought signature attached), its response,
// and inline image data. With no SDK underneath, this test is the wire
// contract.
func TestGenerateContentGoldenRequest(t *testing.T) {
	var gotPath, gotKey, gotContentType string
	var gotBody map[string]any
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-goog-api-key")
		gotContentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &gotBody); err != nil {
			t.Errorf("request body is not valid JSON: %v", err)
		}
		w.Write([]byte(`{}`))
	})

	temp := float32(0.5)
	budget := int32(-1)
	_, err := client.GenerateContent(context.Background(), "gemini-2.5-flash", &GenerateContentRequest{
		Contents: []*Content{
			{Role: "user", Parts: []*Part{
				{Text: "hi"},
				{InlineData: &Blob{MIMEType: "image/png", Data: []byte{1, 2}}},
			}},
			{Role: "model", Parts: []*Part{{
				FunctionCall:     &FunctionCall{Name: "lookup", Args: map[string]any{"q": "x"}},
				ThoughtSignature: []byte("sig"),
			}}},
			{Role: "user", Parts: []*Part{{
				FunctionResponse: &FunctionResponse{Name: "lookup", Response: map[string]any{"result": "y"}},
			}}},
		},
		SystemInstruction: &Content{Parts: []*Part{{Text: "be brief"}}},
		Tools: []*Tool{{FunctionDeclarations: []*FunctionDeclaration{{
			Name:                 "lookup",
			Description:          "finds things",
			ParametersJsonSchema: map[string]any{"type": "object"},
		}}}},
		GenerationConfig: &GenerationConfig{
			MaxOutputTokens:  100,
			Temperature:      &temp,
			ResponseMIMEType: "application/json",
			ResponseSchema:   &Schema{Type: TypeObject, Properties: map[string]*Schema{"a": {Type: TypeString}}},
			ThinkingConfig:   &ThinkingConfig{IncludeThoughts: true, ThinkingBudget: &budget},
		},
	})
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}

	if gotPath != "/models/gemini-2.5-flash:generateContent" {
		t.Errorf("path = %q", gotPath)
	}
	if gotKey != "test-key" || gotContentType != "application/json" {
		t.Errorf("headers: key=%q content-type=%q", gotKey, gotContentType)
	}

	want := map[string]any{
		"contents": []any{
			map[string]any{"role": "user", "parts": []any{
				map[string]any{"text": "hi"},
				map[string]any{"inlineData": map[string]any{"mimeType": "image/png", "data": "AQI="}},
			}},
			map[string]any{"role": "model", "parts": []any{
				map[string]any{
					"functionCall":     map[string]any{"name": "lookup", "args": map[string]any{"q": "x"}},
					"thoughtSignature": "c2ln",
				},
			}},
			map[string]any{"role": "user", "parts": []any{
				map[string]any{"functionResponse": map[string]any{"name": "lookup", "response": map[string]any{"result": "y"}}},
			}},
		},
		"systemInstruction": map[string]any{"parts": []any{map[string]any{"text": "be brief"}}},
		"tools": []any{map[string]any{"functionDeclarations": []any{map[string]any{
			"name":                 "lookup",
			"description":          "finds things",
			"parametersJsonSchema": map[string]any{"type": "object"},
		}}}},
		"generationConfig": map[string]any{
			"maxOutputTokens":  float64(100),
			"temperature":      float64(0.5),
			"responseMimeType": "application/json",
			"responseSchema": map[string]any{
				"type":       "OBJECT",
				"properties": map[string]any{"a": map[string]any{"type": "STRING"}},
			},
			"thinkingConfig": map[string]any{"includeThoughts": true, "thinkingBudget": float64(-1)},
		},
	}
	if !reflect.DeepEqual(gotBody, want) {
		gotJSON, _ := json.MarshalIndent(gotBody, "", "  ")
		wantJSON, _ := json.MarshalIndent(want, "", "  ")
		t.Errorf("request body mismatch\ngot:\n%s\nwant:\n%s", gotJSON, wantJSON)
	}
}

// TestGenerateContentStream verifies SSE parsing: data chunks with and
// without the customary space, blank and comment lines skipped, and base64
// thought signatures decoded back to bytes.
func TestGenerateContentStream(t *testing.T) {
	var gotPath string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(
			": keep-alive comment\n" +
				"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"thinking\",\"thought\":true}]}}]}\n" +
				"\n" +
				"data:{\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"f\",\"args\":{\"a\":1}},\"thoughtSignature\":\"c2ln\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":7}}\n" +
				"\n"))
	})

	var chunks []*GenerateContentResponse
	for chunk, err := range client.GenerateContentStream(context.Background(), "gemini-2.5-flash", &GenerateContentRequest{}) {
		if err != nil {
			t.Fatalf("stream error: %v", err)
		}
		chunks = append(chunks, chunk)
	}

	if gotPath != "/models/gemini-2.5-flash:streamGenerateContent?alt=sse" {
		t.Errorf("path = %q", gotPath)
	}
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2", len(chunks))
	}
	if p := chunks[0].Candidates[0].Content.Parts[0]; p.Text != "thinking" || !p.Thought {
		t.Errorf("first chunk part = %+v", p)
	}
	p := chunks[1].Candidates[0].Content.Parts[0]
	if p.FunctionCall == nil || p.FunctionCall.Name != "f" {
		t.Fatalf("second chunk part = %+v", p)
	}
	if string(p.ThoughtSignature) != "sig" {
		t.Errorf("thought signature = %q, want decoded bytes", p.ThoughtSignature)
	}
	if chunks[1].Candidates[0].FinishReason != FinishReasonStop {
		t.Errorf("finish reason = %q", chunks[1].Candidates[0].FinishReason)
	}
	if u := chunks[1].UsageMetadata; u.PromptTokenCount != 3 || u.CandidatesTokenCount != 7 {
		t.Errorf("usage = %+v", u)
	}
}

// TestGenerateContentStreamMidStreamError verifies that a bare error envelope
// between data events surfaces as *APIError, matching how the API reports
// failures after streaming has begun.
func TestGenerateContentStreamMidStreamError(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(
			"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"partial\"}]}}]}\n\n" +
				"{\"error\":{\"code\":429,\"message\":\"quota\",\"status\":\"RESOURCE_EXHAUSTED\"}}\n"))
	})

	var texts []string
	var streamErr error
	for chunk, err := range client.GenerateContentStream(context.Background(), "m", &GenerateContentRequest{}) {
		if err != nil {
			streamErr = err
			break
		}
		texts = append(texts, chunk.Candidates[0].Content.Parts[0].Text)
	}

	if len(texts) != 1 || texts[0] != "partial" {
		t.Errorf("texts before error = %v", texts)
	}
	var apiErr *APIError
	if !errors.As(streamErr, &apiErr) {
		t.Fatalf("stream error = %v, want *APIError", streamErr)
	}
	if apiErr.Code != 429 || apiErr.Status != "RESOURCE_EXHAUSTED" {
		t.Errorf("apiErr = %+v", apiErr)
	}
}

// TestErrorEnvelope verifies non-2xx handling for both the standard JSON
// envelope and a non-JSON body.
func TestErrorEnvelope(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"code":400,"message":"bad schema","status":"INVALID_ARGUMENT"}}`))
	})
	_, err := client.GenerateContent(context.Background(), "m", &GenerateContentRequest{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.Code != 400 || apiErr.Message != "bad schema" || apiErr.Status != "INVALID_ARGUMENT" {
		t.Errorf("apiErr = %+v", apiErr)
	}

	plain := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte("upstream fell over"))
	})
	_, err = plain.GenerateContent(context.Background(), "m", &GenerateContentRequest{})
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.Code != http.StatusBadGateway || apiErr.Message != "upstream fell over" {
		t.Errorf("apiErr = %+v", apiErr)
	}
}

// TestBatchEmbedContents verifies the batch body shape the API requires:
// every entry carries the model in resource form plus its own taskType and
// outputDimensionality.
func TestBatchEmbedContents(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotBody)
		w.Write([]byte(`{"embeddings":[{"values":[0.1,0.2]},{"values":[0.3]}]}`))
	})

	dim := int32(64)
	resp, err := client.BatchEmbedContents(context.Background(), "gemini-embedding-001", []*EmbedContentRequest{
		{Content: &Content{Parts: []*Part{{Text: "a"}}}, TaskType: "RETRIEVAL_QUERY", OutputDimensionality: &dim},
		{Content: &Content{Parts: []*Part{{Text: "b"}}}, TaskType: "RETRIEVAL_QUERY", OutputDimensionality: &dim},
	})
	if err != nil {
		t.Fatalf("BatchEmbedContents: %v", err)
	}

	if gotPath != "/models/gemini-embedding-001:batchEmbedContents" {
		t.Errorf("path = %q", gotPath)
	}
	entry := map[string]any{
		"model":                "models/gemini-embedding-001",
		"content":              map[string]any{"parts": []any{map[string]any{"text": "a"}}},
		"taskType":             "RETRIEVAL_QUERY",
		"outputDimensionality": float64(64),
	}
	entryB := map[string]any{
		"model":                "models/gemini-embedding-001",
		"content":              map[string]any{"parts": []any{map[string]any{"text": "b"}}},
		"taskType":             "RETRIEVAL_QUERY",
		"outputDimensionality": float64(64),
	}
	want := map[string]any{"requests": []any{entry, entryB}}
	if !reflect.DeepEqual(gotBody, want) {
		gotJSON, _ := json.MarshalIndent(gotBody, "", "  ")
		t.Errorf("body mismatch:\n%s", gotJSON)
	}
	if len(resp.Embeddings) != 2 || len(resp.Embeddings[0].Values) != 2 {
		t.Errorf("resp = %+v", resp)
	}
}

// TestModelPath covers resource-name normalization.
func TestModelPath(t *testing.T) {
	tests := []struct{ in, want string }{
		{"gemini-2.5-flash", "models/gemini-2.5-flash"},
		{"models/gemini-2.5-flash", "models/gemini-2.5-flash"},
		{"tunedModels/mine", "tunedModels/mine"},
	}
	for _, tc := range tests {
		if got := modelPath(tc.in); got != tc.want {
			t.Errorf("modelPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
