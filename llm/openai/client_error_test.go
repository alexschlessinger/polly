package openai

import (
	"encoding/json"
	"strings"
	"testing"
)

// A compatible server's error metadata may come in any shape: the error
// still decodes, and OpenRouter's error_type still labels it.
func TestAPIErrorDecodesAnyMetadata(t *testing.T) {
	for raw, label := range map[string]string{
		`{"code":"bad_request","message":"too long","metadata":{"error_type":"context_length_exceeded"}}`: "context_length_exceeded",
		`{"code":"bad_request","message":"too long","metadata":"gateway-x"}`:                              "bad_request",
		`{"code":"bad_request","message":"too long","metadata":{"error_type":42}}`:                        "bad_request",
		`{"code":"bad_request","message":"too long","metadata":["a"]}`:                                    "bad_request",
	} {
		var apiErr *APIError
		if err := json.Unmarshal([]byte(raw), &apiErr); err != nil || apiErr == nil {
			t.Fatalf("%s: decode = %v", raw, err)
		}
		if got := apiErr.Error(); !strings.Contains(got, "("+label+")") || !strings.Contains(got, "too long") {
			t.Fatalf("%s: error = %q, want label %q", raw, got, label)
		}
	}
}
