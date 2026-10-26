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
	existing := []*column.Column{{Name: "login", Type: column.Input}}
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
	_, err := column.ParseType("ai")
	assert.ErrorIs(t, err, model.ErrValidation)
}
