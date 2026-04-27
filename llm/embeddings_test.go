package llm

import (
	"context"
	"math"
	"strings"
	"testing"
)

func TestEmbed_ValidatesRequest(t *testing.T) {
	_, err := Embed(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "request is required") {
		t.Fatalf("expected request validation error, got %v", err)
	}

	_, err = Embed(context.Background(), &EmbeddingRequest{})
	if err == nil || !strings.Contains(err.Error(), "model is required") {
		t.Fatalf("expected model validation error, got %v", err)
	}

	_, err = Embed(context.Background(), &EmbeddingRequest{Model: "openai/text-embedding-3-large"})
	if err == nil || !strings.Contains(err.Error(), "input is required") {
		t.Fatalf("expected input validation error, got %v", err)
	}
}

func TestEmbed_InvalidModelFormat(t *testing.T) {
	_, err := Embed(context.Background(), &EmbeddingRequest{
		Model: "text-embedding-3-large",
		Input: []string{"hello"},
	})
	if err == nil || !strings.Contains(err.Error(), "must include provider prefix") {
		t.Fatalf("expected provider prefix validation error, got %v", err)
	}
}

func TestEmbed_UnsupportedProvider(t *testing.T) {
	_, err := Embed(context.Background(), &EmbeddingRequest{
		Model:  "anthropic/some-embed-model",
		APIKey: "test-key",
		Input:  []string{"hello"},
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported embedding provider") {
		t.Fatalf("expected unsupported provider error, got %v", err)
	}
}

func TestEmbed_MissingAPIKey(t *testing.T) {
	t.Setenv("POLLYTOOL_OPENAIKEY", "")

	_, err := Embed(context.Background(), &EmbeddingRequest{
		Model: "openai/text-embedding-3-large",
		Input: []string{"hello"},
	})
	if err == nil || !strings.Contains(err.Error(), "POLLYTOOL_OPENAIKEY") {
		t.Fatalf("expected missing API key error, got %v", err)
	}
}

func TestResolveEmbeddingAPIKey_OpenAIBaseURLAllowsMissingKey(t *testing.T) {
	t.Setenv("POLLYTOOL_OPENAIKEY", "")

	key, err := resolveEmbeddingAPIKey("openai", "", "http://localhost:11434/v1")
	if err != nil {
		t.Fatalf("expected nil error for openai base url keyless mode, got %v", err)
	}
	if key != "" {
		t.Fatalf("expected empty key for keyless mode, got %q", key)
	}
}

func TestEmbed_OpenAIBaseURLDoesNotFailAPIKeyValidation(t *testing.T) {
	t.Setenv("POLLYTOOL_OPENAIKEY", "")

	_, err := Embed(context.Background(), &EmbeddingRequest{
		Model:   "openai/text-embedding-3-large",
		BaseURL: "bad-url",
		Input:   []string{"hello"},
	})
	if err == nil {
		t.Fatal("expected downstream request error, got nil")
	}
	if strings.Contains(err.Error(), "missing API key") {
		t.Fatalf("expected non-validation error, got %v", err)
	}
}

func TestEmbed_GeminiMissingAPIKey(t *testing.T) {
	t.Setenv("POLLYTOOL_GEMINIKEY", "")

	_, err := Embed(context.Background(), &EmbeddingRequest{
		Model: "gemini/gemini-embedding-001",
		Input: []string{"hello"},
	})
	if err == nil || !strings.Contains(err.Error(), "POLLYTOOL_GEMINIKEY") {
		t.Fatalf("expected missing API key error, got %v", err)
	}
}

func TestL2Normalize(t *testing.T) {
	v := []float64{3, 4}
	l2Normalize(v)
	if math.Abs(v[0]-0.6) > 1e-9 || math.Abs(v[1]-0.8) > 1e-9 {
		t.Fatalf("expected [0.6, 0.8], got %v", v)
	}

	z := []float64{0, 0, 0}
	l2Normalize(z)
	for i, x := range z {
		if x != 0 {
			t.Fatalf("expected zero vector unchanged at index %d, got %v", i, z)
		}
	}
}
