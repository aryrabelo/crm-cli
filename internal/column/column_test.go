package column_test

import (
	"testing"

	"github.com/jdanielnd/crm-cli/internal/column"
	"github.com/jdanielnd/crm-cli/internal/model"
	"github.com/stretchr/testify/assert"
)

func TestMaterialize_FormulasInOrder(t *testing.T) {
	cols := []*column.Column{
		{Name: "first", Type: column.Input},
		{Name: "a.b", Type: column.Input},
		{Name: "profile", Type: column.Enrich},
		{Name: "full", Type: column.Formula, Config: column.Config{Expr: "{{first}} @ {{profile.company}}"}},
		{Name: "tag", Type: column.Formula, Config: column.Config{Expr: "[{{full}}|{{a.b}}|{{profile.missing}}]"}},
	}
	data := map[string]any{"first": "Ana", "a.b": "dotted", "profile": map[string]any{"company": "Acme"}, "extra": "x"}
	cells := column.Materialize(cols, data)
	assert.Equal(t, "Ana @ Acme", cells["full"])
	assert.Equal(t, "[Ana @ Acme|dotted|]", cells["tag"])
	assert.NotContains(t, cells, "extra")
	assert.NotContains(t, data, "full", "input map untouched")
}

func TestValidate(t *testing.T) {
	existing := []*column.Column{{Name: "login", Type: column.Input}, {Name: "why", Type: column.Input}, {Name: "f0", Type: column.Formula}}
	ai := func(prompt, output, reason string) column.Column {
		return column.Column{Name: "fit", Type: column.AI, Config: column.Config{
			Waterfall: []string{"llm"}, Prompt: prompt, Output: output, Reason: reason}}
	}
	cases := []struct {
		name string
		col  column.Column
		want error
	}{
		{"enrich ok", column.Column{Name: "p", Type: column.Enrich, Config: column.Config{Waterfall: []string{"github"}, From: "login"}}, nil},
		{"enrich no waterfall", column.Column{Name: "p", Type: column.Enrich, Config: column.Config{From: "login"}}, model.ErrValidation},
		{"enrich bad from", column.Column{Name: "p", Type: column.Enrich, Config: column.Config{Waterfall: []string{"x"}, From: "nope"}}, model.ErrValidation},
		{"formula ok", column.Column{Name: "f", Type: column.Formula, Config: column.Config{Expr: "{{login}}!"}}, nil},
		{"formula no expr", column.Column{Name: "f", Type: column.Formula}, model.ErrValidation},
		{"input with config", column.Column{Name: "i", Type: column.Input, Config: column.Config{From: "login"}}, model.ErrValidation},
		{"duplicate", column.Column{Name: "login", Type: column.Input}, model.ErrConflict},
		{"ai ok", ai("Rate {{login}}", "score:0-100", ""), nil},
		{"ai with reason", ai("Rate {{login}}", "enum:yes|no", "why"), nil},
		{"ai unknown placeholder", ai("Rate {{company}}", "text", ""), model.ErrValidation},
		{"ai no placeholder", ai("Rate this", "text", ""), model.ErrValidation},
		{"ai no prompt", ai("", "text", ""), model.ErrValidation},
		{"ai bad output", ai("Rate {{login}}", "score:9-1", ""), model.ErrValidation},
		{"ai reason not input", ai("Rate {{login}}", "text", "f0"), model.ErrValidation},
		{"ai reason missing", ai("Rate {{login}}", "text", "nope"), model.ErrValidation},
		{"ai no provider", column.Column{Name: "fit", Type: column.AI, Config: column.Config{Prompt: "{{login}}", Output: "text"}}, model.ErrValidation},
		{"enrich with prompt", column.Column{Name: "p", Type: column.Enrich, Config: column.Config{Waterfall: []string{"x"}, From: "login", Prompt: "{{login}}"}}, model.ErrValidation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.col.Validate(existing)
			if tc.want == nil {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, tc.want)
			}
		})
	}
	_, err := column.ParseType("signal")
	assert.ErrorIs(t, err, model.ErrValidation)
}

func TestParseOutput(t *testing.T) {
	for _, ok := range []string{"text", "bool", "score:0-100", "score:-1-1", "score:0.5-2.5", "enum:yes|no|maybe"} {
		_, err := column.ParseOutput(ok)
		assert.NoError(t, err, ok)
	}
	for _, bad := range []string{"", "number", "text:x", "score", "score:100-0", "score:a-b", "enum:", "enum:a", "enum:a||b", "enum:a|A"} {
		_, err := column.ParseOutput(bad)
		assert.ErrorIs(t, err, model.ErrValidation, bad)
	}
	spec, err := column.ParseOutput("score:0-100")
	assert.NoError(t, err)
	assert.Equal(t, &column.OutputSpec{Kind: "score", Min: 0, Max: 100}, spec)
}

func TestAIInputCellsSources(t *testing.T) {
	cols := []*column.Column{{Name: "company", Type: column.Input}, {Name: "site", Type: column.Input}}
	c := &column.Column{Name: "fit", Type: column.AI, Config: column.Config{
		Prompt: "Is {{company}} ({{site}}) a fit?", Output: "bool", Reason: "fit_reason", Waterfall: []string{"llm"}}}
	assert.Equal(t, []string{"company", "site"}, c.Sources(cols))

	in := c.Input(column.Materialize(cols, map[string]any{"company": "Acme", "site": ""}))
	assert.Equal(t, "Is Acme () a fit?", in.Value)
	in = c.Input(column.Materialize(cols, map[string]any{"company": " "}))
	assert.Empty(t, in.Value, "every placeholder empty: no input, no call")

	assert.Equal(t, map[string]any{"fit": true, "fit_reason": "Acme sells to SMBs."},
		c.Cells(map[string]any{"value": true, "reason": "Acme sells to SMBs."}))
	assert.Equal(t, map[string]any{"fit": nil, "fit_reason": nil}, c.Cells(nil))
	enrich := &column.Column{Name: "p", Type: column.Enrich, Config: column.Config{From: "company"}}
	assert.Equal(t, map[string]any{"p": "x"}, enrich.Cells("x"))
}
