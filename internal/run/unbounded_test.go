package run_test

import (
	"context"
	"testing"
	"time"

	"github.com/jdanielnd/crm-cli/internal/column"
	"github.com/jdanielnd/crm-cli/internal/model"
	"github.com/jdanielnd/crm-cli/internal/run"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Design review P1-5: an ai column reading an enrich column of the same run
// was priced as if the enrich answer were empty, a precise-looking figure
// far below the real prompts. Its worst case is now reported as unbounded.
func TestEstimate_DependentAIColumnIsUnbounded(t *testing.T) {
	e := setup(t, 3)
	api := newAPI(t, nil)
	e.provider(t, "p", api, "company", 1000)
	e.enrich(t, "company", "p")
	e.llm(t, newChat(t, 0))
	ctx := context.Background()
	require.NoError(t, e.tables.AddColumn(ctx, &column.Column{WorkspaceID: e.ws.ID, Name: "fit", Type: column.AI,
		Config: column.Config{Waterfall: []string{"llm"}, Prompt: "Score {{company}}", Output: "score:0-100"}}))

	est, err := e.engine.Estimate(ctx, run.Options{Table: "leads", Columns: []string{"company", "fit"}})
	require.NoError(t, err)
	require.Len(t, est.Columns, 2)
	assert.Zero(t, est.Columns[0].Unbounded, "enrich is priced per call")
	assert.Equal(t, 3, est.Columns[1].Unbounded, "fit's prompt holds company, not known yet")
	assert.Equal(t, 3, est.Total.Unbounded)

	// Without a budget the run is refused, naming the unbounded worst case.
	_, err = e.engine.Execute(ctx, run.Options{Table: "leads", Columns: []string{"company", "fit"},
		Retry: 0, Backoff: time.Millisecond})
	require.ErrorIs(t, err, model.ErrValidation)
	assert.Contains(t, err.Error(), "an unbounded amount")

	// fit alone reads only imported cells: bounded.
	est, err = e.engine.Estimate(ctx, run.Options{Table: "leads", Columns: []string{"fit"}})
	require.NoError(t, err)
	assert.Zero(t, est.Total.Unbounded)
}
