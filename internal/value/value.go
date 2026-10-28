// Package value holds the helpers shared by every data-plane package for cell
// values decoded from JSON (nil, string, json.Number, bool, []any, map[string]any):
// dotted-path lookup, emptiness and the text form used in CSV and filters.
package value

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
)

// Lookup walks a dotted path ("results.0.url") through maps and slices.
// An empty path returns v itself. ok is false when a segment is missing.
func Lookup(v any, path string) (any, bool) {
	if path == "" {
		return v, true
	}
	for _, seg := range strings.Split(path, ".") {
		switch node := v.(type) {
		case map[string]any:
			next, ok := node[seg]
			if !ok {
				return nil, false
			}
			v = next
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(node) {
				return nil, false
			}
			v = node[i]
		default:
			return nil, false
		}
	}
	return v, true
}

// Empty reports whether v carries no data: nil, a blank string, or an empty
// object or array.
func Empty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(x) == ""
	case map[string]any:
		return len(x) == 0
	case []any:
		return len(x) == 0
	default:
		return false
	}
}

// Text renders v as a single cell: strings as-is, nil as "", numbers and
// booleans in JSON notation, objects and arrays as compact JSON.
func Text(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	default:
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(x); err != nil {
			return ""
		}
		return strings.TrimSuffix(buf.String(), "\n")
	}
}

// Decode parses JSON into the generic representation used by Lookup. Numbers
// stay json.Number so integers above 2^53 keep their exact digits.
func Decode(raw []byte) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("invalid JSON: trailing data")
	}
	return v, nil
}
