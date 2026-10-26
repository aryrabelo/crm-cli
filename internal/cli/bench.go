package cli

import (
	"fmt"
	"os"
	"time"

	"github.com/jdanielnd/crm-cli/internal/bench"
	"github.com/jdanielnd/crm-cli/internal/format"
	"github.com/jdanielnd/crm-cli/internal/model"
	"github.com/jdanielnd/crm-cli/internal/provider"
	"github.com/spf13/cobra"
)

var benchColumns = []format.ColumnDef{
	{Header: "Task", Field: "task"},
	{Header: "Mode", Field: "mode"},
	{Header: "Tool Calls", Field: "calls"},
	{Header: "Errors", Field: "errors"},
	{Header: "Provider Calls", Field: "provider_calls"},
	{Header: "Tokens", Field: "tokens"},
	{Header: "Cost", Field: "cost"},
	{Header: "Wall", Field: "wall"},
	{Header: "KB", Field: "kb"},
	{Header: "Check", Field: "check"},
}

var benchCompareColumns = []format.ColumnDef{
	{Header: "Task", Field: "task"},
	{Header: "Batch Calls", Field: "batch_calls"},
	{Header: "Per-Entity Calls", Field: "per_entity_calls"},
	{Header: "Gap", Field: "gap"},
	{Header: "Batch Tokens", Field: "batch_tokens"},
	{Header: "Batch Cost", Field: "batch_cost"},
	{Header: "Batch Wall", Field: "batch_wall"},
	{Header: "Per-Entity Wall", Field: "per_entity_wall"},
	{Header: "Batch KB", Field: "batch_kb"},
	{Header: "Per-Entity KB", Field: "per_entity_kb"},
	{Header: "Batch Check", Field: "batch_check"},
	{Header: "Per-Entity Check", Field: "per_entity_check"},
}

func registerBenchCommands(rootCmd *cobra.Command) {
	cmd := &cobra.Command{
		Use:   "bench",
		Short: "Benchmark what agents pay per task over MCP (calls, cost, time)",
	}
	cmd.AddCommand(benchRunCmd())
	rootCmd.AddCommand(cmd)
}

