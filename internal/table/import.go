package table

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/jdanielnd/crm-cli/internal/column"
	"github.com/jdanielnd/crm-cli/internal/csvio"
	"github.com/jdanielnd/crm-cli/internal/model"
)

// ImportOptions configures Import and Upsert.
type ImportOptions struct {
	// Name is the table to create or append to.
	Name string
	// Key is the header used to upsert rows; empty uses the table's key (if any).
	Key string
	// Source records where the rows came from (absolute path, "-" or "mcp").
	Source string
}

// Cell is one named text cell of a row to upsert.
type Cell struct {
	Column string
	Value  string
}

// RowResult is what Upsert did with one input row: it created or updated the
// row at Position, or failed validation (Err) and was not written.
type RowResult struct {
	// Index is the 0-based position of the row in the input.
	Index    int
	RowID    int64
	Position int
	Key      *string
	Created  bool
	Err      error
}

// ImportResult summarizes an import or upsert.
type ImportResult struct {
	Workspace  *Workspace
	Rows       int
	Created    int
	Updated    int
	Failed     int
	NewColumns []string
	// Results has one entry per input row, in input order.
	Results []RowResult
}

// Import loads a parsed CSV into a table, creating the table on first import.
// Rows are appended in file order; with a key, a row whose key already exists
// updates that row's cells instead (computed cells are kept). Headers that are
// not columns yet become input columns. Any invalid row fails the whole
// import. Run it inside a transaction.
func (s *Store) Import(ctx context.Context, opts ImportOptions, t *csvio.Table) (*ImportResult, error) {
	for i, h := range t.Header {
		if h == "" {
			return nil, model.NewExitError(model.ErrValidation, "column %d has an empty header", i+1)
		}
		if j := slices.Index(t.Header[:i], h); j >= 0 {
			return nil, model.NewExitError(model.ErrValidation, "columns %d and %d have the same header %q", j+1, i+1, h)
		}
	}
	ws, err := s.openWorkspace(ctx, opts)
	if err != nil {
		return nil, err
	}
	if ws.KeyColumn != nil && !slices.Contains(t.Header, *ws.KeyColumn) {
		return nil, model.NewExitError(model.ErrValidation, "key column %q is not in the CSV header", *ws.KeyColumn)
	}
	rows := make([][]Cell, len(t.Rows))
	for i, rec := range t.Rows {
		cells := make([]Cell, len(t.Header))
		for j, h := range t.Header {
			cells[j] = Cell{Column: h, Value: rec[j]}
		}
		rows[i] = cells
	}
	res, err := s.upsert(ctx, ws, opts.Source, t.Header, rows)
	if err != nil {
		return nil, err
	}
	for _, r := range res.Results {
		if r.Err != nil {
			return nil, r.Err
		}
	}
	return res, nil
}

// Upsert writes rows of named cells into a table, creating it on first use.
// A row only sets the cells it names (others keep their value). columns
// gives the order of new input columns; names not listed there follow in
// order of first appearance. A row that fails validation (no or empty key
// cell, a key repeated in the batch, an empty or repeated column name) is
// reported in its RowResult and skipped; the other rows are written. Run it
// inside a transaction.
func (s *Store) Upsert(ctx context.Context, opts ImportOptions, columns []string, rows [][]Cell) (*ImportResult, error) {
	ws, err := s.openWorkspace(ctx, opts)
	if err != nil {
		return nil, err
	}
	return s.upsert(ctx, ws, opts.Source, columns, rows)
}

// openWorkspace finds the table opts.Name, or creates it with key opts.Key.
func (s *Store) openWorkspace(ctx context.Context, opts ImportOptions) (*Workspace, error) {
	ws, err := s.FindWorkspace(ctx, opts.Name)
	switch {
	case errors.Is(err, model.ErrNotFound):
		var key *string
		if opts.Key != "" {
			key = &opts.Key
		}
		return s.CreateWorkspace(ctx, opts.Name, key)
	case err != nil:
		return nil, err
	case opts.Key != "" && (ws.KeyColumn == nil || *ws.KeyColumn != opts.Key):
		current := "none"
		if ws.KeyColumn != nil {
			current = *ws.KeyColumn
		}
		return nil, model.NewExitError(model.ErrValidation, "table %s already exists with key %s; --key %q does not match", ws.Name, current, opts.Key)
	}
	return ws, nil
}

