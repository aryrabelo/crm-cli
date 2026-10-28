package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"

	"github.com/invopop/jsonschema"
	"github.com/jdanielnd/crm-cli/internal/csvio"
	"github.com/jdanielnd/crm-cli/internal/model"
	"github.com/jdanielnd/crm-cli/internal/provider"
	"github.com/jdanielnd/crm-cli/internal/run"
	"github.com/jdanielnd/crm-cli/internal/service"
	"github.com/jdanielnd/crm-cli/internal/table"
	"github.com/jdanielnd/crm-cli/internal/value"
	gomcp "github.com/mark3labs/mcp-go/mcp"
)

// mcpSource is the provenance recorded on rows written through MCP.
const mcpSource = "mcp"

// registerBatchTools registers the data-plane tools. They take arrays where
// an agent would otherwise loop (rows, columns, run ids) and report errors
// per item, so one call does what the per-entity tools need N calls for.
func (s *Server) registerBatchTools() {
	s.mcp.AddTool(gomcp.NewTool("crm_table_import",
		gomcp.WithDescription("Create or append to a table from CSV text or an array of row objects, in one call. "+
			"With a key column, rows whose key exists are updated. A header error (empty or repeated header, key column "+
			"missing from the header) fails the whole call and writes nothing; a row error (empty or repeated key) is "+
			"reported in that row's result and the other rows are written."),
		gomcp.WithInputSchema[importArgs]()), s.tableImport)
	s.mcp.AddTool(gomcp.NewTool("crm_rows_upsert_many",
		gomcp.WithDescription("Upsert many rows into an existing table: each row object sets only the cells it names; "+
			"rows are matched by the table key. Errors are reported per row."),
		gomcp.WithInputSchema[upsertArgs]()), s.rowsUpsertMany)
	s.mcp.AddTool(gomcp.NewTool("crm_table_query",
		gomcp.WithDescription("Query a table: filter expressions (AND), selected columns, limit/offset. Formula columns are evaluated; "+
			"each row has _row (its position). total counts every matching row."),
		gomcp.WithInputSchema[queryArgs]()), s.tableQuery)
	s.mcp.AddTool(gomcp.NewTool("crm_column_add",
		gomcp.WithDescription("Declare one or more columns on a table (input, formula, enrich via a provider waterfall, ai via an llm prompt). "+
			"Errors are reported per column."),
		gomcp.WithInputSchema[columnAddArgs]()), s.columnAdd)
	s.mcp.AddTool(gomcp.NewTool("crm_run_start",
		gomcp.WithDescription("Fill enrich/ai columns of a table in one batch run (cache-first, budget-capped, resumable). "+
			"dry_run estimates calls and cost without calling anything. wait=false returns the run id at once; poll crm_run_status."),
		gomcp.WithInputSchema[runStartArgs]()), s.runStart)
	s.mcp.AddTool(gomcp.NewTool("crm_run_status",
		gomcp.WithDescription("Status, counts, calls, tokens and spend of one or more runs; items=true adds every row × column item."),
		gomcp.WithInputSchema[runStatusArgs]()), s.runStatus)
	s.mcp.AddTool(gomcp.NewTool("crm_qualify",
		gomcp.WithDescription("Qualify every row of a table against markdown criteria: an ai column answers yes/no/maybe "+
			"(plus a reason with explain) in one budget-capped run. dry_run estimates only."),
		gomcp.WithInputSchema[qualifyArgs]()), s.qualify)
	s.mcp.AddTool(gomcp.NewTool("crm_export",
		gomcp.WithDescription("Export the rows of a table matching the filters as csv, tsv or json text."),
		gomcp.WithInputSchema[exportArgs]()), s.export)
	s.mcp.AddTool(gomcp.NewTool("crm_push_dedupe",
		gomcp.WithDescription("Turn table rows into people or organizations, deduplicating by email, domain or fuzzy name "+
			"against existing entities and earlier rows. dry_run reports the plan and writes nothing. Errors are per row."),
		gomcp.WithInputSchema[pushArgs]()), s.pushDedupe)
}

// usd is a dollar amount given as a JSON number (0.5) or string ("0.50").
type usd struct{ provider.Cost }

func (u *usd) UnmarshalJSON(b []byte) error {
	var text string
	if err := json.Unmarshal(b, &text); err != nil {
		var f float64
		if err := json.Unmarshal(b, &f); err != nil {
			return fmt.Errorf("budget must be a number or a string of dollars")
		}
		text = strconv.FormatFloat(f, 'f', -1, 64)
	}
	c, err := provider.ParseUSD(text)
	if err != nil {
		return err
	}
	u.Cost = c
	return nil
}

