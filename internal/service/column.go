package service

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"github.com/jdanielnd/crm-cli/internal/column"
	"github.com/jdanielnd/crm-cli/internal/db/repo"
	"github.com/jdanielnd/crm-cli/internal/model"
	"github.com/jdanielnd/crm-cli/internal/provider"
	"github.com/jdanielnd/crm-cli/internal/table"
	"github.com/jdanielnd/crm-cli/internal/waterfall"
)

// ColumnSpec declares a column (crm column add, crm_column_add).
type ColumnSpec struct {
	Name string `json:"name"`
	// Type is input, formula, enrich or ai.
	Type      string   `json:"type"`
	Waterfall []string `json:"waterfall,omitempty"`
	// From is the enrich input column (default: the table key).
	From  string `json:"from,omitempty"`
	Field string `json:"field,omitempty"`
	Expr  string `json:"expr,omitempty"`
	// Prompt is the ai prompt text with {{column}} placeholders.
	Prompt string `json:"prompt,omitempty"`
	// Output is the ai answer type (default text).
	Output string `json:"output,omitempty"`
	// Explain stores a reason per row in <name>_reason (ai only).
	Explain bool `json:"explain,omitempty"`
}

// ReasonColumn is the reason column of an ai column with --explain.
func ReasonColumn(name string) string { return name + "_reason" }

// AddColumn declares a column on a table: enrich columns read the table key
// by default, ai columns default to text answers from the llm provider.
func (s *Service) AddColumn(ctx context.Context, tableName string, spec ColumnSpec) (*column.Column, error) {
	c, err := s.buildColumn(ctx, tableName, spec)
	if err != nil {
		return nil, err
	}
	if err := s.saveColumn(ctx, c, false); err != nil {
		return nil, err
	}
	return c, nil
}

// PreviewColumn builds the column spec declares, exactly as AddColumn would
// save it, and validates it against the table's columns without saving it:
// the run.Options.Preview of an estimate made before the column exists.
func (s *Service) PreviewColumn(ctx context.Context, tableName string, spec ColumnSpec) (*column.Column, error) {
	c, err := s.buildColumn(ctx, tableName, spec)
	if err != nil {
		return nil, err
	}
	cols, err := table.NewStore(s.db).Columns(ctx, c.WorkspaceID)
	if err != nil {
		return nil, err
	}
	if r := c.Config.Reason; r != "" && !slices.ContainsFunc(cols, func(o *column.Column) bool { return o.Name == r }) {
		cols = append(cols, &column.Column{WorkspaceID: c.WorkspaceID, Name: r, Type: column.Input})
	}
	if err := c.Validate(cols); err != nil {
		return nil, err
	}
	return c, nil
}

// buildColumn turns spec into the column of table tableName, defaults
// applied.
func (s *Service) buildColumn(ctx context.Context, tableName string, spec ColumnSpec) (*column.Column, error) {
	t, err := column.ParseType(spec.Type)
	if err != nil {
		return nil, err
	}
	if t != column.AI && (spec.Prompt != "" || spec.Output != "" || spec.Explain) {
		return nil, model.NewExitError(model.ErrValidation, "--prompt, --output and --explain are for --type ai")
	}
	ws, err := table.NewStore(s.db).FindWorkspace(ctx, tableName)
	if err != nil {
		return nil, err
	}
	c := &column.Column{
		WorkspaceID: ws.ID,
		Name:        spec.Name,
		Type:        t,
		Config: column.Config{Expr: spec.Expr, Waterfall: spec.Waterfall, From: spec.From, Field: spec.Field,
			Prompt: spec.Prompt, Output: spec.Output},
	}
	switch t {
	case column.Enrich:
		if c.Config.From == "" && ws.KeyColumn != nil {
			c.Config.From = *ws.KeyColumn
		}
	case column.AI:
		if c.Config.Output == "" {
			c.Config.Output = column.OutputText
		}
		if len(c.Config.Waterfall) == 0 {
			c.Config.Waterfall = []string{"llm"}
		}
		if spec.Explain {
			c.Config.Reason = ReasonColumn(c.Name)
		}
	}
	return c, nil
}

