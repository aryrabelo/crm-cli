package value_test

import (
	"encoding/json"
	"testing"

	"github.com/jdanielnd/crm-cli/internal/value"
	"github.com/stretchr/testify/assert"
)

func TestLookup(t *testing.T) {
	doc, err := value.Decode([]byte(`{"results":[{"url":"https://a.com"}],"n":2}`))
	assert.NoError(t, err)
	v, ok := value.Lookup(doc, "results.0.url")
	assert.True(t, ok)
	assert.Equal(t, "https://a.com", v)
	_, ok = value.Lookup(doc, "results.1.url")
	assert.False(t, ok)
	_, ok = value.Lookup(doc, "n.x")
	assert.False(t, ok)
	v, ok = value.Lookup(doc, "")
	assert.True(t, ok)
	assert.Equal(t, doc, v)
}

func TestEmptyAndText(t *testing.T) {
	for _, v := range []any{nil, "", "  ", map[string]any{}, []any{}} {
		assert.True(t, value.Empty(v), "%#v", v)
	}
	for _, v := range []any{"x", 0.0, false, []any{nil}} {
		assert.False(t, value.Empty(v), "%#v", v)
	}
	assert.Equal(t, "72", value.Text(72.0))
	assert.Equal(t, "0.5", value.Text(0.5))
	assert.Equal(t, "true", value.Text(true))
	assert.Equal(t, `{"a":1}`, value.Text(map[string]any{"a": 1.0}))
	assert.Equal(t, "", value.Text(nil))
}

func TestDecodeKeepsLargeIntegers(t *testing.T) {
	doc, err := value.Decode([]byte(`{"id":9007199254740993,"f":1.5}`))
	assert.NoError(t, err)
	v, _ := value.Lookup(doc, "id")
	assert.Equal(t, json.Number("9007199254740993"), v)
	assert.Equal(t, "9007199254740993", value.Text(v))
	assert.False(t, value.Empty(v))
	out, err := json.Marshal(doc)
	assert.NoError(t, err)
	assert.JSONEq(t, `{"id":9007199254740993,"f":1.5}`, string(out))
	_, err = value.Decode([]byte(`{"a":1} {"b":2}`))
	assert.Error(t, err)
}

func TestTextDoesNotEscapeHTML(t *testing.T) {
	assert.Equal(t, `{"a":"<b>&"}`, value.Text(map[string]any{"a": "<b>&"}))
}