func benchRunCmd() *cobra.Command {
	var dir, baseline, transport string
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the bench tasks and print calls, cost and time per task",
		Long: `Run every bench task (*.json in --tasks) through an MCP client against a
crm server on a fresh in-memory database, with local fake providers (an
enrichment API and an OpenAI-compatible llm), so runs are deterministic and
free. Per task it prints MCP tool calls, provider calls, llm tokens, cost at
the fake providers' prices, wall time, payload size and whether the task's
check passed, plus totals.

--baseline per-entity runs each task a second time with the upstream
one-entity tools only and compares call counts (n/a when those tools cannot
do the task). --transport http goes through the streamable HTTP transport
(with bearer auth) instead of an in-process client. Exits 1 when a check fails.`,
		Example: `  crm bench run
  crm bench run --baseline per-entity
  crm bench run --tasks bench --transport http -f json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if baseline != "" && baseline != string(bench.PerEntity) {
				return model.NewExitError(model.ErrValidation, "--baseline must be %s", bench.PerEntity)
			}
			tasks, err := bench.LoadTasks(dir)
			if err != nil {
				return err
			}
			runner, err := bench.NewRunner(transport)
			if err != nil {
				return model.NewExitError(model.ErrValidation, "--transport: %s", err)
			}
			defer runner.Close()
			modes := []bench.Mode{bench.Batch}
			if baseline != "" {
				modes = append(modes, bench.PerEntity)
			}
			results := map[bench.Mode][]*bench.Result{}
			failed := 0
			for _, t := range tasks {
				for _, m := range modes {
					res, err := runner.Run(cmd.Context(), t, m)
					if err != nil {
						return err
					}
					switch {
					case res.NA != "" || res.Pass:
					case m == bench.Batch:
						failed++
						fmt.Fprintf(os.Stderr, "bench: %s failed: %s\n", res.Task, res.Failure)
					default:
						// A baseline that cannot get the task right is a finding,
						// not a harness failure.
						fmt.Fprintf(os.Stderr, "bench: %s with %s tools only: %s\n", res.Task, res.Mode, res.Failure)
					}
					results[m] = append(results[m], res)
				}
			}
			var data []map[string]any
			columns := benchColumns
			if baseline == "" {
				data = benchRows(results[bench.Batch])
			} else {
				data, columns = benchCompareRows(results[bench.Batch], results[bench.PerEntity]), benchCompareColumns
			}
			if err := format.Output(os.Stdout, resolveFormat(), data, columns, flagQuiet); err != nil {
				return err
			}
			if failed > 0 {
				return fmt.Errorf("%d bench task(s) failed their check", failed)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&dir, "tasks", "bench", "directory of bench task files (*.json)")
	f.StringVar(&baseline, "baseline", "", "also run the tasks with the one-entity tools only: per-entity")
	f.StringVar(&transport, "transport", bench.InProcess, "MCP transport: inprocess or http")
	return cmd
}

func wallText(d time.Duration) string { return d.Round(100 * time.Microsecond).String() }

func kb(bytes int) string { return fmt.Sprintf("%.1f", float64(bytes)/1024) }

func checkText(r *bench.Result) string {
	switch {
	case r.NA != "":
		return "n/a"
	case r.Pass:
		return "pass"
	default:
		return "FAIL"
	}
}

func benchRows(results []*bench.Result) []map[string]any {
	data := make([]map[string]any, 0, len(results)+1)
	var total bench.Result
	passed := 0
	for _, r := range results {
		data = append(data, map[string]any{
			"task": r.Task, "mode": string(r.Mode), "calls": r.Calls, "errors": r.Errors,
			"provider_calls": r.ProviderCalls, "tokens": r.TokensIn + r.TokensOut, "tokens_in": r.TokensIn,
			"tokens_out": r.TokensOut, "cost": r.Cost.String(), "cost_usd": r.Cost.USD(), "wall": wallText(r.Wall),
			"wall_ms": r.Wall.Milliseconds(), "kb": kb(r.Bytes), "bytes": r.Bytes, "check": checkText(r), "failure": r.Failure,
		})
		total.Calls += r.Calls
		total.Errors += r.Errors
		total.ProviderCalls += r.ProviderCalls
		total.TokensIn += r.TokensIn
		total.TokensOut += r.TokensOut
		total.Cost += r.Cost
		total.Wall += r.Wall
		total.Bytes += r.Bytes
		if r.Pass {
			passed++
		}
	}
	return append(data, map[string]any{
		"task": "TOTAL", "mode": "", "calls": total.Calls, "errors": total.Errors,
		"provider_calls": total.ProviderCalls, "tokens": total.TokensIn + total.TokensOut, "tokens_in": total.TokensIn,
		"tokens_out": total.TokensOut, "cost": total.Cost.String(), "cost_usd": total.Cost.USD(), "wall": wallText(total.Wall),
		"wall_ms": total.Wall.Milliseconds(), "kb": kb(total.Bytes), "bytes": total.Bytes,
		"check": fmt.Sprintf("%d/%d pass", passed, len(results)), "failure": "",
	})
}

// benchCompareRows pairs the batch and per-entity result of every task;
// the totals only add tasks possible in both modes.
func benchCompareRows(batch, perEntity []*bench.Result) []map[string]any {
	data := make([]map[string]any, 0, len(batch)+1)
	var bCalls, pCalls, bBytes, pBytes, both int
	var bWall, pWall time.Duration
	var bCost provider.Cost
	var bTokens int64
	for i, b := range batch {
		p := perEntity[i]
		row := map[string]any{
			"task": b.Task, "batch_calls": b.Calls, "batch_cost": b.Cost.String(), "batch_cost_usd": b.Cost.USD(),
			"batch_tokens": b.TokensIn + b.TokensOut, "batch_provider_calls": b.ProviderCalls,
			"batch_wall": wallText(b.Wall), "batch_wall_ms": b.Wall.Milliseconds(), "batch_kb": kb(b.Bytes),
			"batch_check": checkText(b), "per_entity_check": checkText(p),
			"per_entity_calls": "n/a", "gap": "n/a", "per_entity_wall": "n/a", "per_entity_kb": "n/a", "per_entity_na": p.NA,
		}
		if p.NA == "" {
			row["per_entity_calls"], row["per_entity_wall"], row["per_entity_kb"] = p.Calls, wallText(p.Wall), kb(p.Bytes)
			row["per_entity_wall_ms"] = p.Wall.Milliseconds()
			row["gap"] = fmt.Sprintf("%.1fx", bench.Ratio(p.Calls, b.Calls))
			bCalls += b.Calls
			pCalls += p.Calls
			bBytes += b.Bytes
			pBytes += p.Bytes
			bWall += b.Wall
			pWall += p.Wall
			both++
		}
		bCost += b.Cost
		bTokens += b.TokensIn + b.TokensOut
		data = append(data, row)
	}
	return append(data, map[string]any{
		"task": fmt.Sprintf("TOTAL (calls/wall/KB: %d tasks in both modes)", both), "batch_calls": bCalls, "per_entity_calls": pCalls,
		"gap": fmt.Sprintf("%.1fx", bench.Ratio(pCalls, bCalls)), "batch_cost": bCost.String(), "batch_cost_usd": bCost.USD(),
		"batch_tokens": bTokens,
		"batch_wall": wallText(bWall), "per_entity_wall": wallText(pWall), "batch_kb": kb(bBytes), "per_entity_kb": kb(pBytes),
		"batch_check": "", "per_entity_check": "", "per_entity_na": "",
	})
}