// JSONSchema describes usd for tool input schemas.
func (usd) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		OneOf:       []*jsonschema.Schema{{Type: "number"}, {Type: "string"}},
		Description: `Hard spending cap in USD, e.g. 0.5 or "0.50"`,
	}
}

func budgetOpts(b *usd) (provider.Cost, bool) {
	if b == nil {
		return 0, false
	}
	return b.Cost, true
}

func retryOr(r *int) int {
	if r == nil {
		return run.DefaultRetry
	}
	return *r
}

// bind decodes the tool arguments into args.
func bind(req gomcp.CallToolRequest, args any) *gomcp.CallToolResult {
	if err := req.BindArguments(args); err != nil {
		return gomcp.NewToolResultError("invalid arguments: " + err.Error())
	}
	return nil
}

// sortedUnion is the documented default order of new columns: the sorted
// names of every key of every row, so it does not depend on row order.
func sortedUnion(rows []map[string]any) []string {
	keys := map[string]struct{}{}
	for _, r := range rows {
		for k := range r {
			keys[k] = struct{}{}
		}
	}
	return slices.Sorted(maps.Keys(keys))
}

// orSorted is columns, or the sorted union of the row keys when it is empty.
func orSorted(columns []string, rows []map[string]any) []string {
	if len(columns) > 0 {
		return columns
	}
	return sortedUnion(rows)
}

// rowCells converts row objects to cells (value.Text of each JSON value),
// keys in sorted order; the columns argument of Upsert orders new columns.
func rowCells(rows []map[string]any) [][]table.Cell {
	out := make([][]table.Cell, len(rows))
	for i, r := range rows {
		cells := make([]table.Cell, 0, len(r))
		for _, k := range slices.Sorted(maps.Keys(r)) {
			cells = append(cells, table.Cell{Column: k, Value: value.Text(r[k])})
		}
		out[i] = cells
	}
	return out
}

// rowResult is the per-item outcome of an import or upsert.
type rowResult struct {
	Index  int    `json:"index"`
	Row    int    `json:"row,omitempty"`
	Key    string `json:"key,omitempty"`
	Action string `json:"action"`
	Error  string `json:"error,omitempty"`
}

func importData(res *table.ImportResult) map[string]any {
	results := make([]rowResult, len(res.Results))
	for i, r := range res.Results {
		rr := rowResult{Index: r.Index, Row: r.Position}
		if r.Key != nil {
			rr.Key = *r.Key
		}
		switch {
		case r.Err != nil:
			rr.Action, rr.Error = "error", errText(r.Err)
		case r.Created:
			rr.Action = "created"
		default:
			rr.Action = "updated"
		}
		results[i] = rr
	}
	newCols := res.NewColumns
	if newCols == nil {
		newCols = []string{}
	}
	return map[string]any{
		"table": res.Workspace.Name, "key_column": res.Workspace.KeyColumn, "rows": res.Rows,
		"created": res.Created, "updated": res.Updated, "failed": res.Failed,
		"new_columns": newCols, "results": results,
	}
}

type importArgs struct {
	Table   string           `json:"table" jsonschema_description:"Table name (created on first import)"`
	Key     string           `json:"key,omitempty" jsonschema_description:"Key column used to upsert rows (fixed when the table is created)"`
	CSV     string           `json:"csv,omitempty" jsonschema_description:"CSV text with a header row (use csv or rows)"`
	Rows    []map[string]any `json:"rows,omitempty" jsonschema_description:"Row objects: column name to value (use csv or rows)"`
	Columns []string         `json:"columns,omitempty" jsonschema_description:"Order of new columns for rows (default: sorted names)"`
}

func (s *Server) tableImport(ctx context.Context, req gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	var a importArgs
	if r := bind(req, &a); r != nil {
		return r, nil
	}
	if (a.CSV == "") == (a.Rows == nil) {
		return gomcp.NewToolResultError("give exactly one of csv or rows"), nil
	}
	opts := table.ImportOptions{Name: a.Table, Key: a.Key, Source: mcpSource}
	var res *table.ImportResult
	if a.CSV != "" {
		t, err := csvio.Parse([]byte(a.CSV))
		if err != nil {
			return gomcp.NewToolResultError("invalid csv: " + err.Error()), nil
		}
		// The same service call as `crm table import`, so both behave alike.
		res, err = s.svc.ImportCSV(ctx, opts, t)
		if err != nil {
			return toolError(err)
		}
	} else {
		var err error
		if res, err = s.svc.UpsertRows(ctx, opts, orSorted(a.Columns, a.Rows), rowCells(a.Rows), false); err != nil {
			return toolError(err)
		}
	}
	return jsonResult(importData(res))
}

