package tools

import (
	"encoding/json"
	"errors"
)

// ToolError is a structured error returned by tools. When a tool returns
// a *ToolError from Execute, the agent serializes it as JSON with "error"
// and "code" fields instead of the generic "Error: ..." format.
type ToolError struct {
	Message string `json:"error"`
	Code    string `json:"code,omitempty"`
}

func (e *ToolError) Error() string { return e.Message }

// NewToolError creates a structured tool error with message and code.
func NewToolError(message, code string) *ToolError {
	return &ToolError{Message: message, Code: code}
}

// FormatToolError checks whether err wraps a *ToolError and, if so,
// returns the JSON-serialized error string. Returns ("", false) otherwise.
func FormatToolError(err error) (string, bool) {
	var toolErr *ToolError
	if errors.As(err, &toolErr) {
		return Error(toolErr.Message, toolErr.Code), true
	}
	return "", false
}

// Error marshals a tool error as JSON: {"error": message, "code": code}.
func Error(message, code string) string {
	b, err := json.Marshal(&ToolError{Message: message, Code: code})
	if err != nil {
		return `{"error":"failed to encode error","code":"ENCODE_ERROR"}`
	}
	return string(b)
}
