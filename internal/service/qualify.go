package service

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/jdanielnd/crm-cli/internal/column"
	"github.com/jdanielnd/crm-cli/internal/model"
	"github.com/jdanielnd/crm-cli/internal/provider"
	"github.com/jdanielnd/crm-cli/internal/run"
	"github.com/jdanielnd/crm-cli/internal/table"
)

// QualifyDecisions are the values of a qualify decision column.
var QualifyDecisions = []string{"yes", "no", "maybe"}

// DefaultQualifyColumn is the decision column name when none is given.
const DefaultQualifyColumn = "qualified"

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

// QualifyOptions configures Qualify.
type QualifyOptions struct {
	Table string
	// Criteria is the markdown text rows are judged against.
	Criteria string
	// Name is the decision column (default DefaultQualifyColumn).
	Name    string
	Explain bool
	// Only restricts the run to rows matching every filter.
	Only []string
	// Waterfall lists the llm providers (default llm).
	Waterfall   []string
	Budget      provider.Cost
	BudgetSet   bool
	Concurrency int
	Retry       int
	// RetryUncertain: see run.Options.RetryUncertain.
	RetryUncertain bool
	DryRun         bool
}

// QualifyResult is a dry-run estimate (Estimate) or a finished run (Run)
// with the decision counts.
type QualifyResult struct {
	Table  string
	Column string
	Output string
	// Reason is the reason column ("" without Explain).
	Reason string
	// Redefine is set when the decision column already existed.
	Redefine bool
	Estimate *run.Estimate
	Run      *run.Result
	// Counts maps yes/no/maybe and the item statuses that carry no decision
	// (empty, failed, skipped, pending) to row counts.
	Counts map[string]int
}

// Qualify creates (or redefines) the ai decision column of a table — values
// yes, no or maybe, with a reason column when Explain — whose prompt is the
// criteria plus every other column, and fills it through crm run.
func (s *Service) Qualify(ctx context.Context, opts QualifyOptions) (*QualifyResult, error) {
	if strings.TrimSpace(opts.Criteria) == "" {
		return nil, model.NewExitError(model.ErrValidation, "criteria are empty")
	}
	if opts.Name == "" {
		opts.Name = DefaultQualifyColumn
	}
	if len(opts.Waterfall) == 0 {
		opts.Waterfall = []string{"llm"}
	}
	ts := table.NewStore(s.db)
	ws, err := ts.FindWorkspace(ctx, opts.Table)
	if err != nil {
		return nil, err
	}
	cols, err := ts.Columns(ctx, ws.ID)
	if err != nil {
		return nil, err
	}
	name := opts.Name
	res := &QualifyResult{Table: ws.Name, Column: name, Output: "enum:" + strings.Join(QualifyDecisions, "|")}
	if opts.Explain {
		res.Reason = ReasonColumn(name)
	}
	c := &column.Column{WorkspaceID: ws.ID, Name: name, Type: column.AI,
		Config: column.Config{Output: res.Output, Reason: res.Reason, Waterfall: opts.Waterfall}}
	var fields []string
	for _, o := range cols {
		switch {
		case o.Name == name:
			if o.Type != column.AI {
				return nil, model.NewExitError(model.ErrConflict, "column %q already exists as a %s column; pick another --name", name, o.Type)
			}
			c.ID = o.ID
		case o.Name == ReasonColumn(name), strings.Contains(o.Name, "{{"), strings.Contains(o.Name, "}}"):
		default:
			fields = append(fields, o.Name)
		}
	}
	c.Config.Prompt = qualifyPrompt(opts.Criteria, fields)
	res.Redefine = c.ID != 0

	runOpts := run.Options{Table: ws.Name, Columns: []string{name}, Concurrency: opts.Concurrency, Retry: opts.Retry,
		Only: opts.Only, Budget: opts.Budget, BudgetSet: opts.BudgetSet, RetryUncertain: opts.RetryUncertain}
	engine := s.Engine()
	if opts.DryRun {
		preview := []*column.Column{c}
		others := slices.DeleteFunc(slices.Clone(cols), func(o *column.Column) bool { return o.Name == name })
		if r := res.Reason; r != "" && !slices.ContainsFunc(cols, func(o *column.Column) bool { return o.Name == r }) {
			rc := &column.Column{WorkspaceID: ws.ID, Name: r, Type: column.Input}
			preview = append([]*column.Column{rc}, preview...)
			others = append(others, rc)
		}
		if err := c.Validate(others); err != nil {
			return nil, err
		}
		runOpts.Preview = preview
		if res.Estimate, err = engine.Estimate(ctx, runOpts); err != nil {
			return nil, err
		}
		return res, nil
	}

	if err := s.saveColumn(ctx, c, res.Redefine); err != nil {
		return nil, err
	}
	if res.Run, err = engine.Execute(ctx, runOpts); err != nil {
		return nil, err
	}
	items, err := run.NewStore(s.db).Items(ctx, res.Run.Run.ID, false)
	if err != nil {
		return nil, err
	}
	res.Counts = map[string]int{}
	for _, it := range items {
		res.Counts[decision(it)]++
	}
	return res, nil
}

// QualifyData is the output record of a finished qualify run.
func QualifyData(res *QualifyResult) map[string]any {
	r, st := res.Run.Run, res.Run.Run.Stats
	return map[string]any{
		"run": r.ID, "table": res.Table, "column": res.Column, "reason_column": res.Reason, "status": string(r.Status),
		"yes": res.Counts["yes"], "no": res.Counts["no"], "maybe": res.Counts["maybe"],
		"empty": res.Counts[run.ItemEmpty], "failed": res.Counts[run.ItemFailed],
		"skipped": res.Counts[run.ItemSkipped], "pending": res.Counts[run.ItemPending],
		"cache_hits": st.CacheHits, "calls": st.Calls, "tokens_in": st.TokensIn, "tokens_out": st.TokensOut,
		"spent": r.Spent.String(), "spent_usd": r.Spent.USD(), "budget": r.Budget.String(), "budget_usd": r.Budget.USD(),
	}
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
