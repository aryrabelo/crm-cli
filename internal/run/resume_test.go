package run_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jdanielnd/crm-cli/internal/model"
	"github.com/jdanielnd/crm-cli/internal/run"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Design review P1-3: crm column rm + crm column add with the same name,
// then crm run --resume, used to panic with a nil pointer in a worker (the
// run's items point at the archived column). It is now refused with a usage
// error naming the column, for both --dry-run and the run itself.
func TestResume_ColumnRemovedAndReAdded(t *testing.T) {
	e := setup(t, 3)
	down := newAPI(t, func(w http.ResponseWriter, _ string) { w.WriteHeader(http.StatusInternalServerError) })
	e.provider(t, "down", down, "v", 1000)
	e.enrich(t, "fit", "down")
	ctx := context.Background()
	first, err := e.engine.Execute(ctx, run.Options{Table: "leads", Columns: []string{"fit"}, Retry: 0,
		Backoff: time.Millisecond, Budget: 1_000_000, BudgetSet: true})
	require.NoError(t, err)
	require.Equal(t, 3, first.Run.Stats.Failed, "items stay open for --resume")

	require.NoError(t, e.tables.RemoveColumn(ctx, e.ws, "fit"))
	e.enrich(t, "fit", "down")

	opts := run.Options{Resume: first.Run.ID, Retry: 0, Backoff: time.Millisecond}
	_, err = e.engine.Execute(ctx, opts)
	require.ErrorIs(t, err, model.ErrValidation)
	assert.Contains(t, err.Error(), "start a new run: crm run leads --columns fit")

	_, err = e.engine.Estimate(ctx, opts)
	require.ErrorIs(t, err, model.ErrValidation)
	assert.Contains(t, err.Error(), `column(s) fit of table leads were removed`)

	// The new column runs fine in a new run.
	again, err := e.engine.Execute(ctx, run.Options{Table: "leads", Columns: []string{"fit"}, Retry: 0,
		Backoff: time.Millisecond, Budget: 1_000_000, BudgetSet: true})
	require.NoError(t, err)
	assert.Equal(t, 3, again.Run.Stats.Items)
}
