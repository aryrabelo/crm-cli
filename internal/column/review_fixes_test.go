package column

import (
	"strings"
	"testing"

	"github.com/jdanielnd/crm-cli/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseType_AI(t *testing.T) {
	ty, err := ParseType("ai")
	require.NoError(t, err)
	assert.Equal(t, AI, ty)
}

func TestValidate_RejectsSurroundingWhitespaceInName(t *testing.T) {
	err := (&Column{Name: " last", Type: Input}).Validate(nil)
	assert.ErrorIs(t, err, model.ErrValidation)
	err = (&Column{Name: "last ", Type: Input}).Validate(nil)
	assert.ErrorIs(t, err, model.ErrValidation)
	assert.NoError(t, (&Column{Name: "first last", Type: Input}).Validate(nil))
}

func TestValidate_RejectsDuplicateWaterfallProvider(t *testing.T) {
	existing := []*Column{{Name: "name", Type: Input}}
	enrich := &Column{Name: "e", Type: Enrich, Config: Config{Waterfall: []string{"a", "b", "a"}, From: "name"}}
	assert.ErrorIs(t, enrich.Validate(existing), model.ErrValidation)
	ai := &Column{Name: "x", Type: AI, Config: Config{Waterfall: []string{"llm", "llm"}, Prompt: "{{name}}", Output: "bool"}}
	assert.ErrorIs(t, ai.Validate(existing), model.ErrValidation)
}

func TestValidate_DottedNameCannotShadowExistingReference(t *testing.T) {
	existing := []*Column{
		{Name: "profile", Type: Input},
		{Name: "f", Type: Formula, Config: Config{Expr: "{{profile.company}}"}},
	}
	err := (&Column{Name: "profile.company", Type: Input}).Validate(existing)
	assert.ErrorIs(t, err, model.ErrConflict)
	assert.NoError(t, (&Column{Name: "other.company", Type: Input}).Validate(existing))
}

func TestParseOutput_ScoreBoundsMustBeFinite(t *testing.T) {
	_, err := ParseOutput("score:0-1" + strings.Repeat("0", 400))
	assert.ErrorIs(t, err, model.ErrValidation)
}

func TestContract_EscapesEnumLabels(t *testing.T) {
	c := &Column{Name: "k", Type: AI, Config: Config{Output: `enum:say "hi"|back\slash`, Prompt: "{{a}}"}}
	contract, err := c.contract()
	require.NoError(t, err)
	assert.Contains(t, contract.Instructions, `{"value": "say \"hi\"" | "back\\slash"}`)
	assert.Contains(t, contract.Instructions, `exactly one of the strings "say \"hi\"", "back\\slash"`)
}

func TestReferences(t *testing.T) {
	cols := []*Column{{Name: "company", Type: Input}, {Name: "company.domain", Type: Input}}
	f := &Column{Name: "f", Type: Formula, Config: Config{Expr: "{{company.domain}} {{company.x}}"}}
	assert.Equal(t, []string{"company.domain", "company"}, f.References(cols))
	assert.Equal(t, []string{"company", "company"}, f.References(cols[:1]))
}
