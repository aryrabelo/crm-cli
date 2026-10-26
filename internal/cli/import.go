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
	"github.com/jdanielnd/crm-cli/internal/importer"
	"github.com/jdanielnd/crm-cli/internal/model"
	"github.com/spf13/cobra"
)

var importSummaryColumns = []format.ColumnDef{
	{Header: "Batch", Field: "batch_id"},
	{Header: "Entity", Field: "entity"},
	{Header: "Rows", Field: "rows"},
	{Header: "Created", Field: "created"},
	{Header: "Updated", Field: "updated"},
}

var importActionColumns = []format.ColumnDef{
	{Header: "Row", Field: "row"},
	{Header: "Action", Field: "action"},
	{Header: "ID", Field: "id"},
	{Header: "Key", Field: "key"},
}

// parseImportEntity accepts the entity names used by `crm import --as` and `crm rows`.
func parseImportEntity(s string) (string, error) {
	switch s {
	case "":
		return "", model.NewExitError(model.ErrValidation, "entity is required (use person or org)")
	case "person", "people":
		return "person", nil
	case "org", "orgs", "organization", "organizations":
		return "organization", nil
	default:
		return "", model.NewExitError(model.ErrValidation, "unknown entity %q (use person or org)", s)
	}
}

func registerImportCommand(rootCmd *cobra.Command) {
	rootCmd.AddCommand(importCmd())
}

func importCmd() *cobra.Command {
	var as, key string
	var maps []string
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "import <file.csv|->",
		Short: "Import a CSV as people or organizations",
		Long: `Import a CSV file (or stdin with "-") as people or organizations.

Columns with obvious headers (name, email, phone, company, domain, ...) map to
core fields; every other column is stored as a custom field keyed by its header.
Override with --map csvcol=field (an empty field keeps the column as a custom
field). With --key, rows whose key matches an existing record update it instead
of creating a duplicate. Each run is recorded as an import batch; export it in
the original column and row order with: crm rows <entity> --import <batch-id>`,
		Example: `  crm import leads.csv --as person --key email
  crm import companies.csv --as org --map "Razão Social=name,Site=domain" --dry-run
  cat leads.csv | crm import - --as person`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			entityType, err := parseImportEntity(as)
			if err != nil {
				return err
			}

			source := args[0]
			var data []byte
			if source == "-" {
				data, err = io.ReadAll(cmd.InOrStdin())
			} else {
				data, err = os.ReadFile(source)
				if abs, absErr := filepath.Abs(source); absErr == nil {
					source = abs
				}
			}
			if errors.Is(err, fs.ErrNotExist) {
				return model.NewExitError(model.ErrNotFound, "file not found: %s", args[0])
			}
			if err != nil {
				return fmt.Errorf("read %s: %w", args[0], err)
			}

			table, err := csvio.Parse(data)
			if err != nil {
				return model.NewExitError(model.ErrValidation, "parse %s: %s", args[0], err)
			}

			db, err := openDB()
			if err != nil {
				return err
			}
			defer db.Close()

			tx, err := db.BeginTx(cmd.Context(), nil)
			if err != nil {
				return fmt.Errorf("begin import: %w", err)
			}
			defer func() { _ = tx.Rollback() }()

			res, err := importer.Run(cmd.Context(), tx, importer.Options{
				EntityType: entityType,
				Source:     source,
				Maps:       maps,
				Key:        key,
			}, table)
			if err != nil {
				return err
			}

			if dryRun {
				if err := tx.Rollback(); err != nil {
					return fmt.Errorf("discard dry run: %w", err)
				}
				fmt.Fprintf(os.Stderr, "dry run: %d rows → %d create, %d update; nothing written\n",
					len(res.Actions), res.Created, res.Updated)
				data := make([]map[string]any, len(res.Actions))
				for i, a := range res.Actions {
					m := map[string]any{"row": a.Row, "action": a.Action}
					if a.Action == "update" {
						m["id"] = a.EntityID
					}
					if a.Key != "" {
						m["key"] = a.Key
					}
					data[i] = m
				}
				return format.Output(os.Stdout, resolveFormat(), data, importActionColumns, flagQuiet)
			}

			if err := tx.Commit(); err != nil {
				return fmt.Errorf("commit import: %w", err)
			}
			summary := map[string]any{
				"id":       res.Batch.ID,
				"batch_id": res.Batch.ID,
				"entity":   entityType,
				"rows":     len(res.Actions),
				"created":  res.Created,
				"updated":  res.Updated,
			}
			return format.Output(os.Stdout, resolveFormat(), []map[string]any{summary}, importSummaryColumns, flagQuiet)
		},
	}

	cmd.Flags().StringVar(&as, "as", "", "entity to import as: person or org (required)")
	cmd.Flags().StringSliceVar(&maps, "map", nil, "column mapping csvcol=field, comma-separated or repeated")
	cmd.Flags().StringVar(&key, "key", "", "column header or field used to upsert (no duplicates on re-import)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would be created/updated without writing")

	return cmd
}