type upsertArgs struct {
	Table   string           `json:"table" jsonschema_description:"Existing table name"`
	Rows    []map[string]any `json:"rows" jsonschema_description:"Row objects: column name to value; each sets only the cells it names"`
	Columns []string         `json:"columns,omitempty" jsonschema_description:"Order of new columns (default: sorted names)"`
}

func (s *Server) rowsUpsertMany(ctx context.Context, req gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	var a upsertArgs
	if r := bind(req, &a); r != nil {
		return r, nil
	}
	res, err := s.svc.UpsertRows(ctx, table.ImportOptions{Name: a.Table, Source: mcpSource}, orSorted(a.Columns, a.Rows), rowCells(a.Rows), true)
	if err != nil {
		return toolError(err)
	}
	return jsonResult(importData(res))
}

type queryArgs struct {
	Table   string   `json:"table"`
	Filters []string `json:"filters,omitempty" jsonschema_description:"Filter expressions, ANDed: <column><op><value> with op = != > >= < <= ~ (regex) !~; col!= keeps non-empty"`
	Columns []string `json:"columns,omitempty" jsonschema_description:"Columns to return, in order (default: all)"`
	Limit   int      `json:"limit,omitempty" jsonschema_description:"Max rows (default 100)"`
	Offset  int      `json:"offset,omitempty" jsonschema_description:"Matching rows to skip"`
}

func (s *Server) tableQuery(ctx context.Context, req gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	var a queryArgs
	if r := bind(req, &a); r != nil {
		return r, nil
	}
	res, err := s.svc.Query(ctx, service.QueryOptions{Table: a.Table, Only: a.Filters, Columns: a.Columns, Limit: a.Limit, Offset: a.Offset})
	if err != nil {
		return toolError(err)
	}
	return jsonResult(res)
}

type columnAddArgs struct {
	Table   string               `json:"table"`
	Columns []service.ColumnSpec `json:"columns" jsonschema_description:"Columns to add, in order (a later column may use an earlier one)"`
}

type columnResult struct {
	Name   string         `json:"name"`
	Column map[string]any `json:"column,omitempty"`
	Error  string         `json:"error,omitempty"`
}

func (s *Server) columnAdd(ctx context.Context, req gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	var a columnAddArgs
	if r := bind(req, &a); r != nil {
		return r, nil
	}
	if len(a.Columns) == 0 {
		return gomcp.NewToolResultError("columns is empty"), nil
	}
	results := make([]columnResult, len(a.Columns))
	added := 0
	for i, spec := range a.Columns {
		results[i].Name = spec.Name
		c, err := s.svc.AddColumn(ctx, a.Table, spec)
		if err != nil {
			results[i].Error = errText(err)
			continue
		}
		results[i].Column = service.ColumnData(c, 0, 0)
		added++
	}
	return jsonResult(map[string]any{"table": a.Table, "added": added, "failed": len(a.Columns) - added, "results": results})
}

type runStartArgs struct {
	Table       string   `json:"table"`
	Columns     []string `json:"columns" jsonschema_description:"Enrich or ai columns to fill, in order"`
	Filters     []string `json:"filters,omitempty" jsonschema_description:"Only run rows matching every filter expression"`
	Budget      *usd     `json:"budget,omitempty"`
	DryRun      bool     `json:"dry_run,omitempty" jsonschema_description:"Estimate items, cache hits, calls and cost; call and write nothing"`
	Wait        *bool    `json:"wait,omitempty" jsonschema_description:"Wait for the run to finish (default true); false returns the run id at once"`
	Concurrency int      `json:"concurrency,omitempty" jsonschema_description:"Rows processed in parallel (default 4)"`
	Retry       *int     `json:"retry,omitempty" jsonschema_description:"Retries per provider call on 429/5xx/timeouts (default 2)"`
	// RetryUncertain: see run.Options.RetryUncertain.
	RetryUncertain bool `json:"retry_uncertain,omitempty" jsonschema_description:"Also retry paid POST calls that timed out after the request was sent (may pay twice; default false)"`
}

