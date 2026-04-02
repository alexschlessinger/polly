package tools

import (
	"strings"
	"testing"
)

func TestResult_MarshalFailure(t *testing.T) {
	// Channels can't be marshaled
	got := Result(make(chan int))
	if !strings.Contains(got, "ENCODE_ERROR") {
		t.Errorf("expected ENCODE_ERROR fallback, got: %s", got)
	}
}
