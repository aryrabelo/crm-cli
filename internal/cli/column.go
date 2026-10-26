package cli

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/jdanielnd/crm-cli/internal/column"
	"github.com/jdanielnd/crm-cli/internal/format"
	"github.com/jdanielnd/crm-cli/internal/model"
	"github.com/jdanielnd/crm-cli/internal/provider"
	"github.com/jdanielnd/crm-cli/internal/table"
	"github.com/jdanielnd/crm-cli/internal/value"
	"github.com/jdanielnd/crm-cli/internal/waterfall"
	"github.com/spf13/cobra"
)

func registerColumnCommands(rootCmd *cobra.Command) {
	cmd := &cobra.Command{
		Use:   "column",
		Short: "Declare table columns: input, formula, enrich, ai",
	}
	cmd.AddCommand(columnAddCmd(), columnListCmd(), columnRmCmd())
	rootCmd.AddCommand(cmd)
}

var columnColumns = []format.ColumnDef{
	{Header: "Column", Field: "name"},
	{Header: "Type", Field: "type"},
	{Header: "Definition", Field: "definition"},
	{Header: "Max Cost/Row", Field: "cost_est"},
	{Header: "Filled", Field: "filled"},
}

// definition summarizes a column config in one line.
func definition(c *column.Column) string {
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

// readPrompt returns the content of the regular file named arg, or arg
// itself when it names no file (an inline prompt).
func readPrompt(arg string) (string, error) {
	if info, err := os.Stat(arg); err != nil || !info.Mode().IsRegular() {
		return arg, nil
	}
	b, err := os.ReadFile(arg)
	if err != nil {
		return "", fmt.Errorf("read prompt %s: %w", arg, err)
	}
	return string(b), nil
}

// reasonColumn is the default reason column of an ai column (--explain).
func reasonColumn(name string) string { return name + "_reason" }

// saveColumn adds c (or, with update, redefines the existing column of the
// same name) in one transaction: the reason input column of an ai column is
// created when missing (and placed right after a new ai column), c is
// validated, its providers are opened (so a misconfigured provider fails
// now, not at run time) and its per-row cost estimate is recorded.
func saveColumn(ctx context.Context, db *sql.DB, c *column.Column, update bool) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
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
		wf, _, err := c.Waterfall(ctx, provider.NewStore(tx), newRegistry(), waterfall.NewCache(tx))
		if err != nil {
			return err
		}
		est, err := wf.Estimate(ctx, provider.Input{Value: c.Config.Prompt}, c.Config.Field, false)
		if err != nil {
			return err
		}
		c.CostEst = est.Worst
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
		if err := ts.SwapPositions(ctx, newReason, c); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func columnData(c *column.Column, filled, rows int) map[string]any {
	return map[string]any{
		"id":           c.ID,
		"name":         c.Name,
		"type":         string(c.Type),
		"config":       c.Config,
		"definition":   definition(c),
		"cost_est":     c.CostEst.String(),
		"cost_est_usd": c.CostEst.USD(),
		"filled":       fmt.Sprintf("%d/%d", filled, rows),
	}
}

func columnAddCmd() *cobra.Command {
	var typ, from, field, expr, prompt, output string
	var wf []string
	var explain bool
	cmd := &cobra.Command{
		Use:   "add <table> <name>",
		Short: "Add a column to a table",
		Long: `Add a column to a table.

  input    an empty column filled by later imports
  formula  computed on read from a template: --expr "{{first}} {{last}}"
           ({{col.json.path}} reaches into JSON values)
  enrich   filled by crm run through a provider waterfall: --waterfall p1,p2
           tries providers in order (first non-empty answer wins) with the
           cell of --from as input (default: the table key); --field picks a
           value inside the answer (an empty field falls through too)
  ai       filled by crm run with an llm answer per row: --prompt is a file
           (or inline text) whose {{column}} placeholders are replaced by the
           row's cells; --output types the answer: text, score:MIN-MAX,
           enum:a|b|c or bool. --explain also stores a 1–2 sentence reason in
           <name>_reason. --waterfall picks the llm providers (default llm;
           see crm provider set llm).`,
		Example: `  crm column add leads profile --type enrich --waterfall github --from login
  crm column add leads company --type enrich --waterfall github,exa --from login --field company
  crm column add leads label --type formula --expr "{{name}} ({{company}})"
  crm column add leads fit --type ai --prompt files/fit.md --output score:0-100 --explain`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := column.ParseType(typ)
			if err != nil {
				return err
			}
			if t != column.AI && (prompt != "" || output != "" || explain) {
				return model.NewExitError(model.ErrValidation, "--prompt, --output and --explain are for --type ai")
			}
			db, err := openDB()
			if err != nil {
				return err
			}
			defer db.Close()
			ctx := cmd.Context()
			ws, err := table.NewStore(db).FindWorkspace(ctx, args[0])
			if err != nil {
				return err
			}
			c := &column.Column{
				WorkspaceID: ws.ID,
				Name:        args[1],
				Type:        t,
				Config:      column.Config{Expr: expr, Waterfall: wf, From: from, Field: field, Output: output},
			}
			switch t {
			case column.Enrich:
				if c.Config.From == "" && ws.KeyColumn != nil {
					c.Config.From = *ws.KeyColumn
				}
			case column.AI:
				if prompt != "" {
					if c.Config.Prompt, err = readPrompt(prompt); err != nil {
						return err
					}
				}
				if c.Config.Output == "" {
					c.Config.Output = column.OutputText
				}
				if len(c.Config.Waterfall) == 0 {
					c.Config.Waterfall = []string{"llm"}
				}
				if explain {
					c.Config.Reason = reasonColumn(c.Name)
				}
			}
			if err := saveColumn(ctx, db, c, false); err != nil {
				return err
			}
			return format.Output(os.Stdout, resolveFormat(), []map[string]any{columnData(c, 0, 0)}, columnColumns[:4], flagQuiet)
		},
	}
	f := cmd.Flags()
	f.StringVar(&typ, "type", "", "column type: input, formula, enrich or ai (required)")
	f.StringSliceVar(&wf, "waterfall", nil, "enrich/ai: providers in order, comma-separated (ai default: llm; see crm provider list)")
	f.StringVar(&from, "from", "", "enrich: input column (default: the table key)")
	f.StringVar(&field, "field", "", "enrich: dotted path of the value to keep inside the answer (e.g. company, 0.url)")
	f.StringVar(&expr, "expr", "", "formula: template with {{column}} placeholders")
	f.StringVar(&prompt, "prompt", "", "ai: prompt file (.md) or inline text with {{column}} placeholders")
	f.StringVar(&output, "output", "", "ai: answer type: text (default), score:MIN-MAX, enum:a|b|c or bool")
	f.BoolVar(&explain, "explain", false, "ai: also store a 1–2 sentence reason in <name>_reason")
	return cmd
}

func columnListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list <table>",
		Short: "List the columns of a table with how many rows are filled",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, view, err := loadView(cmd, args[0], nil)
			if err != nil {
				return err
			}
			data := make([]map[string]any, len(view.Columns))
			for i, c := range view.Columns {
				filled := 0
				for _, cells := range view.Cells {
					if !value.Empty(cells[c.Name]) {
						filled++
					}
				}
				data[i] = columnData(c, filled, len(view.Cells))
			}
			return format.Output(os.Stdout, resolveFormat(), data, columnColumns, flagQuiet)
		},
	}
}

func columnRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <table> <name>",
		Short: "Remove a column (its values stay in the rows)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := openDB()
			if err != nil {
				return err
			}
			defer db.Close()
			ts := table.NewStore(db)
			ws, err := ts.FindWorkspace(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if err := ts.RemoveColumn(cmd.Context(), ws, args[1]); err != nil {
				return err
			}
			if !flagQuiet {
				fmt.Fprintf(os.Stderr, "Removed column %s from table %s\n", args[1], ws.Name)
			}
			return nil
		},
	}
}
