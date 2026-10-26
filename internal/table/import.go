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

// ImportOptions configures Import.
type ImportOptions struct {
	// Name is the table to create or append to.
	Name string
	// Key is the header used to upsert rows; empty uses the table's key (if any).
	Key string
	// Source records where the rows came from (absolute path or "-").
	Source string
}

// ImportResult summarizes an import.
type ImportResult struct {
	Workspace  *Workspace
	Rows       int
	Created    int
	Updated    int
	NewColumns []string
}

// Import loads a parsed CSV into a table, creating the table on first import.
// Rows are appended in file order; with a key, a row whose key already exists
// updates that row's cells instead (computed cells are kept). Headers that are
// not columns yet become input columns. Run it inside a transaction.
func (s *Store) Import(ctx context.Context, opts ImportOptions, t *csvio.Table) (*ImportResult, error) {
	for i, h := range t.Header {
		if h == "" {
			return nil, model.NewExitError(model.ErrValidation, "column %d has an empty header", i+1)
		}
		if j := slices.Index(t.Header[:i], h); j >= 0 {
			return nil, model.NewExitError(model.ErrValidation, "columns %d and %d have the same header %q", j+1, i+1, h)
		}
	}

	ws, err := s.FindWorkspace(ctx, opts.Name)
	switch {
	case errors.Is(err, model.ErrNotFound):
		var key *string
		if opts.Key != "" {
			key = &opts.Key
		}
		if ws, err = s.CreateWorkspace(ctx, opts.Name, key); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	case opts.Key != "" && (ws.KeyColumn == nil || *ws.KeyColumn != opts.Key):
		current := "none"
		if ws.KeyColumn != nil {
			current = *ws.KeyColumn
		}
		return nil, model.NewExitError(model.ErrValidation, "table %s already exists with key %s; --key %q does not match", ws.Name, current, opts.Key)
	}

	keyIdx := -1
	if ws.KeyColumn != nil {
		keyIdx = slices.Index(t.Header, *ws.KeyColumn)
		if keyIdx < 0 {
			return nil, model.NewExitError(model.ErrValidation, "key column %q is not in the CSV header", *ws.KeyColumn)
		}
		seen := make(map[string]int, len(t.Rows))
		for i, rec := range t.Rows {
			v := rec[keyIdx]
			if v == "" {
				return nil, model.NewExitError(model.ErrValidation, "row %d: key column %q is empty", i+1, *ws.KeyColumn)
			}
			if prev, dup := seen[v]; dup {
				return nil, model.NewExitError(model.ErrValidation, "rows %d and %d have the same key %q in column %q", prev, i+1, v, *ws.KeyColumn)
			}
			seen[v] = i + 1
		}
	}

	res := &ImportResult{Workspace: ws, Rows: len(t.Rows)}
	cols, err := s.Columns(ctx, ws.ID)
	if err != nil {
		return nil, err
	}
	for _, h := range t.Header {
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
	if keyIdx >= 0 {
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
	if opts.Source != "" {
		source = &opts.Source
	}
	for _, rec := range t.Rows {
		var key *string
		data := map[string]any{}
		if keyIdx >= 0 {
			k := rec[keyIdx]
			key = &k
			if r, ok := existing[k]; ok {
				data = r.Data
			}
		}
		for i, h := range t.Header {
			data[h] = rec[i]
		}
		enc, err := json.Marshal(data)
		if err != nil {
			return nil, fmt.Errorf("encode row: %w", err)
		}
		if key != nil {
			if r, ok := existing[*key]; ok {
				if _, err := s.db.ExecContext(ctx,
					"UPDATE workspace_rows SET data = ?, source = ?, updated_at = datetime('now') WHERE id = ?",
					string(enc), source, r.ID); err != nil {
					return nil, fmt.Errorf("update row %q: %w", *key, err)
				}
				res.Updated++
				continue
			}
		}
		position++
		if _, err := s.db.ExecContext(ctx,
			"INSERT INTO workspace_rows (workspace_id, position, key, data, source) VALUES (?, ?, ?, ?, ?)",
			ws.ID, position, key, string(enc), source); err != nil {
			return nil, fmt.Errorf("insert row %d: %w", position, err)
		}
		res.Created++
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE workspaces SET updated_at = datetime('now') WHERE id = ?", ws.ID); err != nil {
		return nil, fmt.Errorf("touch table %s: %w", ws.Name, err)
	}
	return res, nil
}
