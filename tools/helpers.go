package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// NativeTool provides default GetType and GetSource implementations
// for native Go tools. Embed it to avoid repeating boilerplate:
//
//	type myTool struct {
//	    tools.NativeTool
//	    // ...
//	}
type NativeTool struct{}

func (NativeTool) GetType() string   { return "native" }
func (NativeTool) GetSource() string { return "builtin" }

// resolveLocalPath expands a leading tilde and makes path absolute against
// the process working directory.
func resolveLocalPath(path string) (string, error) {
	if strings.HasPrefix(path, "~/") || path == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		path = filepath.Join(home, strings.TrimPrefix(path[1:], "/"))
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve path %s: %w", path, err)
	}
	return abs, nil
}

// Result marshals v as a JSON string for tool results.
// Falls back to an error JSON on marshal failure.
func Result(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return Error("failed to encode tool result", "ENCODE_ERROR")
	}
	return string(b)
}
