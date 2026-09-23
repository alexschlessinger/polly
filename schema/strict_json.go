package schema

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// DecodeJSON rejects duplicate keys at every depth and trailing values. A
// second verdict for the same finding must never silently replace the first.
func DecodeJSON(text string) (any, error) {
	d := json.NewDecoder(strings.NewReader(text))
	v, err := decodeValue(d, 0)
	if err != nil {
		return nil, decodeError(text, d, err)
	}
	end := d.InputOffset()
	if _, err := d.Token(); err != io.EOF {
		if err == nil {
			return nil, locateJSONError(text, errors.New("multiple JSON values"), end)
		}
		return nil, decodeError(text, d, err)
	}
	return v, nil
}

// jsonErrorContext is how many bytes on each side of a fault a located JSON
// error quotes.
const jsonErrorContext = 60

// LocateJSONError names where in text a positional JSON error occurred: the
// byte offset and a short quote around it. Errors without a position return
// unchanged. A model whose provider replays a failed call's arguments as an
// empty object cannot otherwise see where its JSON broke.
func LocateJSONError(text string, err error) error {
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		return locateJSONError(text, err, syntax.Offset)
	}
	var mismatch *json.UnmarshalTypeError
	if errors.As(err, &mismatch) {
		return locateJSONError(text, err, mismatch.Offset)
	}
	return err
}

// decodeError attaches a position to a decoder error: a syntax error carries
// its own offset, a truncated input ends at the text's end, and this
// package's own findings (duplicate key, nesting) sit at the decoder's
// current offset.
func decodeError(text string, d *json.Decoder, err error) error {
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		return locateJSONError(text, err, syntax.Offset)
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return locateJSONError(text, errors.New("unexpected end of JSON input"), int64(len(text)))
	}
	return locateJSONError(text, err, d.InputOffset())
}

func locateJSONError(text string, err error, offset int64) error {
	pos := int(min(max(offset, 0), int64(len(text))))
	start := max(pos-jsonErrorContext, 0)
	for start > 0 && !utf8.RuneStart(text[start]) {
		start--
	}
	end := min(pos+jsonErrorContext, len(text))
	for end < len(text) && !utf8.RuneStart(text[end]) {
		end++
	}
	snippet := text[start:end]
	if start > 0 {
		snippet = "..." + snippet
	}
	if end < len(text) {
		snippet += "..."
	}
	return fmt.Errorf("%w at byte %d near %q", err, pos, snippet)
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
