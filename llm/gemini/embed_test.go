package gemini

import (
	"math"
	"testing"
)

func TestIsGemini2EmbedModel(t *testing.T) {
	cases := map[string]bool{
		"gemini-embedding-2":             true,
		"gemini-embedding-2-exp-11-2025": true,
		"gemini-embedding-001":           false,
		"text-embedding-004":             false,
		"":                               false,
	}
	for model, want := range cases {
		if got := isGemini2EmbedModel(model); got != want {
			t.Errorf("isGemini2EmbedModel(%q) = %v, want %v", model, got, want)
		}
	}
}

func TestGemini2TaskPrefix(t *testing.T) {
	cases := map[string]string{
		"RETRIEVAL_QUERY":      "task: search result | query: ",
		"RETRIEVAL_DOCUMENT":   "title: none | text: ",
		"SEMANTIC_SIMILARITY":  "task: sentence similarity | query: ",
		"CLASSIFICATION":       "task: classification | query: ",
		"CLUSTERING":           "task: clustering | query: ",
		"QUESTION_ANSWERING":   "task: question answering | query: ",
		"FACT_VERIFICATION":    "task: fact checking | query: ",
		"CODE_RETRIEVAL_QUERY": "task: code retrieval | query: ",
		"retrieval_query":      "task: search result | query: ", // case-insensitive
	}
	for taskType, want := range cases {
		got, err := gemini2TaskPrefix(taskType)
		if err != nil {
			t.Errorf("gemini2TaskPrefix(%q) errored: %v", taskType, err)
			continue
		}
		if got != want {
			t.Errorf("gemini2TaskPrefix(%q) = %q, want %q", taskType, got, want)
		}
	}

	if _, err := gemini2TaskPrefix("MADE_UP"); err == nil {
		t.Error("expected error for unknown task type, got nil")
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