// checkRow validates the cells of input row i (0-based) and returns its key
// cell when the table has a key. seen maps keys already in the batch to
// their 1-based row number.
func checkRow(ws *Workspace, i int, cells []Cell, seen map[string]int) (*string, error) {
	var key *string
	for j, c := range cells {
		if c.Column == "" {
			return nil, model.NewExitError(model.ErrValidation, "row %d: cell %d has an empty column name", i+1, j+1)
		}
		if slices.ContainsFunc(cells[:j], func(o Cell) bool { return o.Column == c.Column }) {
			return nil, model.NewExitError(model.ErrValidation, "row %d: column %q is given twice", i+1, c.Column)
		}
		if ws.KeyColumn != nil && c.Column == *ws.KeyColumn {
			v := c.Value
			key = &v
		}
	}
	if ws.KeyColumn == nil {
		return nil, nil
	}
	switch {
	case key == nil:
		return nil, model.NewExitError(model.ErrValidation, "row %d: key column %q is missing", i+1, *ws.KeyColumn)
	case *key == "":
		return nil, model.NewExitError(model.ErrValidation, "row %d: key column %q is empty", i+1, *ws.KeyColumn)
	}
	if prev, dup := seen[*key]; dup {
		return nil, model.NewExitError(model.ErrValidation, "rows %d and %d have the same key %q in column %q", prev, i+1, *key, *ws.KeyColumn)
	}
	seen[*key] = i + 1
	return key, nil
}

func (s *Store) upsert(ctx context.Context, ws *Workspace, src string, columns []string, rows [][]Cell) (*ImportResult, error) {
	res := &ImportResult{Workspace: ws, Rows: len(rows), Results: make([]RowResult, len(rows))}
	order := slices.Clone(columns)
	seen := make(map[string]int, len(rows))
	for i, cells := range rows {
		rr := &res.Results[i]
		rr.Index = i
		rr.Key, rr.Err = checkRow(ws, i, cells, seen)
		if rr.Err != nil {
			res.Failed++
			continue
		}
		for _, c := range cells {
			if !slices.Contains(order, c.Column) {
				order = append(order, c.Column)
			}
		}
	}

	cols, err := s.Columns(ctx, ws.ID)
	if err != nil {
		return nil, err
	}
	for _, h := range order {
		if slices.ContainsFunc(cols, func(c *column.Column) bool { return c.Name == h }) {
			continue
		}
		c := &column.Column{WorkspaceID: ws.ID, Name: h, Type: column.Input}
		if err := s.AddColumn(ctx, c); err != nil {
			return nil, err
		}
		cols = append(cols, c)
		res.NewColumns = append(res.NewColumns, h)
	}

	existing := map[string]*Row{}
	if ws.KeyColumn != nil {
		rows, err := s.Rows(ctx, ws.ID)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			if r.Key != nil {
				existing[*r.Key] = r
			}
		}
	}
	var position int
	if err := s.db.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(position), 0) FROM workspace_rows WHERE workspace_id = ?", ws.ID).Scan(&position); err != nil {
		return nil, fmt.Errorf("read last row position: %w", err)
	}

	var source *string
	if src != "" {
		source = &src
	}
	for i, cells := range rows {
		rr := &res.Results[i]
		if rr.Err != nil {
			continue
		}
		data := map[string]any{}
		var prev *Row
		if rr.Key != nil {
			prev = existing[*rr.Key]
		}
		if prev != nil {
			data = prev.Data
		}
		for _, c := range cells {
			data[c.Column] = c.Value
		}
		enc, err := json.Marshal(data)
		if err != nil {
			return nil, fmt.Errorf("encode row: %w", err)
		}
		if prev != nil {
			if _, err := s.db.ExecContext(ctx,
				"UPDATE workspace_rows SET data = ?, source = ?, updated_at = datetime('now') WHERE id = ?",
				string(enc), source, prev.ID); err != nil {
				return nil, fmt.Errorf("update row %q: %w", *rr.Key, err)
			}
			rr.RowID, rr.Position = prev.ID, prev.Position
			res.Updated++
			continue
		}
		position++
		r, err := s.db.ExecContext(ctx,
			"INSERT INTO workspace_rows (workspace_id, position, key, data, source) VALUES (?, ?, ?, ?, ?)",
			ws.ID, position, rr.Key, string(enc), source)
		if err != nil {
			return nil, fmt.Errorf("insert row %d: %w", position, err)
		}
		if rr.RowID, err = r.LastInsertId(); err != nil {
			return nil, fmt.Errorf("last insert id: %w", err)
		}
		rr.Position, rr.Created = position, true
		res.Created++
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE workspaces SET updated_at = datetime('now') WHERE id = ?", ws.ID); err != nil {
		return nil, fmt.Errorf("touch table %s: %w", ws.Name, err)
	}
	return res, nil
}
