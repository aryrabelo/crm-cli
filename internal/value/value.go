// Package value holds the helpers shared by every data-plane package for cell
// values decoded from JSON (nil, string, float64, bool, []any, map[string]any):
// dotted-path lookup, emptiness and the text form used in CSV and filters.
package value

import (
	"encoding/json"
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
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// Decode parses JSON into the generic representation used by Lookup.
func Decode(raw []byte) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return v, nil
}
