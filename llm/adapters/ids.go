package adapters

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
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

// SyntheticCallID names a tool call the provider left unnamed: the nth call
// of a stream whose adapter holds nonce (from randomIDPrefix). The shape is
// polly's own so replay can tell it from a server-issued ID.
func SyntheticCallID(provider, nonce string, n int) string {
	return fmt.Sprintf("%s_call_%s_%d", provider, nonce, n)
}

// syntheticCallIDPattern matches every ID shape polly has synthesized: the
// current <provider>_call_<nonce>_<n>, and the earlier Ollama call_<nonce>_<n>
// and Gemini gemini-<nonce>-<n> still present in saved sessions.
var syntheticCallIDPattern = regexp.MustCompile(`^(?:[a-z]+_call_[0-9a-f]+_[0-9]+|call_[0-9a-f]{8}_[0-9]+|gemini-[0-9a-f]+-[0-9]+)$`)

// IsSyntheticCallID reports whether id was synthesized by polly rather than
// issued by a provider.
func IsSyntheticCallID(id string) bool {
	return syntheticCallIDPattern.MatchString(id)
}

// NativeCallID returns the provider-issued call ID, or "" when polly
// synthesized it: a synthetic ID pairs calls with results internally and
// must never be echoed back to the provider.
func NativeCallID(id string) string {
	if IsSyntheticCallID(id) {
		return ""
	}
	return id
}
