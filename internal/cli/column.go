package cli

import (
	"fmt"
	"os"

	"github.com/jdanielnd/crm-cli/internal/format"
	"github.com/jdanielnd/crm-cli/internal/service"
	"github.com/jdanielnd/crm-cli/internal/table"
	"github.com/jdanielnd/crm-cli/internal/value"
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
			db, err := openDB()
			if err != nil {
				return err
			}
			defer db.Close()
			spec := service.ColumnSpec{Name: args[1], Type: typ, Waterfall: wf, From: from, Field: field, Expr: expr,
				Output: output, Explain: explain}
			if prompt != "" {
				if spec.Prompt, err = readPrompt(prompt); err != nil {
					return err
				}
			}
			c, err := newService(db).AddColumn(cmd.Context(), args[0], spec)
			if err != nil {
				return err
			}
			return format.Output(os.Stdout, resolveFormat(), []map[string]any{service.ColumnData(c, 0, 0)}, columnColumns[:4], flagQuiet)
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
				data[i] = service.ColumnData(c, filled, len(view.Cells))
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
