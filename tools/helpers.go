package tools

import "encoding/json"

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

// Result marshals v as a JSON string for tool results.
// Falls back to an error JSON on marshal failure.
func Result(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return Error("failed to encode tool result", "ENCODE_ERROR")
	}
	return string(b)
}
