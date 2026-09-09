package schema

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// DecodeJSON rejects duplicate keys at every depth and trailing values. A
// second verdict for the same finding must never silently replace the first.
func DecodeJSON(text string) (any, error) {
	d := json.NewDecoder(strings.NewReader(text))
	v, err := decodeValue(d, 0)
	if err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return nil, err
	}
	return v, nil
}

func decodeValue(d *json.Decoder, depth int) (any, error) {
	if depth > 128 {
		return nil, fmt.Errorf("JSON nesting exceeds 128")
	}
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return t, nil
	}
	switch delim {
	case '{':
		obj := map[string]any{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return nil, err
			}
			k, ok := key.(string)
			if !ok {
				return nil, fmt.Errorf("expected object key")
			}
			if _, exists := obj[k]; exists {
				return nil, fmt.Errorf("duplicate JSON key %q", k)
			}
			v, err := decodeValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			obj[k] = v
		}
		_, err := d.Token()
		return obj, err
	case '[':
		arr := []any{}
		for d.More() {
			v, err := decodeValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		_, err := d.Token()
		return arr, err
	default:
		return nil, fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
}
