package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"

	"github.com/jdanielnd/crm-cli/internal/column"
	"github.com/jdanielnd/crm-cli/internal/format"
	"github.com/jdanielnd/crm-cli/internal/model"
	"github.com/jdanielnd/crm-cli/internal/provider"
	"github.com/jdanielnd/crm-cli/internal/run"
	"github.com/jdanielnd/crm-cli/internal/table"
	"github.com/spf13/cobra"
)

func registerQualifyCommand(rootCmd *cobra.Command) {
	rootCmd.AddCommand(qualifyCmd())
}

// qualifyDecisions are the values of a qualify decision column.
var qualifyDecisions = []string{"yes", "no", "maybe"}

// qualifyPrompt is the decision prompt: the criteria, then every given row
// field as a {{placeholder}}.
func qualifyPrompt(criteria string, fields []string) string {
	var b strings.Builder
	b.WriteString(`Decide whether this row matches the criteria below. Answer "yes" when the row clearly matches, ` +
		`"no" when it clearly does not, and "maybe" when the row lacks the information to decide.` + "\n\n## Criteria\n\n")
	b.WriteString(strings.TrimSpace(criteria))
	b.WriteString("\n\n## Row\n\n")
	for _, f := range fields {
		fmt.Fprintf(&b, "- %s: {{%s}}\n", f, f)
	}
	return b.String()
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
	var explain, dryRun bool
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
decision counts, tokens in/out and cost vs budget.`,
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
			opts := run.Options{Columns: []string{name}, Concurrency: concurrency, Retry: retry, Only: only}
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

			db, err := openDB()
			if err != nil {
				return err
			}
			defer db.Close()
			ctx := cmd.Context()
			ts := table.NewStore(db)
			ws, err := ts.FindWorkspace(ctx, args[0])
			if err != nil {
				return err
			}
			opts.Table = ws.Name
			cols, err := ts.Columns(ctx, ws.ID)
			if err != nil {
				return err
			}
			reason := ""
			if explain {
				reason = reasonColumn(name)
			}
			c := &column.Column{WorkspaceID: ws.ID, Name: name, Type: column.AI,
				Config: column.Config{Output: "enum:" + strings.Join(qualifyDecisions, "|"), Reason: reason, Waterfall: wf}}
			var fields []string
			for _, o := range cols {
				switch {
				case o.Name == name:
					if o.Type != column.AI {
						return model.NewExitError(model.ErrConflict, "column %q already exists as a %s column; pick another --name", name, o.Type)
					}
					c.ID = o.ID
				case o.Name == reasonColumn(name), strings.Contains(o.Name, "{{"), strings.Contains(o.Name, "}}"):
				default:
					fields = append(fields, o.Name)
				}
			}
			c.Config.Prompt = qualifyPrompt(string(text), fields)
			update := c.ID != 0

			engine := run.NewEngine(db, newRegistry())
			if dryRun {
				preview := []*column.Column{c}
				others := slices.DeleteFunc(slices.Clone(cols), func(o *column.Column) bool { return o.Name == name })
				if reason != "" && !slices.ContainsFunc(cols, func(o *column.Column) bool { return o.Name == reason }) {
					rc := &column.Column{WorkspaceID: ws.ID, Name: reason, Type: column.Input}
					preview = append([]*column.Column{rc}, preview...)
					others = append(others, rc)
				}
				if err := c.Validate(others); err != nil {
					return err
				}
				opts.Preview = preview
				est, err := engine.Estimate(ctx, opts)
				if err != nil {
					return err
				}
				verb := "create"
				if update {
					verb = "redefine"
				}
				fmt.Fprintf(os.Stderr, "qualify would %s ai column %s (%s)", verb, name, c.Config.Output)
				if reason != "" {
					fmt.Fprintf(os.Stderr, " with reasons in %s", reason)
				}
				fmt.Fprintln(os.Stderr)
				return printEstimate(est, opts)
			}

			if err := saveColumn(ctx, db, c, update); err != nil {
				return err
			}
			res, err := engine.Execute(ctx, opts)
			if err != nil {
				return err
			}
			printRunLine(res)
			items, err := run.NewStore(db).Items(ctx, res.Run.ID, false)
			if err != nil {
				return err
			}
			counts := map[string]int{}
			for _, it := range items {
				counts[decision(it)]++
			}
			r, st := res.Run, res.Run.Stats
			fmt.Fprintf(os.Stderr, "qualify %s.%s: yes %d · no %d · maybe %d · failed %d · skipped %d — tokens in %d / out %d · spent %s of %s budget\n",
				ws.Name, name, counts["yes"], counts["no"], counts["maybe"], counts[run.ItemFailed], counts[run.ItemSkipped],
				st.TokensIn, st.TokensOut, r.Spent, r.Budget)
			out := map[string]any{
				"run": r.ID, "table": ws.Name, "column": name, "reason_column": reason, "status": string(r.Status),
				"yes": counts["yes"], "no": counts["no"], "maybe": counts["maybe"],
				"empty": counts[run.ItemEmpty], "failed": counts[run.ItemFailed],
				"skipped": counts[run.ItemSkipped], "pending": counts[run.ItemPending],
				"cache_hits": st.CacheHits, "calls": st.Calls, "tokens_in": st.TokensIn, "tokens_out": st.TokensOut,
				"spent": r.Spent.String(), "spent_usd": r.Spent.USD(), "budget": r.Budget.String(), "budget_usd": r.Budget.USD(),
			}
			return format.Output(os.Stdout, resolveFormat(), []map[string]any{out}, qualifyColumns, flagQuiet)
		},
	}
	f := cmd.Flags()
	f.StringVar(&criteria, "criteria", "", "markdown file with the qualification criteria (required)")
	f.BoolVar(&explain, "explain", false, "also store a 1–2 sentence reason per row in <name>_reason")
	f.StringVar(&name, "name", "qualified", "decision column name")
	f.StringVar(&budget, "budget", "", "hard spending cap in USD for the run (e.g. 0.50)")
	f.BoolVar(&dryRun, "dry-run", false, "estimate calls, cost and cache hits; call nothing, write nothing")
	f.StringArrayVar(&only, "only", nil, onlyHelp+"; only matching rows are qualified")
	f.StringSliceVar(&wf, "waterfall", []string{"llm"}, "llm providers in order (see crm provider list)")
	f.IntVar(&concurrency, "concurrency", run.DefaultConcurrency, "rows processed in parallel")
	f.IntVar(&retry, "retry", run.DefaultRetry, "retries per llm call on 429/5xx/timeouts")
	return cmd
}

// decision is the decision of an ok item ("yes", "no", "maybe"), or the item
// status otherwise.
func decision(it *run.Item) string {
	if it.Status != run.ItemOK || it.Value == nil {
		return it.Status
	}
	var answer struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal([]byte(*it.Value), &answer); err != nil || answer.Value == "" {
		return it.Status
	}
	return answer.Value
}
