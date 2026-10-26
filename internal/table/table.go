// Package table stores workspace tables: named sets of rows (kept in import
// order) with declared columns, plus the row filter used by show/export.
package table

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/google/uuid"
	"github.com/jdanielnd/crm-cli/internal/column"
	"github.com/jdanielnd/crm-cli/internal/db/repo"
	"github.com/jdanielnd/crm-cli/internal/model"
)

// Workspace is one table: a named set of rows with declared columns.
type Workspace struct {
	ID        int64   `json:"id"`
	UUID      string  `json:"uuid"`
	Name      string  `json:"name"`
	KeyColumn *string `json:"key_column"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
}

// Summary is a workspace with its row and column counts.
type Summary struct {
	Workspace
	Rows    int `json:"rows"`
	Columns int `json:"columns"`
}

// Row is one row of a workspace. Data holds input cells (strings) and the
// values written by runs (any JSON value).
type Row struct {
	ID          int64          `json:"id"`
	WorkspaceID int64          `json:"workspace_id"`
	Position    int            `json:"position"`
	Key         *string        `json:"key"`
	Data        map[string]any `json:"data"`
	State       string         `json:"state"`
	Source      *string        `json:"source"`
}

// Store reads and writes workspaces, rows and columns.
type Store struct {
	db repo.DBTX
}

// NewStore creates a Store on a *sql.DB or *sql.Tx.
func NewStore(db repo.DBTX) *Store { return &Store{db: db} }

var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// ValidateName checks a table name (letters, digits, '_', '.', '-').
func ValidateName(name string) error {
	if !validName.MatchString(name) {
		return model.NewExitError(model.ErrValidation, "invalid table name %q (use letters, digits, '_', '.', '-')", name)
	}
	return nil
}

const workspaceColumns = "id, uuid, name, key_column, created_at, updated_at"

func scanWorkspace(scan func(...any) error) (*Workspace, error) {
	var w Workspace
	if err := scan(&w.ID, &w.UUID, &w.Name, &w.KeyColumn, &w.CreatedAt, &w.UpdatedAt); err != nil {
		return nil, err
	}
	return &w, nil
}

// CreateWorkspace inserts a new, empty workspace.
func (s *Store) CreateWorkspace(ctx context.Context, name string, keyColumn *string) (*Workspace, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO workspaces (uuid, name, key_column) VALUES (?, ?, ?)", uuid.New().String(), name, keyColumn)
	if err != nil {
		return nil, fmt.Errorf("create table %s: %w", name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("last insert id: %w", err)
	}
	return s.workspaceByID(ctx, id)
}

func (s *Store) workspaceByID(ctx context.Context, id int64) (*Workspace, error) {
	w, err := scanWorkspace(s.db.QueryRowContext(ctx, "SELECT "+workspaceColumns+" FROM workspaces WHERE id = ?", id).Scan)
	if err != nil {
		return nil, fmt.Errorf("get table %d: %w", id, err)
	}
	return w, nil
}

// FindWorkspace returns the workspace named name (exit code 3 when missing).
func (s *Store) FindWorkspace(ctx context.Context, name string) (*Workspace, error) {
	w, err := scanWorkspace(s.db.QueryRowContext(ctx, "SELECT "+workspaceColumns+" FROM workspaces WHERE name = ?", name).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, model.NewExitError(model.ErrNotFound, "table %q not found (see crm table list)", name)
	}
	if err != nil {
		return nil, fmt.Errorf("get table %s: %w", name, err)
	}
	return w, nil
}

// ListWorkspaces returns every workspace with row and column counts.
func (s *Store) ListWorkspaces(ctx context.Context) ([]*Summary, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT w.id, w.uuid, w.name, w.key_column, w.created_at, w.updated_at,
		(SELECT COUNT(*) FROM workspace_rows r WHERE r.workspace_id = w.id),
		(SELECT COUNT(*) FROM workspace_columns c WHERE c.workspace_id = w.id AND c.archived = 0)
		FROM workspaces w ORDER BY w.name`)
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	defer rows.Close()
	var out []*Summary
	for rows.Next() {
		var sm Summary
		if err := rows.Scan(&sm.ID, &sm.UUID, &sm.Name, &sm.KeyColumn, &sm.CreatedAt, &sm.UpdatedAt, &sm.Rows, &sm.Columns); err != nil {
			return nil, fmt.Errorf("scan table: %w", err)
		}
		out = append(out, &sm)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tables: %w", err)
	}
	return out, nil
}

