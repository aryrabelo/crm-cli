package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/jdanielnd/crm-cli/internal/format"
	"github.com/jdanielnd/crm-cli/internal/model"
	"github.com/jdanielnd/crm-cli/internal/provider"
	"github.com/jdanielnd/crm-cli/internal/run"
	"github.com/jdanielnd/crm-cli/internal/service"
	"github.com/spf13/cobra"
)

func registerQualifyCommand(rootCmd *cobra.Command) {
	rootCmd.AddCommand(qualifyCmd())
}

var qualifyColumns = []format.ColumnDef{
	{Header: "Run", Field: "run"},
	{Header: "Column", Field: "column"},
	{Header: "Yes", Field: "yes"},
	{Header: "No", Field: "no"},
	{Header: "Maybe", Field: "maybe"},
	{Header: "Failed", Field: "failed"},
	{Header: "Skipped", Field: "skipped"},
	{Header: "Cache Hits", Field: "cache_hits"},
	{Header: "Tokens In", Field: "tokens_in"},
	{Header: "Tokens Out", Field: "tokens_out"},
	{Header: "Spent", Field: "spent"},
	{Header: "Budget", Field: "budget"},
}

func qualifyCmd() *cobra.Command {
	var criteria, name, budget string
	var only, wf []string
	var explain, dryRun, retryUncertain bool
	var concurrency, retry int
	cmd := &cobra.Command{
		Use:   "qualify <table> --criteria <file.md>",
		Short: "Decide yes/no/maybe per row against ICP criteria (llm, with --explain reasons)",
		Long: `Qualify every row of a table (or the rows matching --only) against the
criteria in a markdown file. Creates — or, on later calls, redefines — the ai
column --name (default "qualified") with values yes, no or maybe; with
--explain, also <name>_reason with one or two sentences citing the row fields
behind each decision.

The prompt is the criteria plus every other column of the row. It runs
through crm run: answers are cached (re-running unchanged rows costs 0), the
budget is a hard cap (--budget is required when the run may spend money), and
--dry-run estimates calls and cost without writing anything. Prints the
decision counts, tokens in/out and cost vs budget, and exits like crm run:
5 when the budget stopped it, 6 with failed rows, 128+n on a signal.`,
		Example: `  crm qualify leads --criteria files/icp.md --explain --dry-run
  crm qualify leads --criteria files/icp.md --explain --budget 0.50
  crm table export leads --only "qualified=yes"`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if criteria == "" {
				return model.NewExitError(model.ErrValidation, "--criteria <file.md> is required")
			}
			if concurrency < 1 {
				return model.NewExitError(model.ErrValidation, "--concurrency must be at least 1")
			}
			opts := service.QualifyOptions{Table: args[0], Name: name, Explain: explain, Only: only, Waterfall: wf,
				Concurrency: concurrency, Retry: retry, RetryUncertain: retryUncertain, DryRun: dryRun}
			if cmd.Flags().Changed("budget") {
				b, err := provider.ParseUSD(budget)
				if err != nil {
					return model.NewExitError(model.ErrValidation, "--budget: %s", err)
				}
				opts.Budget, opts.BudgetSet = b, true
			}
			text, err := os.ReadFile(criteria)
			if errors.Is(err, fs.ErrNotExist) {
				return model.NewExitError(model.ErrNotFound, "criteria file not found: %s", criteria)
			}
			if err != nil {
				return fmt.Errorf("read criteria %s: %w", criteria, err)
			}
			if strings.TrimSpace(string(text)) == "" {
				return model.NewExitError(model.ErrValidation, "criteria file %s is empty", criteria)
			}
			opts.Criteria = string(text)

			db, err := openDB()
			if err != nil {
				return err
			}
			defer db.Close()
			res, err := newService(db).Qualify(cmd.Context(), opts)
			if err != nil {
				return err
			}
			if res.Estimate != nil {
				verb := "create"
				if res.Redefine {
					verb = "redefine"
				}
				fmt.Fprintf(os.Stderr, "qualify would %s ai column %s (%s)", verb, res.Column, res.Output)
				if res.Reason != "" {
					fmt.Fprintf(os.Stderr, " with reasons in %s", res.Reason)
				}
				fmt.Fprintln(os.Stderr)
				return printEstimate(res.Estimate, run.Options{Budget: opts.Budget, BudgetSet: opts.BudgetSet})
			}
			printRunLine(res.Run)
			r, st := res.Run.Run, res.Run.Run.Stats
			fmt.Fprintf(os.Stderr, "qualify %s.%s: yes %d · no %d · maybe %d · failed %d · skipped %d — tokens in %d / out %d · spent %s of %s budget\n",
				res.Table, res.Column, res.Counts["yes"], res.Counts["no"], res.Counts["maybe"], res.Counts[run.ItemFailed],
				res.Counts[run.ItemSkipped], st.TokensIn, st.TokensOut, r.Spent, r.Budget)
			if err := format.Output(os.Stdout, resolveFormat(), []map[string]any{service.QualifyData(res)}, qualifyColumns, flagQuiet); err != nil {
				return err
			}
			return runOutcome(res.Run.Run)
		},
	}
	f := cmd.Flags()
	f.StringVar(&criteria, "criteria", "", "markdown file with the qualification criteria (required)")
	f.BoolVar(&explain, "explain", false, "also store a 1–2 sentence reason per row in <name>_reason")
	f.StringVar(&name, "name", service.DefaultQualifyColumn, "decision column name")
	f.StringVar(&budget, "budget", "", "spending cap in USD for the run, at the configured token prices (e.g. 0.50; see crm run --help)")
	f.BoolVar(&dryRun, "dry-run", false, "estimate calls, cost and cache hits; call nothing, write nothing")
	f.StringArrayVar(&only, "only", nil, onlyHelp+"; only matching rows are qualified")
	f.StringSliceVar(&wf, "waterfall", []string{"llm"}, "llm providers in order (see crm provider list)")
	f.IntVar(&concurrency, "concurrency", run.DefaultConcurrency, "rows processed in parallel")
	f.IntVar(&retry, "retry", run.DefaultRetry, "retries per llm call on 429/5xx/timeouts")
	f.BoolVar(&retryUncertain, "retry-uncertain", false, "also retry paid llm calls that timed out after the request was sent (may pay twice)")
	return cmd
}
