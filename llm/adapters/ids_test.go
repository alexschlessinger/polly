package adapters

import "testing"

// TestSyntheticToolCallIDsUniqueAcrossStreams guards against the ID collision
// that let denial stripping erase unrelated exchanges: every stream gets a
// fresh adapter, and each adapter must namespace its synthetic IDs.
func TestSyntheticToolCallIDsUniqueAcrossStreams(t *testing.T) {
	g1, g2 := NewGeminiAdapter(), NewGeminiAdapter()
	if g1.idPrefix == "" || g1.idPrefix == g2.idPrefix {
		t.Errorf("Gemini adapter prefixes not unique: %q vs %q", g1.idPrefix, g2.idPrefix)
	}

	o1, o2 := NewOllamaAdapter(), NewOllamaAdapter()
	if o1.idPrefix == "" || o1.idPrefix == o2.idPrefix {
		t.Errorf("Ollama adapter prefixes not unique: %q vs %q", o1.idPrefix, o2.idPrefix)
	}
}