// Columns returns the active columns of a workspace in declaration order.
func (s *Store) Columns(ctx context.Context, workspaceID int64) ([]*column.Column, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, workspace_id, name, type, config, cost_est_micros, position, created_at
		 FROM workspace_columns WHERE workspace_id = ? AND archived = 0 ORDER BY position`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list columns: %w", err)
	}
	defer rows.Close()
	var out []*column.Column
	for rows.Next() {
		var c column.Column
		var cfg string
		if err := rows.Scan(&c.ID, &c.WorkspaceID, &c.Name, &c.Type, &cfg, &c.CostEst, &c.Position, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan column: %w", err)
		}
		if err := json.Unmarshal([]byte(cfg), &c.Config); err != nil {
			return nil, fmt.Errorf("column %s: decode config: %w", c.Name, err)
		}
		out = append(out, &c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate columns: %w", err)
	}
	return out, nil
}

// AddColumn validates c against the existing columns and appends it.
func (s *Store) AddColumn(ctx context.Context, c *column.Column) error {
	existing, err := s.Columns(ctx, c.WorkspaceID)
	if err != nil {
		return err
	}
	if err := c.Validate(existing); err != nil {
		return err
	}
	cfg, err := json.Marshal(c.Config)
	if err != nil {
		return fmt.Errorf("encode column config: %w", err)
	}
	c.Position = 1
	if n := len(existing); n > 0 {
		c.Position = existing[n-1].Position + 1
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO workspace_columns (workspace_id, name, type, config, cost_est_micros, position)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		c.WorkspaceID, c.Name, c.Type, string(cfg), c.CostEst, c.Position)
	if err != nil {
		return fmt.Errorf("add column %s: %w", c.Name, err)
	}
	c.ID, err = res.LastInsertId()
	if err != nil {
		return fmt.Errorf("last insert id: %w", err)
	}
	return nil
}

// RemoveColumn archives a column. Cell values stay in the rows. A column
// that another column reads (enrich --from, formula {{ref}}) cannot be removed.
func (s *Store) RemoveColumn(ctx context.Context, w *Workspace, name string) error {
	cols, err := s.Columns(ctx, w.ID)
	if err != nil {
		return err
	}
	var target *column.Column
	var others []*column.Column
	for _, c := range cols {
		if c.Name == name {
			target = c
		} else {
			others = append(others, c)
		}
	}
	if target == nil {
		return model.NewExitError(model.ErrNotFound, "table %s has no column %q", w.Name, name)
	}
	if w.KeyColumn != nil && *w.KeyColumn == name {
		return model.NewExitError(model.ErrConflict, "column %q is the key of table %s", name, w.Name)
	}
	for _, c := range others {
		if c.Type == column.Enrich && c.Config.From == name {
			return model.NewExitError(model.ErrConflict, "column %q is the --from of column %q", name, c.Name)
		}
		if c.Type == column.Formula {
			probe := column.Column{Name: c.Name, Type: column.Formula, Config: c.Config}
			if probe.Validate(withoutColumn(others, c.Name)) != nil {
				return model.NewExitError(model.ErrConflict, "column %q is used by formula %q", name, c.Name)
			}
		}
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE workspace_columns SET archived = 1 WHERE id = ?", target.ID); err != nil {
		return fmt.Errorf("remove column %s: %w", name, err)
	}
	return nil
}

func withoutColumn(cols []*column.Column, name string) []*column.Column {
	out := make([]*column.Column, 0, len(cols))
	for _, c := range cols {
		if c.Name != name {
			out = append(out, c)
		}
	}
	return out
}

const rowColumns = "id, workspace_id, position, key, data, state, source"

func scanRow(scan func(...any) error) (*Row, error) {
	var r Row
	var data string
	if err := scan(&r.ID, &r.WorkspaceID, &r.Position, &r.Key, &data, &r.State, &r.Source); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(data), &r.Data); err != nil {
		return nil, fmt.Errorf("row %d: decode data: %w", r.ID, err)
	}
	return &r, nil
}

// Rows returns every row of a workspace in position order.
func (s *Store) Rows(ctx context.Context, workspaceID int64) ([]*Row, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+rowColumns+" FROM workspace_rows WHERE workspace_id = ? ORDER BY position", workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list rows: %w", err)
	}
	defer rows.Close()
	var out []*Row
	for rows.Next() {
		r, err := scanRow(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rows: %w", err)
	}
	return out, nil
}

// SetCell writes one cell of a row (read-modify-write of the data object).
func (s *Store) SetCell(ctx context.Context, rowID int64, name string, v any) error {
	var data string
	if err := s.db.QueryRowContext(ctx, "SELECT data FROM workspace_rows WHERE id = ?", rowID).Scan(&data); err != nil {
		return fmt.Errorf("read row %d: %w", rowID, err)
	}
	cells := map[string]any{}
	if err := json.Unmarshal([]byte(data), &cells); err != nil {
		return fmt.Errorf("row %d: decode data: %w", rowID, err)
	}
	cells[name] = v
	enc, err := json.Marshal(cells)
	if err != nil {
		return fmt.Errorf("row %d: encode data: %w", rowID, err)
	}
	if _, err := s.db.ExecContext(ctx,
		"UPDATE workspace_rows SET data = ?, updated_at = datetime('now') WHERE id = ?", string(enc), rowID); err != nil {
		return fmt.Errorf("write row %d: %w", rowID, err)
	}
	return nil
}
