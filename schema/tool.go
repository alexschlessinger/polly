package schema

import "encoding/json"

// ToolSchema describes a tool's name, description, and input parameters.
type ToolSchema struct {
	Raw map[string]any
}

// Title returns the tool name, or "" if absent.
func (s *ToolSchema) Title() string {
	if s == nil {
		return ""
	}
	if t, ok := s.Raw["title"].(string); ok {
		return t
	}
	return ""
}

// SetTitle sets the tool name.
func (s *ToolSchema) SetTitle(t string) {
	if s != nil && s.Raw != nil {
		s.Raw["title"] = t
	}
}

// Description returns the tool description, or "" if absent.
func (s *ToolSchema) Description() string {
	if s == nil {
		return ""
	}
	if d, ok := s.Raw["description"].(string); ok {
		return d
	}
	return ""
}

// Copy returns a shallow copy of the schema (safe to mutate top-level keys).
func (s *ToolSchema) Copy() *ToolSchema {
	if s == nil {
		return nil
	}
	raw := make(map[string]any, len(s.Raw))
	for k, v := range s.Raw {
		raw[k] = v
	}
	return &ToolSchema{Raw: raw}
}

// Properties returns the tool's parameter definitions, or nil if absent.
func (s *ToolSchema) Properties() map[string]any {
	if s == nil {
		return nil
	}
	if p, ok := s.Raw["properties"].(map[string]any); ok {
		return p
	}
	return nil
}

// Required returns the tool's required parameter names.
// Handles both []string and []any (from JSON unmarshal).
func (s *ToolSchema) Required() []string {
	if s == nil {
		return nil
	}
	switch req := s.Raw["required"].(type) {
	case []string:
		return req
	case []any:
		out := make([]string, 0, len(req))
		for _, r := range req {
			if str, ok := r.(string); ok {
				out = append(out, str)
			}
		}
		return out
	default:
		return nil
	}
}

// ToolSchemaFromJSON unmarshals JSON bytes into a ToolSchema.
// Returns nil if data is empty or invalid.
func ToolSchemaFromJSON(data []byte) *ToolSchema {
	if len(data) == 0 {
		return nil
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil
	}
	return &ToolSchema{Raw: raw}
}

// ToolSchemaFromString parses a JSON string into a ToolSchema.
// Returns nil if the string is empty or invalid.
func ToolSchemaFromString(s string) *ToolSchema {
	if s == "" {
		return nil
	}
	return ToolSchemaFromJSON([]byte(s))
}

// Params is a map of parameter names to their JSON schema definitions.
type Params = map[string]any

// Tool builds a tool schema with type "object".
func Tool(title, description string, params Params, required ...string) *ToolSchema {
	raw := map[string]any{
		"title":       title,
		"description": description,
		"type":        "object",
	}
	if params != nil {
		raw["properties"] = params
	} else {
		raw["properties"] = Params{}
	}
	if len(required) > 0 {
		raw["required"] = required
	}
	return &ToolSchema{Raw: raw}
}

// S builds a string parameter.
func S(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

// Int builds an integer parameter.
func Int(description string) map[string]any {
	return map[string]any{"type": "integer", "description": description}
}

// Bool builds a boolean parameter.
func Bool(description string) map[string]any {
	return map[string]any{"type": "boolean", "description": description}
}

// Enum builds a string parameter constrained to the given values.
func Enum(description string, values ...string) map[string]any {
	enum := make([]any, len(values))
	for i, v := range values {
		enum[i] = v
	}
	return map[string]any{"type": "string", "description": description, "enum": enum}
}

// Strings builds an array-of-strings parameter.
func Strings(description string) map[string]any {
	return map[string]any{"type": "array", "description": description, "items": map[string]any{"type": "string"}}
}

// Array builds an array parameter with the given item schema.
func Array(description string, items map[string]any) map[string]any {
	return map[string]any{"type": "array", "description": description, "items": items}
}
