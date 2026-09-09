package schema

import "testing"

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
