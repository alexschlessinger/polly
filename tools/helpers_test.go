package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNativeTool_Defaults(t *testing.T) {
	nt := NativeTool{}
	if got := nt.GetType(); got != "native" {
		t.Errorf("GetType() = %q, want native", got)
	}
	if got := nt.GetSource(); got != "builtin" {
		t.Errorf("GetSource() = %q, want builtin", got)
	}
}

func TestNativeTool_Embedding(t *testing.T) {
	type myTool struct {
		NativeTool
	}
	mt := myTool{}
	if mt.GetType() != "native" {
		t.Error("embedded GetType failed")
	}
	if mt.GetSource() != "builtin" {
		t.Error("embedded GetSource failed")
	}
}

func TestNativeTool_SourceOverride(t *testing.T) {
	type customTool struct {
		NativeTool
		source string
	}
	// Verify the struct-level method can override
	ct := customTool{source: "my-app"}
	// NativeTool's GetSource still returns "builtin" — override requires a method
	if ct.GetSource() != "builtin" {
		t.Error("without method override, should return builtin")
	}
}

func TestResult_Struct(t *testing.T) {
	type response struct {
		Count int    `json:"count"`
		Name  string `json:"name"`
	}
	got := Result(response{Count: 3, Name: "test"})
	var m map[string]any
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("Result produced invalid JSON: %v", err)
	}
	if m["count"] != float64(3) || m["name"] != "test" {
		t.Errorf("unexpected result: %s", got)
	}
}

func TestResult_Slice(t *testing.T) {
	got := Result([]string{"a", "b"})
	if got != `["a","b"]` {
		t.Errorf("Result(slice) = %s", got)
	}
}

func TestResult_MarshalFailure(t *testing.T) {
	// Channels can't be marshaled
	got := Result(make(chan int))
	if !strings.Contains(got, "ENCODE_ERROR") {
		t.Errorf("expected ENCODE_ERROR fallback, got: %s", got)
	}
}