func estimateData(est *run.Estimate) map[string]any {
	cols := make([]map[string]any, len(est.Columns))
	for i, ce := range est.Columns {
		cols[i] = service.EstimateData(ce)
	}
	return map[string]any{"dry_run": true, "table": est.Table, "columns": cols, "total": service.EstimateData(&est.Total)}
}

func (s *Server) runStart(ctx context.Context, req gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	var a runStartArgs
	if r := bind(req, &a); r != nil {
		return r, nil
	}
	opts := run.Options{Table: a.Table, Columns: a.Columns, Only: a.Filters, Concurrency: a.Concurrency, Retry: retryOr(a.Retry),
		RetryUncertain: a.RetryUncertain}
	opts.Budget, opts.BudgetSet = budgetOpts(a.Budget)
	engine := s.svc.Engine()
	if a.DryRun {
		est, err := engine.Estimate(ctx, opts)
		if err != nil {
			return toolError(err)
		}
		return jsonResult(estimateData(est))
	}
	if a.Wait == nil || *a.Wait {
		res, err := engine.Execute(ctx, opts)
		if err != nil {
			return toolError(err)
		}
		return jsonResult(map[string]any{"run": service.RunData(res.Run), "wall_ms": res.Duration.Milliseconds()})
	}
	r, err := engine.Start(ctx, opts)
	if err != nil {
		return toolError(err)
	}
	opts.Resume, opts.Only = r.ID, nil
	s.runs.Start(r.ID, func(bg context.Context) error {
		_, err := engine.Execute(bg, opts)
		return err
	})
	return jsonResult(map[string]any{"run": service.RunData(r), "waiting": false})
}

type runStatusArgs struct {
	RunIDs []int64 `json:"run_ids" jsonschema_description:"Run ids"`
	Items  bool    `json:"items,omitempty" jsonschema_description:"Include every row × column item"`
}

type runStatusResult struct {
	RunID int64            `json:"run_id"`
	Run   map[string]any   `json:"run,omitempty"`
	Items []map[string]any `json:"items,omitempty"`
	// Active is set while a run started with wait=false is still processing.
	Active bool   `json:"active"`
	Error  string `json:"error,omitempty"`
}

func (s *Server) runStatus(ctx context.Context, req gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	var a runStatusArgs
	if r := bind(req, &a); r != nil {
		return r, nil
	}
	if len(a.RunIDs) == 0 {
		return gomcp.NewToolResultError("run_ids is empty"), nil
	}
	results := make([]runStatusResult, len(a.RunIDs))
	for i, id := range a.RunIDs {
		res := &results[i]
		res.RunID = id
		var runErr error
		res.Active, runErr = s.runs.State(id)
		if runErr != nil {
			res.Error = errText(runErr)
		}
		r, err := s.svc.RunStatus(ctx, id)
		if err != nil {
			res.Error = errText(err)
			continue
		}
		res.Run = service.RunData(r)
		if a.Items {
			items, err := s.svc.RunItems(ctx, id)
			if err != nil {
				res.Error = errText(err)
				continue
			}
			res.Items = make([]map[string]any, len(items))
			for j, it := range items {
				res.Items[j] = service.ItemData(it)
			}
		}
	}
	return jsonResult(map[string]any{"results": results})
}

type qualifyArgs struct {
	Table       string   `json:"table"`
	Criteria    string   `json:"criteria" jsonschema_description:"Markdown qualification criteria"`
	Name        string   `json:"name,omitempty" jsonschema_description:"Decision column (default qualified)"`
	Explain     bool     `json:"explain,omitempty" jsonschema_description:"Also store a reason per row in <name>_reason"`
	Filters     []string `json:"filters,omitempty" jsonschema_description:"Only qualify rows matching every filter expression"`
	Waterfall   []string `json:"waterfall,omitempty" jsonschema_description:"llm providers in order (default llm)"`
	Fields      []string `json:"fields,omitempty" jsonschema_description:"Columns (or column.json.path) the prompt shows per row. Default: input and formula columns plus enrich columns with a field path; ai, reason and raw enrich columns are left out"`
	FieldLimit  int      `json:"field_limit,omitempty" jsonschema_description:"Cut each field value to this many characters (default 1000)"`
	Budget      *usd     `json:"budget,omitempty"`
	DryRun      bool     `json:"dry_run,omitempty"`
	Concurrency int      `json:"concurrency,omitempty"`
	Retry       *int     `json:"retry,omitempty"`
	// RetryUncertain: see run.Options.RetryUncertain.
	RetryUncertain bool `json:"retry_uncertain,omitempty" jsonschema_description:"Also retry paid llm calls that timed out after the request was sent (may pay twice; default false)"`
}

