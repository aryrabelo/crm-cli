package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/jdanielnd/crm-cli/internal/column"
	"github.com/jdanielnd/crm-cli/internal/format"
	"github.com/jdanielnd/crm-cli/internal/provider"
	"github.com/jdanielnd/crm-cli/internal/table"
	"github.com/jdanielnd/crm-cli/internal/value"
	"github.com/jdanielnd/crm-cli/internal/waterfall"
	"github.com/spf13/cobra"
)

func registerColumnCommands(rootCmd *cobra.Command) {
	cmd := &cobra.Command{
		Use:   "column",
		Short: "Declare table columns: input, formula, enrich",
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
	default:
		return ""
	}
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
	var typ, from, field, expr string
	var wf []string
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
           value inside the answer (an empty field falls through too)`,
		Example: `  crm column add leads profile --type enrich --waterfall github --from login
  crm column add leads company --type enrich --waterfall github,exa --from login --field company
  crm column add leads label --type formula --expr "{{name}} ({{company}})"`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := column.ParseType(typ)
			if err != nil {
				return err
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
			c := &column.Column{
				WorkspaceID: ws.ID,
				Name:        args[1],
				Type:        t,
				Config:      column.Config{Expr: expr, Waterfall: wf, From: from, Field: field},
			}
			if t == column.Enrich {
				if c.Config.From == "" && ws.KeyColumn != nil {
					c.Config.From = *ws.KeyColumn
				}
				_, configs, err := c.Waterfall(ctx, provider.NewStore(db), newRegistry(), waterfall.NewCache(db))
				if err != nil {
					return err
				}
				c.CostEst = column.WorstCost(configs)
			}
			if err := ts.AddColumn(ctx, c); err != nil {
				return err
			}
			return format.Output(os.Stdout, resolveFormat(), []map[string]any{columnData(c, 0, 0)}, columnColumns[:4], flagQuiet)
		},
	}
	cmd.Flags().StringVar(&typ, "type", "", "column type: input, formula or enrich (required)")
	cmd.Flags().StringSliceVar(&wf, "waterfall", nil, "enrich: providers in order, comma-separated (see crm provider list)")
	cmd.Flags().StringVar(&from, "from", "", "enrich: input column (default: the table key)")
	cmd.Flags().StringVar(&field, "field", "", "enrich: dotted path of the value to keep inside the answer (e.g. company, 0.url)")
	cmd.Flags().StringVar(&expr, "expr", "", "formula: template with {{column}} placeholders")
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
