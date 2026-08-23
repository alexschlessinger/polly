package adapters

import (
	"crypto/rand"
	"encoding/hex"
)

// randomIDPrefix returns a short random token used to namespace synthetic
// tool-call IDs. Providers that don't supply call IDs (Gemini, Ollama) get
// per-position IDs; without a per-stream prefix those repeat across LLM calls
// ("gemini-0" in every response), which corrupts ID-keyed history operations
// such as denial stripping.
func randomIDPrefix() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "0000"
	}
	return hex.EncodeToString(b[:])
}
