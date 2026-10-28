package cli

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/jdanielnd/crm-cli/internal/csvio"
	"github.com/jdanielnd/crm-cli/internal/db/repo"
	"github.com/jdanielnd/crm-cli/internal/format"
	"github.com/jdanielnd/crm-cli/internal/importer"
	"github.com/jdanielnd/crm-cli/internal/model"
	"github.com/spf13/cobra"
)

func registerRowsCommand(rootCmd *cobra.Command) {
	rootCmd.AddCommand(rowsCmd())
}

func rowsCmd() *cobra.Command {
	var batchID int64
	var only []string

	cmd := &cobra.Command{
		Use:   "rows <person|org>",
		Short: "Export people or organizations as rows (core + custom fields)",
		Long: `Export people or organizations as rows with core and custom fields.

Without --import: one row per record (ordered by ID) with an "id" column, the
core fields, then every custom field key. With --import <batch-id>: exactly the
columns and row order of that import batch; -f csv reproduces the original
file (header text, BOM, line endings and trailing newline included) as long
as the imported records are unchanged. The cells are the ones recorded when the
batch was imported: a later import, edit or archive does not change them.
Batches imported by an older crm have no recorded cells and are read from the
CURRENT records instead (archived records omitted).`,
		Example: `  crm rows person -f csv
  crm rows person --import 3 -f csv > roundtrip.csv
  crm rows org --only industry=SaaS -f json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			entityType, err := parseImportEntity(args[0])
			if err != nil {
				return err
			}

			db, err := openDB()
			if err != nil {
				return err
			}
			defer db.Close()

			ir := repo.NewImportRepo(db)
			var cols []model.ImportColumn
			var rows []*model.EntityRow
			var snapshot []*model.BatchRecord
			dialect := csvio.DefaultDialect
			withID := !cmd.Flags().Changed("import")

			if withID {
				var keys []string
				rows, keys, err = ir.ListEntityRows(cmd.Context(), entityType)
				if err != nil {
					return err
				}
				cols = importer.ExportColumns(entityType, keys)
			} else {
				batch, err := ir.FindBatch(cmd.Context(), batchID)
				if err != nil {
					return err
				}
				if batch.EntityType != entityType {
					return model.NewExitError(model.ErrValidation, "import batch %d holds %s records, not %s", batchID, batch.EntityType, entityType)
				}
				if snapshot, err = ir.BatchRecords(cmd.Context(), batch); err != nil {
					return err
				}
				if len(snapshot) > 0 && snapshot[0].Cells == nil {
					// imported before cells were recorded: current records
					snapshot = nil
					if rows, err = ir.BatchRows(cmd.Context(), batch); err != nil {
						return err
					}
				}
				cols = batch.Columns
				dialect = csvio.Dialect{BOM: batch.HasBOM, CRLF: batch.CRLF, TrailingNewline: batch.TrailingNewline}
			}

			header := make([]string, 0, len(cols)+1)
			if withID {
				header = append(header, "id")
			}
			for _, c := range cols {
				header = append(header, c.Header)
			}

			type filter struct {
				col   int
				value string
			}
			filters := make([]filter, 0, len(only))
			for _, o := range only {
				k, v, ok := strings.Cut(o, "=")
				if !ok {
					return model.NewExitError(model.ErrValidation, "invalid --only %q: expected column=value", o)
				}
				i := slices.Index(header, k)
				if i < 0 {
					return model.NewExitError(model.ErrValidation, "--only %q: no column named %q", o, k)
				}
				filters = append(filters, filter{col: i, value: v})
			}

			var ids []int64
			var records [][]string
			n := len(rows)
			if snapshot != nil {
				n = len(snapshot)
			}
		rowLoop:
			for i := range n {
				var rowID int64
				rec := make([]string, 0, len(header))
				switch {
				case snapshot != nil:
					rowID = snapshot[i].EntityID
					rec = append(rec, snapshot[i].Cells...)
				default:
					rowID = rows[i].ID
					if withID {
						rec = append(rec, strconv.FormatInt(rowID, 10))
					}
					for _, c := range cols {
						rec = append(rec, importer.Cell(entityType, c, rows[i]))
					}
				}
				for _, f := range filters {
					if rec[f.col] != f.value {
						continue rowLoop
					}
				}
				ids = append(ids, rowID)
				records = append(records, rec)
			}

			if flagQuiet {
				for _, id := range ids {
					fmt.Fprintln(os.Stdout, id)
				}
				return nil
			}

			switch f := resolveFormat(); f {
			case format.FormatCSV:
				return csvio.Write(os.Stdout, ',', header, records, dialect)
			case format.FormatTSV:
				return csvio.Write(os.Stdout, '\t', header, records, dialect)
			default:
				data := make([]map[string]any, len(records))
				for i, rec := range records {
					m := make(map[string]any, len(header))
					for j, h := range header {
						m[h] = rec[j]
					}
					if withID {
						m["id"] = ids[i]
					}
					data[i] = m
				}
				defs := make([]format.ColumnDef, len(header))
				for i, h := range header {
					defs[i] = format.ColumnDef{Header: h, Field: h}
				}
				return format.Output(os.Stdout, f, data, defs, false)
			}
		},
	}

	cmd.Flags().Int64Var(&batchID, "import", 0, "export the rows of this import batch in original column/row order")
	cmd.Flags().StringArrayVar(&only, "only", nil, "keep rows where column equals value (column=value, repeatable)")

	return cmd
}