// saveColumn adds c (or, with update, redefines the existing column of the
// same name) in one transaction: the reason input column of an ai column is
// created when missing (and placed right after a new ai column), c is
// validated, its providers are opened (so a misconfigured provider fails
// now, not at run time) and its per-row cost estimate is recorded.
func (s *Service) saveColumn(ctx context.Context, c *column.Column, update bool) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		ts := table.NewStore(tx)
		cols, err := ts.Columns(ctx, c.WorkspaceID)
		if err != nil {
			return err
		}
		var newReason *column.Column
		if r := c.Config.Reason; r != "" {
			i := slices.IndexFunc(cols, func(o *column.Column) bool { return o.Name == r })
			switch {
			case i < 0:
				newReason = &column.Column{WorkspaceID: c.WorkspaceID, Name: r, Type: column.Input}
				if err := ts.AddColumn(ctx, newReason); err != nil {
					return err
				}
				cols = append(cols, newReason)
			case cols[i].Type != column.Input:
				return model.NewExitError(model.ErrConflict, "reason column %q already exists as a %s column", r, cols[i].Type)
			}
		}
		if err := c.Validate(slices.DeleteFunc(slices.Clone(cols), func(o *column.Column) bool { return update && o.Name == c.Name })); err != nil {
			return err
		}
		if c.Type.Runnable() {
			if c.CostEst, err = s.worstCost(ctx, tx, c); err != nil {
				return err
			}
		}
		if update {
			err = ts.UpdateColumn(ctx, c)
		} else {
			err = ts.AddColumn(ctx, c)
		}
		if err != nil {
			return err
		}
		if newReason != nil && !update {
			return ts.SwapPositions(ctx, newReason, c)
		}
		return nil
	})
}

// Definition summarizes a column config in one line.
// worstCost is the worst-case price of one cell of c at the provider prices
// stored now.
func (s *Service) worstCost(ctx context.Context, db repo.DBTX, c *column.Column) (provider.Cost, error) {
	wf, _, err := c.Waterfall(ctx, provider.NewStore(db), s.registry, waterfall.NewCache(db))
	if err != nil {
		return 0, err
	}
	est, err := wf.Estimate(ctx, provider.Input{Value: c.Config.Prompt}, c.Config.Field, false)
	if err != nil {
		return 0, err
	}
	return est.Worst, nil
}

// RefreshCostEst recomputes CostEst of the runnable columns in place from the
// current provider prices, which crm provider set may have changed since the
// column was saved. A column whose providers cannot be opened keeps its
// stored value.
func (s *Service) RefreshCostEst(ctx context.Context, cols []*column.Column) {
	for _, c := range cols {
		if !c.Type.Runnable() {
			continue
		}
		if cost, err := s.worstCost(ctx, s.db, c); err == nil {
			c.CostEst = cost
		}
	}
}

func Definition(c *column.Column) string {
	switch c.Type {
	case column.Formula:
		return c.Config.Expr
	case column.Enrich:
		s := strings.Join(c.Config.Waterfall, " → ") + " ← " + c.Config.From
		if c.Config.Field != "" {
			s += " ." + c.Config.Field
		}
		return s
	case column.AI:
		s := c.Config.Output + " ← " + strings.Join(c.Config.Waterfall, " → ")
		if c.Config.Reason != "" {
			s += " (+ reason in " + c.Config.Reason + ")"
		}
		return s
	default:
		return ""
	}
}

// ColumnData is the output record of a column; filled of rows cells are set.
func ColumnData(c *column.Column, filled, rows int) map[string]any {
	return map[string]any{
		"id":           c.ID,
		"name":         c.Name,
		"type":         string(c.Type),
		"config":       c.Config,
		"definition":   Definition(c),
		"cost_est":     c.CostEst.String(),
		"cost_est_usd": c.CostEst.USD(),
		"filled":       fmt.Sprintf("%d/%d", filled, rows),
	}
}
