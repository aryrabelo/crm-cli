package service

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"

	"github.com/jdanielnd/crm-cli/internal/csvio"
	"github.com/jdanielnd/crm-cli/internal/db/repo"
	"github.com/jdanielnd/crm-cli/internal/model"
	"github.com/jdanielnd/crm-cli/internal/table"
)

// ImportCSV imports a parsed CSV into a table in one transaction: every row
// is written, or none (see table.Store.Import).
func (s *Service) ImportCSV(ctx context.Context, opts table.ImportOptions, t *csvio.Table) (*table.ImportResult, error) {
	if err := table.ValidateName(opts.Name); err != nil {
		return nil, err
	}
	var res *table.ImportResult
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		res, err = table.NewStore(tx).Import(ctx, opts, t)
		return err
	})
	return res, err
}

// UpsertRows writes rows of named cells into a table in one transaction.
// Rows that fail validation are reported in their RowResult and skipped;
// the others are written. With mustExist a missing table is an error
// instead of being created.
func (s *Service) UpsertRows(ctx context.Context, opts table.ImportOptions, columns []string, rows [][]table.Cell, mustExist bool) (*table.ImportResult, error) {
	if err := table.ValidateName(opts.Name); err != nil {
		return nil, err
	}
	var res *table.ImportResult
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		ts := table.NewStore(tx)
		if mustExist {
			if _, err := ts.FindWorkspace(ctx, opts.Name); err != nil {
				return err
			}
		}
		var err error
		res, err = ts.Upsert(ctx, opts, columns, rows)
		return err
	})
	return res, err
}

// View returns the rows of table name that match every only filter, with
// formula columns evaluated.
func (s *Service) View(ctx context.Context, name string, only []string) (*table.Workspace, *table.View, error) {
	return loadView(ctx, s.db, name, only)
}

func loadView(ctx context.Context, db repo.DBTX, name string, only []string) (*table.Workspace, *table.View, error) {
	ts := table.NewStore(db)
	ws, err := ts.FindWorkspace(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	cols, err := ts.Columns(ctx, ws.ID)
	if err != nil {
		return nil, nil, err
	}
	filters, err := table.ParseFilters(only, cols)
	if err != nil {
		return nil, nil, err
	}
	rows, err := ts.Rows(ctx, ws.ID)
	if err != nil {
		return nil, nil, err
	}
	return ws, table.NewView(cols, rows, filters), nil
}

// DefaultQueryLimit is the page size of Query when none is given.
const DefaultQueryLimit = 100

// QueryOptions selects rows and columns of a table.
type QueryOptions struct {
	Table string
	// Only holds filter expressions (AND), as in table show --only.
	Only []string
	// Columns picks and orders the returned columns (default: all).
	Columns []string
	// Limit caps the returned rows (default DefaultQueryLimit); Offset skips
	// matching rows first.
	Limit  int
	Offset int
}

// QueryResult is one page of matching rows. Total counts every matching
// row; each row object holds the selected cells plus "_row", its position.
type QueryResult struct {
	Table   string           `json:"table"`
	Total   int              `json:"total"`
	Offset  int              `json:"offset"`
	Columns []string         `json:"columns"`
	Rows    []map[string]any `json:"rows"`
}

// Query returns a page of the rows of a table matching the filters.
func (s *Service) Query(ctx context.Context, opts QueryOptions) (*QueryResult, error) {
	if opts.Limit < 0 || opts.Offset < 0 {
		return nil, model.NewExitError(model.ErrValidation, "limit and offset must be >= 0")
	}
	if opts.Limit == 0 {
		opts.Limit = DefaultQueryLimit
	}
	ws, view, err := s.View(ctx, opts.Table, opts.Only)
	if err != nil {
		return nil, err
	}
	if view, err = view.Project(opts.Columns); err != nil {
		return nil, err
	}
	res := &QueryResult{Table: ws.Name, Total: len(view.Rows), Offset: opts.Offset, Columns: view.Header(), Rows: []map[string]any{}}
	end := min(opts.Offset+opts.Limit, len(view.Rows))
	for i := opts.Offset; i < end; i++ {
		m := make(map[string]any, len(view.Columns)+1)
		m["_row"] = view.Rows[i].Position
		for _, c := range view.Columns {
			m[c.Name] = view.Cells[i][c.Name]
		}
		res.Rows = append(res.Rows, m)
	}
	return res, nil
}

// Export writes the view as csv (default), tsv or json. Computed values that
// are objects or arrays are compact JSON in csv/tsv; json keeps them as JSON
// values, with object keys in column order.
func Export(w io.Writer, view *table.View, format string) error {
	switch format {
	case "", "csv":
		return csvio.Write(w, ',', view.Header(), view.Records(), csvio.DefaultDialect)
	case "tsv":
		return csvio.Write(w, '\t', view.Header(), view.Records(), csvio.DefaultDialect)
	case "json":
		return writeOrderedJSON(w, view)
	default:
		return model.NewExitError(model.ErrValidation, "export supports csv, tsv or json, not %q", format)
	}
}

// writeOrderedJSON writes the rows as a JSON array of objects whose keys
// follow the column order.
func writeOrderedJSON(w io.Writer, view *table.View) error {
	var buf bytes.Buffer
	buf.WriteString("[")
	for i, cells := range view.Cells {
		if i > 0 {
			buf.WriteString(",")
		}
		buf.WriteString("\n  {")
		for j, c := range view.Columns {
			if j > 0 {
				buf.WriteString(", ")
			}
			k, err := json.Marshal(c.Name)
			if err != nil {
				return err
			}
			v, err := json.Marshal(cells[c.Name])
			if err != nil {
				return fmt.Errorf("encode %s: %w", c.Name, err)
			}
			buf.Write(k)
			buf.WriteString(": ")
			buf.Write(v)
		}
		buf.WriteString("}")
	}
	if len(view.Cells) > 0 {
		buf.WriteString("\n")
	}
	buf.WriteString("]\n")
	_, err := w.Write(buf.Bytes())
	return err
}
