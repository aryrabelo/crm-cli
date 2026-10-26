package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jdanielnd/crm-cli/internal/format"
	"github.com/jdanielnd/crm-cli/internal/model"
	"github.com/jdanielnd/crm-cli/internal/provider"
	"github.com/jdanielnd/crm-cli/internal/run"
	"github.com/spf13/cobra"
)

func registerRunCommands(rootCmd *cobra.Command) {
	cmd := runCmd()
	cmd.AddCommand(runListCmd(), runShowCmd())
	rootCmd.AddCommand(cmd)
}

var runSummaryColumns = []format.ColumnDef{
	{Header: "Run", Field: "id"},
	{Header: "Table", Field: "table"},
	{Header: "Status", Field: "status"},
	{Header: "Items", Field: "items"},
	{Header: "OK", Field: "ok"},
	{Header: "Empty", Field: "empty"},
	{Header: "Failed", Field: "failed"},
	{Header: "Skipped", Field: "skipped"},
	{Header: "Cache Hits", Field: "cache_hits"},
	{Header: "Calls", Field: "calls"},
	{Header: "Spent", Field: "spent"},
	{Header: "Budget", Field: "budget"},
	{Header: "Duration", Field: "duration"},
}

var estimateColumns = []format.ColumnDef{
	{Header: "Column", Field: "column"},
	{Header: "Items", Field: "items"},
	{Header: "Cache Hits", Field: "cache_hits"},
	{Header: "No Input", Field: "no_input"},
	{Header: "Calls", Field: "calls"},
	{Header: "Est. Cost", Field: "expected"},
	{Header: "Worst Case", Field: "worst"},
}

func runData(r *run.Run) map[string]any {
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

func estimateData(ce *run.ColumnEstimate) map[string]any {
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

func runCmd() *cobra.Command {
	var columns []string
	var concurrency, retry int
	var budget string
	var dryRun bool
	var backoffBase time.Duration
	var resume int64
	cmd := &cobra.Command{
		Use:   "run <table> --columns a,b",
		Short: "Fill enrich columns in batch (waterfall, cache, budget, resume)",
		Long: `Fill enrich columns for every row of a table.

Each row × column goes through the column's provider waterfall: cached answers
are used first (cost 0), then providers are called in order until one answers
with a non-empty value. Retryable errors (429, 5xx, timeouts) are retried with
exponential backoff. The budget is a hard cap: a call is only made after its
price is reserved, so the run never spends more than --budget; items that do
not fit are marked skipped. --budget is required when the run may spend money
(see --dry-run). Every item is checkpointed: --resume <run-id> continues a run,
re-trying pending, failed and skipped items (with a new --budget, if given).`,
		Example: `  crm run leads --columns company --dry-run
  crm run leads --columns company,site --concurrency 8 --budget 2.00
  crm run --resume 3 --budget 5`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts := run.Options{
				Columns:     columns,
				Concurrency: concurrency,
				Retry:       retry,
				Backoff:     backoffBase,
				Resume:      resume,
			}
			switch {
			case resume != 0 && (len(args) > 0 || len(columns) > 0):
				return model.NewExitError(model.ErrValidation, "--resume takes the table and columns from the run; drop <table> and --columns")
			case resume == 0 && len(args) == 0:
				return model.NewExitError(model.ErrValidation, "table is required: crm run <table> --columns a,b")
			case resume == 0:
				opts.Table = args[0]
			}
			if cmd.Flags().Changed("budget") {
				b, err := provider.ParseUSD(budget)
				if err != nil {
					return model.NewExitError(model.ErrValidation, "--budget: %s", err)
				}
				opts.Budget, opts.BudgetSet = b, true
			}
			if concurrency < 1 {
				return model.NewExitError(model.ErrValidation, "--concurrency must be at least 1")
			}

			db, err := openDB()
			if err != nil {
				return err
			}
			defer db.Close()
			engine := run.NewEngine(db, newRegistry())

			if dryRun {
				est, err := engine.Estimate(cmd.Context(), opts)
				if err != nil {
					return err
				}
				data := make([]map[string]any, 0, len(est.Columns)+1)
				for _, ce := range est.Columns {
					data = append(data, estimateData(ce))
				}
				data = append(data, estimateData(&est.Total))
				t := est.Total
				fmt.Fprintf(os.Stderr, "dry run on %s: %d items, %d cache hits, %d without input, %d to call — est. %s (worst case %s); nothing written\n",
					est.Table, t.Items, t.CacheHits, t.NoInput, t.Calls, t.Expected, t.Worst)
				if opts.BudgetSet && t.Worst > opts.Budget {
					fmt.Fprintf(os.Stderr, "budget %s may not cover the worst case; items that do not fit will be skipped (resumable)\n", opts.Budget)
				}
				return format.Output(os.Stdout, resolveFormat(), data, estimateColumns, false)
			}

			res, err := engine.Execute(cmd.Context(), opts)
			if err != nil {
				return err
			}
			r, st := res.Run, res.Run.Stats
			fmt.Fprintf(os.Stderr, "run %d %s in %s: %d items — ok %d, empty %d, failed %d, skipped %d, pending %d · cache hits %d · calls %d · spent %s of %s budget\n",
				r.ID, r.Status, res.Duration.Round(time.Millisecond), st.Items, st.OK, st.Empty, st.Failed, st.Skipped, st.Pending,
				st.CacheHits, st.Calls, r.Spent, r.Budget)
			if st.Skipped > 0 || st.Pending > 0 || st.Failed > 0 {
				fmt.Fprintf(os.Stderr, "resume with: crm run --resume %d [--budget USD]   (details: crm run show %d --items)\n", r.ID, r.ID)
			}
			return format.Output(os.Stdout, resolveFormat(), []map[string]any{runData(r)}, runSummaryColumns, flagQuiet)
		},
	}
	f := cmd.Flags()
	f.StringSliceVar(&columns, "columns", nil, "enrich columns to fill, in order (a later column may read an earlier one)")
	f.IntVar(&concurrency, "concurrency", run.DefaultConcurrency, "rows processed in parallel")
	f.StringVar(&budget, "budget", "", "hard spending cap in USD for the whole run (e.g. 2.00)")
	f.BoolVar(&dryRun, "dry-run", false, "estimate calls, cost and cache hits; call nothing, write nothing")
	f.IntVar(&retry, "retry", run.DefaultRetry, "retries per provider call on 429/5xx/timeouts")
	f.DurationVar(&backoffBase, "backoff", run.DefaultBackoff, "first retry delay (doubles on each retry)")
	f.Int64Var(&resume, "resume", 0, "continue run RUN_ID (skips items already done)")
	return cmd
}

var runListColumns = []format.ColumnDef{
	{Header: "Run", Field: "id"},
	{Header: "Table", Field: "table"},
	{Header: "Columns", Field: "columns"},
	{Header: "Status", Field: "status"},
	{Header: "Items", Field: "items"},
	{Header: "Spent", Field: "spent"},
	{Header: "Budget", Field: "budget"},
	{Header: "Started", Field: "started_at"},
}

func runListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List runs, newest first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			db, err := openDB()
			if err != nil {
				return err
			}
			defer db.Close()
			runs, err := run.NewStore(db).List(cmd.Context())
			if err != nil {
				return err
			}
			data := make([]map[string]any, len(runs))
			for i, r := range runs {
				data[i] = runData(r)
			}
			return format.Output(os.Stdout, resolveFormat(), data, runListColumns, flagQuiet)
		},
	}
}

