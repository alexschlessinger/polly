package schema

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDecodeJSONRejectsAmbiguousValues(t *testing.T) {
	for _, raw := range []string{`{"id":1,"id":2}`, `{"nested":{"id":1,"id":2}}`, `[{"id":1,"id":2}]`, `{} {}`, `[1,]`} {
		if _, err := DecodeJSON(raw); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if _, err := DecodeJSON(`{"a":{"id":1},"b":{"id":2}}`); err != nil {
		t.Fatal(err)
	}
}

// Decoder errors name the byte where the JSON broke and quote the text
// around it; errors without a position pass through unchanged.
func TestLocateJSONError(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{`[1,]`, "near"},
		{`{"a":`, "unexpected end of JSON input at byte 5"},
		{`{"id":1,"id":2}`, `duplicate JSON key "id"`},
		{`{} {}`, "multiple JSON values"},
	} {
		_, err := DecodeJSON(tc.raw)
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "at byte ") {
			t.Fatalf("%s: %v", tc.raw, err)
		}
	}
	if _, err := DecodeJSON(`{"é":[1,]}`); err == nil || !utf8.ValidString(err.Error()) || strings.Contains(err.Error(), `\x`) {
		t.Fatalf("snippet is not clean UTF-8: %v", err)
	}
	plain := errors.New("plain")
	if got := LocateJSONError("x", plain); got != plain {
		t.Fatalf("non-positional error changed: %v", got)
	}
}
