package tools

// Args wraps map[string]any with typed accessors for tool arguments.
// JSON unmarshals numbers as float64, so direct type assertions on int
// will panic. Use Args helpers instead.
//
// Usage: args := tools.Args(rawArgs)
type Args map[string]any

// String returns the string value for key, or empty string if missing or wrong type.
func (a Args) String(key string) string {
	v, ok := a[key].(string)
	if !ok {
		return ""
	}
	return v
}

// Int returns the integer value for key, handling float64 (from JSON),
// int, and int64 types. Returns defaultValue if missing or wrong type.
func (a Args) Int(key string, defaultValue int) int {
	raw, ok := a[key]
	if !ok {
		return defaultValue
	}
	switch v := raw.(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	default:
		return defaultValue
	}
}

// Float returns the float64 value for key. Returns defaultValue if missing or wrong type.
func (a Args) Float(key string, defaultValue float64) float64 {
	raw, ok := a[key]
	if !ok {
		return defaultValue
	}
	switch v := raw.(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	default:
		return defaultValue
	}
}

// Bool returns the boolean value for key, or false if missing or wrong type.
func (a Args) Bool(key string) bool {
	v, ok := a[key].(bool)
	return ok && v
}

// StringSlice returns a deduplicated string slice for key,
// handling both []any (from JSON) and []string. Skips empty strings.
func (a Args) StringSlice(key string) []string {
	var items []string
	switch v := a[key].(type) {
	case []any:
		items = make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				items = append(items, s)
			}
		}
	case []string:
		items = v
	default:
		return nil
	}
	return appendUniqueStrings(make([]string, 0, len(items)), items)
}