var runItemColumns = []format.ColumnDef{
	{Header: "Row", Field: "row"},
	{Header: "Column", Field: "column"},
	{Header: "Status", Field: "status"},
	{Header: "Provider", Field: "provider"},
	{Header: "Cache", Field: "cache_hit"},
	{Header: "Attempts", Field: "attempts"},
	{Header: "Cost", Field: "cost"},
	{Header: "ms", Field: "ms"},
	{Header: "Value", Field: "value"},
	{Header: "Error", Field: "error"},
}

func runShowCmd() *cobra.Command {
	var items bool
	cmd := &cobra.Command{
		Use:   "show <run-id>",
		Short: "Show a run summary (or its items with --items)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return model.NewExitError(model.ErrValidation, "invalid run id %q", args[0])
			}
			db, err := openDB()
			if err != nil {
				return err
			}
			defer db.Close()
			rs := run.NewStore(db)
			r, err := rs.Get(cmd.Context(), id)
			if err != nil {
				return err
			}
			if !items {
				// Item counts are live (a run may be in progress or interrupted).
				st, err := rs.Stats(cmd.Context(), id)
				if err != nil {
					return err
				}
				st.DurationMS = r.Stats.DurationMS
				r.Stats = st
				return format.Output(os.Stdout, resolveFormat(), []map[string]any{runData(r)}, runSummaryColumns, flagQuiet)
			}
			list, err := rs.Items(cmd.Context(), id, false)
			if err != nil {
				return err
			}
			data := make([]map[string]any, len(list))
			for i, it := range list {
				m := map[string]any{
					"id": it.ID, "row": it.RowPosition, "column": it.Column, "status": it.Status,
					"cache_hit": it.CacheHit, "attempts": it.Attempts, "calls": it.Calls,
					"cost": it.Cost.String(), "cost_usd": it.Cost.USD(), "ms": it.MS,
					"provider": "", "value": "", "error": "",
				}
				if it.Provider != nil {
					m["provider"] = *it.Provider
				}
				if it.Value != nil {
					m["value"] = *it.Value
				}
				if it.Error != nil {
					m["error"] = *it.Error
				}
				data[i] = m
			}
			return format.Output(os.Stdout, resolveFormat(), data, runItemColumns, flagQuiet)
		},
	}
	cmd.Flags().BoolVar(&items, "items", false, "list every row × column item instead of the summary")
	return cmd
}