func (s *Server) qualify(ctx context.Context, req gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	var a qualifyArgs
	if r := bind(req, &a); r != nil {
		return r, nil
	}
	opts := service.QualifyOptions{Table: a.Table, Criteria: a.Criteria, Name: a.Name, Explain: a.Explain, Only: a.Filters,
		Waterfall: a.Waterfall, Fields: a.Fields, FieldLimit: a.FieldLimit, Concurrency: a.Concurrency, Retry: retryOr(a.Retry),
		RetryUncertain: a.RetryUncertain, DryRun: a.DryRun}
	opts.Budget, opts.BudgetSet = budgetOpts(a.Budget)
	res, err := s.svc.Qualify(ctx, opts)
	if err != nil {
		return toolError(err)
	}
	if res.Estimate != nil {
		out := estimateData(res.Estimate)
		out["column"], out["redefine"], out["fields"], out["field_limit"] = res.Column, res.Redefine, res.Fields, res.FieldLimit
		return jsonResult(out)
	}
	return jsonResult(service.QualifyData(res))
}

type exportArgs struct {
	Table   string   `json:"table"`
	Filters []string `json:"filters,omitempty"`
	Columns []string `json:"columns,omitempty" jsonschema_description:"Columns to export, in order (default: all)"`
	Format  string   `json:"format,omitempty" jsonschema:"enum=csv,enum=tsv,enum=json" jsonschema_description:"csv (default), tsv or json"`
}

func (s *Server) export(ctx context.Context, req gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	var a exportArgs
	if r := bind(req, &a); r != nil {
		return r, nil
	}
	_, view, err := s.svc.View(ctx, a.Table, a.Filters)
	if err != nil {
		return toolError(err)
	}
	if view, err = view.Project(a.Columns); err != nil {
		return toolError(err)
	}
	var buf bytes.Buffer
	if err := service.Export(&buf, view, a.Format); err != nil {
		return toolError(err)
	}
	f := a.Format
	if f == "" {
		f = "csv"
	}
	return jsonResult(map[string]any{"table": a.Table, "format": f, "rows": len(view.Rows), "content": buf.String()})
}

type pushArgs struct {
	Table     string   `json:"table"`
	To        string   `json:"to" jsonschema:"enum=person,enum=org" jsonschema_description:"person or org"`
	Dedupe    string   `json:"dedupe,omitempty" jsonschema:"enum=email,enum=domain,enum=fuzz" jsonschema_description:"email (people, default), domain (orgs, default) or fuzz (similar names)"`
	Filters   []string `json:"filters,omitempty" jsonschema_description:"Only push rows matching every filter expression"`
	Map       []string `json:"map,omitempty" jsonschema_description:"column=field overrides, as crm import --map"`
	Threshold *float64 `json:"threshold,omitempty" jsonschema_description:"Fuzz name similarity cut-off 0-1 (default 0.88)"`
	DryRun    bool     `json:"dry_run,omitempty" jsonschema_description:"Report creates/updates/merges; write nothing"`
}

func (s *Server) pushDedupe(ctx context.Context, req gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	var a pushArgs
	if r := bind(req, &a); r != nil {
		return r, nil
	}
	res, err := s.svc.Push(ctx, service.PushOptions{Table: a.Table, To: a.To, Dedupe: a.Dedupe, Only: a.Filters,
		Maps: a.Map, Threshold: a.Threshold, DryRun: a.DryRun})
	if err != nil {
		return toolError(err)
	}
	return jsonResult(res)
}

// errText is the client-facing message of err: the message of a not found,
// conflict or validation error, "internal error" otherwise (database and
// network details stay out of tool results).
func errText(err error) string {
	switch {
	case errors.Is(err, model.ErrNotFound):
		return "not found: " + err.Error()
	case errors.Is(err, model.ErrConflict):
		return "conflict: " + err.Error()
	case errors.Is(err, model.ErrValidation):
		return "validation error: " + err.Error()
	default:
		return "internal error"
	}
}

// toolError is the tool result of a call-level error.
func toolError(err error) (*gomcp.CallToolResult, error) {
	return gomcp.NewToolResultError(errText(err)), nil
}
