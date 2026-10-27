package cli

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/jdanielnd/crm-cli/internal/format"
	"github.com/jdanielnd/crm-cli/internal/model"
	"github.com/jdanielnd/crm-cli/internal/provider"
	"github.com/jdanielnd/crm-cli/internal/run"
	"github.com/jdanielnd/crm-cli/internal/service"
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
	{Header: "Tokens In", Field: "tokens_in"},
	{Header: "Tokens Out", Field: "tokens_out"},
	{Header: "Spent", Field: "spent"},
	{Header: "Budget", Field: "budget"},
	{Header: "Uncertain", Field: "uncertain_spent"},
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

// printEstimate reports a dry run: a summary line on stderr, the per-column
// estimate on stdout.
func printEstimate(est *run.Estimate, opts run.Options) error {
	data := make([]map[string]any, 0, len(est.Columns)+1)
	for _, ce := range est.Columns {
		data = append(data, service.EstimateData(ce))
	}
	data = append(data, service.EstimateData(&est.Total))
	t := est.Total
	worst := t.Worst.String()
	if t.Unbounded > 0 {
		worst = "unbounded, at least " + worst
	}
	fmt.Fprintf(os.Stderr, "dry run on %s: %d items, %d cache hits, %d without input, %d to call — est. %s (worst case %s); nothing written\n",
		est.Table, t.Items, t.CacheHits, t.NoInput, t.Calls, t.Expected, worst)
	if t.Unbounded > 0 {
		fmt.Fprintf(os.Stderr, "%d item(s) send a prompt built from a column computed earlier in this run, so its size (and price) is not known yet; the budget still caps the run and items that do not fit are skipped\n", t.Unbounded)
	}
	if opts.BudgetSet && (t.Worst > opts.Budget || t.Unbounded > 0) {
		fmt.Fprintf(os.Stderr, "budget %s may not cover the worst case; items that do not fit will be skipped (resumable)\n", opts.Budget)
	}
	return format.Output(os.Stdout, resolveFormat(), data, estimateColumns, false)
}

// printRunLine reports a finished run invocation on stderr.
func printRunLine(res *run.Result) {
	r, st := res.Run, res.Run.Stats
	fmt.Fprintf(os.Stderr, "run %d %s in %s: %d items — ok %d, empty %d, failed %d, skipped %d, pending %d · cache hits %d · calls %d · tokens in %d / out %d · spent %s of %s budget\n",
		r.ID, r.Status, res.Duration.Round(time.Millisecond), st.Items, st.OK, st.Empty, st.Failed, st.Skipped, st.Pending,
		st.CacheHits, st.Calls, st.TokensIn, st.TokensOut, r.Spent, r.Budget)
	if st.Uncertain > 0 {
		fmt.Fprintf(os.Stderr, "%s of the spend is uncertain: %d item(s) sent a call that got no response, counted at its estimate (the provider may have billed it)\n",
			provider.Cost(st.UncertainMicros), st.Uncertain)
	}
	if st.Skipped > 0 || st.Pending > 0 || st.Failed > 0 {
		fmt.Fprintf(os.Stderr, "resume with: crm run --resume %d [--budget USD]   (details: crm run show %d --items)\n", r.ID, r.ID)
	}
}

// runOutcome is the exit status of a run invocation that did not finish
// every item: interrupted (7; the process exits 128+n when a signal stopped
// it), budget_exhausted (5), or completed with failed items (6). The run is
// saved and resumable either way; nil means every item is done.
func runOutcome(r *run.Run) error {
	switch {
	case r.Status == run.StatusInterrupted:
		return model.NewExitError(model.ErrInterrupted, "run %d interrupted: %d item(s) pending (crm run --resume %d)", r.ID, r.Stats.Pending, r.ID)
	case r.Status == run.StatusBudgetExhausted:
		return model.NewExitError(model.ErrBudgetExhausted, "run %d stopped at its budget: %d item(s) skipped (crm run --resume %d --budget USD)", r.ID, r.Stats.Skipped, r.ID)
	case r.Stats.Failed > 0:
		return model.NewExitError(model.ErrItemsFailed, "run %d finished with %d failed item(s) (crm run show %d --items)", r.ID, r.Stats.Failed, r.ID)
	default:
		return nil
	}
}

