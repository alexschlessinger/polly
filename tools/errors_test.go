package tools

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestToolError_ImplementsError(t *testing.T) {
	var err error = NewToolError("something broke", "BROKEN")
	if err.Error() != "something broke" {
		t.Errorf("Error() = %q, want %q", err.Error(), "something broke")
	}
}

func TestToolError_ErrorsAs(t *testing.T) {
	err := NewToolError("not found", "NOT_FOUND")
	var wrapped error = err

	var target *ToolError
	if !errors.As(wrapped, &target) {
		t.Fatal("errors.As failed to unwrap *ToolError")
	}
	if target.Code != "NOT_FOUND" {
		t.Errorf("Code = %q, want NOT_FOUND", target.Code)
	}
}

func TestToolError_JSON(t *testing.T) {
	te := NewToolError("bad input", "INVALID_ARGUMENT")
	b, err := json.Marshal(te)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if m["error"] != "bad input" {
		t.Errorf("error = %q, want %q", m["error"], "bad input")
	}
	if m["code"] != "INVALID_ARGUMENT" {
		t.Errorf("code = %q, want %q", m["code"], "INVALID_ARGUMENT")
	}
}

func TestToolError_EmptyCode(t *testing.T) {
	te := NewToolError("oops", "")
	b, _ := json.Marshal(te)
	var m map[string]any
	json.Unmarshal(b, &m)
	if _, ok := m["code"]; ok {
		t.Error("expected code to be omitted when empty")
	}
}

func TestError_Helper(t *testing.T) {
	result := Error("timeout", "TIMEOUT")
	var m map[string]string
	if err := json.Unmarshal([]byte(result), &m); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if m["error"] != "timeout" {
		t.Errorf("error = %q, want timeout", m["error"])
	}
	if m["code"] != "TIMEOUT" {
		t.Errorf("code = %q, want TIMEOUT", m["code"])
	}
}
