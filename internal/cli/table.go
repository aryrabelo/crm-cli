package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/jdanielnd/crm-cli/internal/csvio"
	"github.com/jdanielnd/crm-cli/internal/format"
	"github.com/jdanielnd/crm-cli/internal/model"
	"github.com/jdanielnd/crm-cli/internal/service"
	"github.com/jdanielnd/crm-cli/internal/table"
	"github.com/spf13/cobra"
)

// readCSVSource reads a CSV argument: a file path or "-" for stdin. source is
// the absolute path (or "-") recorded as provenance.
func readCSVSource(cmd *cobra.Command, arg string) (data []byte, source string, err error) {
	source = arg
	if arg == "-" {
		data, err = io.ReadAll(cmd.InOrStdin())
	} else {
		data, err = os.ReadFile(arg)
		if abs, absErr := filepath.Abs(arg); absErr == nil {
			source = abs
		}
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil, "", model.NewExitError(model.ErrNotFound, "file not found: %s", arg)
	}
	if err != nil {
		return nil, "", fmt.Errorf("read %s: %w", arg, err)
	}
	return data, source, nil
}

func registerTableCommands(rootCmd *cobra.Command) {
	cmd := &cobra.Command{
		Use:   "table",
		Short: "Workspace tables: import rows, list, show and export",
		Long: `Workspace tables (data plane): named sets of rows with declared columns.

Import a CSV as a table, add computed columns (crm column add), fill them in
batch (crm run), then show or export the result. Rows keep their import order.`,
	}
	cmd.AddCommand(tableImportCmd(), tableListCmd(), tableShowCmd(), tableExportCmd())
	rootCmd.AddCommand(cmd)
}

var tableImportColumns = []format.ColumnDef{
	{Header: "Table", Field: "table"},
	{Header: "Rows", Field: "rows"},
	{Header: "Created", Field: "created"},
	{Header: "Updated", Field: "updated"},
	{Header: "New Columns", Field: "new_columns"},
}

func tableImportCmd() *cobra.Command {
	var as, key string
	cmd := &cobra.Command{
		Use:   "import <file.csv|->",
		Short: "Import a CSV into a table (created on first import)",
		Long: `Import a CSV file (or stdin with "-") into the table named by --as.

Every header becomes an input column; rows are appended in file order. With
--key, a row whose key already exists updates that row instead of adding a
duplicate (values computed by runs are kept). The key is fixed by the first
import of a table.`,
		Example: `  crm table import leads.csv --as leads --key email
  cat more.csv | crm table import - --as leads`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if as == "" {
				return model.NewExitError(model.ErrValidation, "--as <table> is required")
			}
			if err := table.ValidateName(as); err != nil {
				return err
			}
			data, source, err := readCSVSource(cmd, args[0])
			if err != nil {
				return err
			}
			parsed, err := csvio.Parse(data)
			if err != nil {
				return model.NewExitError(model.ErrValidation, "parse %s: %s", args[0], err)
			}

			db, err := openDB()
			if err != nil {
				return err
			}
			defer db.Close()
			res, err := newService(db).ImportCSV(cmd.Context(), table.ImportOptions{Name: as, Key: key, Source: source}, parsed)
			if err != nil {
				return err
			}
			out := map[string]any{
				"id":          res.Workspace.ID,
				"table":       res.Workspace.Name,
				"rows":        res.Rows,
				"created":     res.Created,
				"updated":     res.Updated,
				"new_columns": len(res.NewColumns),
			}
			return format.Output(os.Stdout, resolveFormat(), []map[string]any{out}, tableImportColumns, flagQuiet)
		},
	}
	cmd.Flags().StringVar(&as, "as", "", "table name (required)")
	cmd.Flags().StringVar(&key, "key", "", "column used to upsert rows (no duplicates on re-import)")
	return cmd
}

var tableListColumns = []format.ColumnDef{
	{Header: "ID", Field: "id"},
	{Header: "Table", Field: "name"},
	{Header: "Rows", Field: "rows"},
	{Header: "Columns", Field: "columns"},
	{Header: "Key", Field: "key_column"},
	{Header: "Updated", Field: "updated_at"},
}

func tableListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List tables",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			db, err := openDB()
			if err != nil {
				return err
			}
			defer db.Close()
			list, err := table.NewStore(db).ListWorkspaces(cmd.Context())
			if err != nil {
				return err
			}
			data := make([]map[string]any, len(list))
			for i, w := range list {
				data[i] = map[string]any{
					"id": w.ID, "name": w.Name, "rows": w.Rows, "columns": w.Columns,
					"key_column": w.KeyColumn, "created_at": w.CreatedAt, "updated_at": w.UpdatedAt,
				}
				if w.KeyColumn != nil {
					data[i]["key_column"] = *w.KeyColumn
				}
			}
			return format.Output(os.Stdout, resolveFormat(), data, tableListColumns, flagQuiet)
		},
	}
}

// loadView opens a table and returns its filtered, materialized rows.
func loadView(cmd *cobra.Command, name string, only []string) (*table.Workspace, *table.View, error) {
	db, err := openDB()
	if err != nil {
		return nil, nil, err
	}
	defer db.Close()
	return newService(db).View(cmd.Context(), name, only)
}

const onlyHelp = `keep rows matching <column><op><value>; op is = != > >= < <= ~ (regex) !~; repeatable (AND). "col!=" keeps non-empty cells`

func tableShowCmd() *cobra.Command {
	var only []string
	cmd := &cobra.Command{
		Use:   "show <table>",
		Short: "Show the rows of a table",
		Long: `Show the rows of a table (formula columns evaluated). Table output for a
terminal, JSON when piped; -f csv|tsv|json to choose.`,
		Example: `  crm table show leads
  crm table show leads --only "fit>=70" --only "email~@acme\.com$"`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ws, view, err := loadView(cmd, args[0], only)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "table %s: %d rows shown, %d columns\n", ws.Name, len(view.Rows), len(view.Columns))
			header := view.Header()
			defs := make([]format.ColumnDef, len(header))
			for i, h := range header {
				defs[i] = format.ColumnDef{Header: h, Field: h}
			}
			f := resolveFormat()
			records := view.Records()
			data := make([]map[string]any, len(view.Cells))
			for i := range view.Cells {
				m := make(map[string]any, len(header))
				for j, h := range header {
					if f == format.FormatJSON {
						m[h] = view.Cells[i][h]
					} else {
						m[h] = records[i][j]
					}
				}
				data[i] = m
			}
			return format.Output(os.Stdout, f, data, defs, false)
		},
	}
	cmd.Flags().StringArrayVar(&only, "only", nil, onlyHelp)
	return cmd
}

func tableExportCmd() *cobra.Command {
	var only []string
	cmd := &cobra.Command{
		Use:   "export <table>",
		Short: "Export a table as CSV (default), TSV or JSON",
		Long: `Export every column of a table in row order. CSV is the default (also when
writing to a terminal). Computed values that are objects or arrays are written
as compact JSON in CSV/TSV; -f json keeps them as JSON values, with object keys
in column order.`,
		Example: `  crm table export leads > leads-enriched.csv
  crm table export leads -f json --only "fit>=70"`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, view, err := loadView(cmd, args[0], only)
			if err != nil {
				return err
			}
			return service.Export(os.Stdout, view, flagFormat)
		},
	}
	cmd.Flags().StringArrayVar(&only, "only", nil, onlyHelp)
	return cmd
}