func runCmd() *cobra.Command {
	var columns, only []string
	var concurrency, retry int
	var budget string
	var dryRun, retryUncertain bool
	var backoffBase time.Duration
	var resume int64
	cmd := &cobra.Command{
		Use:   "run <table> --columns a,b",
		Short: "Fill enrich and ai columns in batch (waterfall, cache, budget, resume)",
		Long: `Fill enrich and ai columns for every row of a table (or the rows matching --only).

Each row × column goes through the column's provider waterfall: cached answers
are used first (cost 0), then providers are called in order until one answers
with a non-empty value. Retryable errors (429, 5xx, timeouts) are retried with
exponential backoff.

The budget caps the spend crm accounts for, at the configured provider prices:
a call is only made after its estimated price is reserved, so the recorded
spend never exceeds --budget; items that do not fit are marked skipped. ai
columns reserve their worst case (prompt bound plus max tokens, twice for the
one JSON repair retry) and are charged the tokens the provider reports. Failed
calls with an error response cost nothing, but a call that times out or loses
its connection after the request was sent may have been billed: it is counted
at its estimate and reported as uncertain, and a paid POST in that state is not
retried (--retry-uncertain retries it). What a provider actually bills follows
its own prices, which crm does not see.

--budget is required when the run may spend money (see --dry-run). Every item
is checkpointed: --resume <run-id> continues a run, re-trying pending, failed
and skipped items (with a new --budget, if given).

Exit status: 0 when every item is done (ok or empty); 5 when the budget
stopped the run (budget_exhausted); 6 when it completed with failed items;
7 when it was interrupted with items pending; 128+n when signal n stopped it
(130 for Ctrl-C, 143 for SIGTERM); 4 when another crm run holds the run.`,
		Example: `  crm run leads --columns company --dry-run
  crm run leads --columns company,fit --concurrency 8 --budget 2.00
  crm run leads --columns fit --only "company!=" --budget 0.50
  crm run --resume 3 --budget 5`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts := run.Options{
				Columns:        columns,
				Concurrency:    concurrency,
				Retry:          retry,
				Backoff:        backoffBase,
				Resume:         resume,
				Only:           only,
				RetryUncertain: retryUncertain,
			}
			switch {
			case resume != 0 && (len(args) > 0 || len(columns) > 0 || len(only) > 0):
				return model.NewExitError(model.ErrValidation, "--resume takes the table, columns and rows from the run; drop <table>, --columns and --only")
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
			engine := newService(db).Engine()

			if dryRun {
				est, err := engine.Estimate(cmd.Context(), opts)
				if err != nil {
					return err
				}
				return printEstimate(est, opts)
			}

			res, err := engine.Execute(cmd.Context(), opts)
			if err != nil {
				return err
			}
			printRunLine(res)
			if err := format.Output(os.Stdout, resolveFormat(), []map[string]any{service.RunData(res.Run)}, runSummaryColumns, flagQuiet); err != nil {
				return err
			}
			return runOutcome(res.Run)
		},
	}
	f := cmd.Flags()
	f.StringSliceVar(&columns, "columns", nil, "enrich/ai columns to fill, in order (a later column may read an earlier one)")
	f.StringArrayVar(&only, "only", nil, onlyHelp+"; only matching rows are run")
	f.IntVar(&concurrency, "concurrency", run.DefaultConcurrency, "rows processed in parallel")
	f.StringVar(&budget, "budget", "", "spending cap in USD for the whole run, at the configured provider prices (e.g. 2.00)")
	f.BoolVar(&dryRun, "dry-run", false, "estimate calls, cost and cache hits; call nothing, write nothing")
	f.IntVar(&retry, "retry", run.DefaultRetry, "retries per provider call on 429/5xx/timeouts")
	f.BoolVar(&retryUncertain, "retry-uncertain", false, "also retry paid POST calls that timed out after the request was sent (may pay twice)")
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
				data[i] = service.RunData(r)
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
	{Header: "Tok In", Field: "tokens_in"},
	{Header: "Tok Out", Field: "tokens_out"},
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
			if !items {
				r, err := newService(db).RunStatus(cmd.Context(), id)
				if err != nil {
					return err
				}
				return format.Output(os.Stdout, resolveFormat(), []map[string]any{service.RunData(r)}, runSummaryColumns, flagQuiet)
			}
			list, err := newService(db).RunItems(cmd.Context(), id)
			if err != nil {
				return err
			}
			data := make([]map[string]any, len(list))
			for i, it := range list {
				data[i] = service.ItemData(it)
			}
			return format.Output(os.Stdout, resolveFormat(), data, runItemColumns, flagQuiet)
		},
	}
	cmd.Flags().BoolVar(&items, "items", false, "list every row × column item instead of the summary")
	return cmd
}
