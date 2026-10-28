package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jdanielnd/crm-cli/internal/column"
)

func TestQualifyFields_ReasonColumnIsInputUnlessExplained(t *testing.T) {
	cols := []*column.Column{
		{Name: "company", Type: column.Input},
		{Name: "qualified_reason", Type: column.Input},
	}
	got, err := qualifyFields(cols, "qualified", false, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"company", "qualified_reason"}, got)

	got, err = qualifyFields(cols, "qualified", true, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"company"}, got)

	_, err = qualifyFields(cols, "qualified", true, []string{"qualified_reason"})
	assert.Error(t, err)
	got, err = qualifyFields(cols, "qualified", false, []string{"qualified_reason"})
	require.NoError(t, err)
	assert.Equal(t, []string{"qualified_reason"}, got)
}

func TestQualifyPrompt_LiteralBracesInCriteriaValidate(t *testing.T) {
	prompt := qualifyPrompt("Mentions {{industry}} and {{{x}}} literally", []string{"company"}, 100)
	c := &column.Column{Name: "qualified", Type: column.AI,
		Config: column.Config{Prompt: prompt, Output: "enum:yes|no|maybe", Waterfall: []string{"llm"}}}
	require.NoError(t, c.Validate([]*column.Column{{Name: "company", Type: column.Input}}))
	assert.Contains(t, prompt, "{ {industry} }")
}
