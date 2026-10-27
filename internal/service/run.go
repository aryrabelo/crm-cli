package service

import (
	"context"
	"strings"
	"time"

	"github.com/jdanielnd/crm-cli/internal/provider"
	"github.com/jdanielnd/crm-cli/internal/run"
)

// RunStatus returns run id with live item counts (a run may be in progress
// or interrupted; its stored stats are only final once it finishes).
func (s *Service) RunStatus(ctx context.Context, id int64) (*run.Run, error) {
	rs := run.NewStore(s.db)
	r, err := rs.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	st, err := rs.Stats(ctx, id)
	if err != nil {
		return nil, err
	}
	st.DurationMS = r.Stats.DurationMS
	r.Stats = st
	return r, nil
}

// RunItems returns every row × column item of run id.
func (s *Service) RunItems(ctx context.Context, id int64) ([]*run.Item, error) {
	rs := run.NewStore(s.db)
	if _, err := rs.Get(ctx, id); err != nil {
		return nil, err
	}
	return rs.Items(ctx, id, false)
}

// ItemData is the output record of a run item.
func ItemData(it *run.Item) map[string]any {
	m := map[string]any{
		"id": it.ID, "row": it.RowPosition, "column": it.Column, "status": it.Status,
		"cache_hit": it.CacheHit, "attempts": it.Attempts, "calls": it.Calls,
		"cost": it.Cost.String(), "cost_usd": it.Cost.USD(), "ms": it.MS,
		"tokens_in": it.TokensIn, "tokens_out": it.TokensOut,
		"provider": "", "value": "", "error": "",
	}
	if it.Provider != nil {
		m["provider"] = *it.Provider
	}
	if it.Value != nil {
		m["value"] = *it.Value
	}
	if it.Error != nil {
		// Redacted again on output: rows written by earlier builds stored
		// provider errors verbatim.
		m["error"] = provider.RedactError(*it.Error)
	}
	return m
}

// RunData is the output record of a run.
func RunData(r *run.Run) map[string]any {
	st := r.Stats
	return map[string]any{
		"id":          r.ID,
		"uuid":        r.UUID,
		"table":       r.Table,
		"columns":     strings.Join(r.Columns, ","),
		"status":      string(r.Status),
		"items":       st.Items,
		"ok":          st.OK,
		"empty":       st.Empty,
		"failed":      st.Failed,
		"skipped":     st.Skipped,
		"pending":     st.Pending,
		"cache_hits":  st.CacheHits,
		"calls":       st.Calls,
		"tokens_in":   st.TokensIn,
		"tokens_out":  st.TokensOut,
		"providers":   st.Providers,
		"spent":       r.Spent.String(),
		"spent_usd":   r.Spent.USD(),
		"budget":      r.Budget.String(),
		"budget_usd":  r.Budget.USD(),
		"duration":    (time.Duration(st.DurationMS) * time.Millisecond).String(),
		"duration_ms": st.DurationMS,
		"started_at":  r.StartedAt,
		"finished_at": r.FinishedAt,
	}
}

// EstimateData is the output record of one column (or the total) of a dry run.
func EstimateData(ce *run.ColumnEstimate) map[string]any {
	return map[string]any{
		"column":       ce.Column,
		"items":        ce.Items,
		"cache_hits":   ce.CacheHits,
		"no_input":     ce.NoInput,
		"calls":        ce.Calls,
		"expected":     ce.Expected.String(),
		"expected_usd": ce.Expected.USD(),
		"worst":        ce.Worst.String(),
		"worst_usd":    ce.Worst.USD(),
	}
}
