package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jdanielnd/crm-cli/internal/model"
)

// ImportRepo persists import batches and the entity rows they write.
// It works on raw core columns (model.CoreColumns) so callers control the
// exact stored values; it does not apply the interactive duplicate checks of
// PersonRepo.Create — imports dedupe through an explicit key instead.
type ImportRepo struct {
	db DBTX
}

// NewImportRepo creates a new ImportRepo.
func NewImportRepo(db DBTX) *ImportRepo {
	return &ImportRepo{db: db}
}

func importTable(entityType string) (string, error) {
	if !slices.Contains(model.ImportableEntityTypes, entityType) {
		return "", fmt.Errorf("entity type %q cannot be imported (use person or organization): %w", entityType, model.ErrValidation)
	}
	return entityTables[entityType], nil
}

func checkCoreColumns(entityType string, core map[string]*string) error {
	for col := range core {
		if !slices.Contains(model.CoreColumns[entityType], col) {
			return fmt.Errorf("unknown %s column %q: %w", entityType, col, model.ErrValidation)
		}
	}
	return nil
}

// CreateBatch inserts an import batch and sets its ID and UUID.
func (r *ImportRepo) CreateBatch(ctx context.Context, b *model.ImportBatch) error {
	if _, err := importTable(b.EntityType); err != nil {
		return err
	}
	cols, err := json.Marshal(b.Columns)
	if err != nil {
		return fmt.Errorf("encode import columns: %w", err)
	}
	lineEnding := "lf"
	if b.CRLF {
		lineEnding = "crlf"
	}
	b.UUID = uuid.New().String()
	result, err := r.db.ExecContext(ctx,
		`INSERT INTO import_batches (uuid, entity_type, source, columns, key_column, has_bom, line_ending, trailing_newline, row_count)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		b.UUID, b.EntityType, b.Source, string(cols), b.KeyColumn, b.HasBOM, lineEnding, b.TrailingNewline, b.RowCount)
	if err != nil {
		return fmt.Errorf("insert import batch: %w", err)
	}
	b.ID, err = result.LastInsertId()
	if err != nil {
		return fmt.Errorf("last insert id: %w", err)
	}
	return nil
}

// AddRow records that the data row at position (1-based) landed on entityID,
// with the CSV cells it was imported from (one per batch column).
func (r *ImportRepo) AddRow(ctx context.Context, batchID int64, position int, entityID int64, cells []string) error {
	enc, err := json.Marshal(cells)
	if err != nil {
		return fmt.Errorf("encode import row %d: %w", position, err)
	}
	_, err = r.db.ExecContext(ctx,
		"INSERT INTO import_rows (batch_id, position, entity_id, cells) VALUES (?, ?, ?, ?)",
		batchID, position, entityID, string(enc))
	if err != nil {
		return fmt.Errorf("insert import row %d: %w", position, err)
	}
	return nil
}

// FindBatch returns an import batch by ID.
func (r *ImportRepo) FindBatch(ctx context.Context, id int64) (*model.ImportBatch, error) {
	var b model.ImportBatch
	var cols, lineEnding string
	err := r.db.QueryRowContext(ctx,
		`SELECT id, uuid, entity_type, source, columns, key_column, has_bom, line_ending, trailing_newline, row_count, created_at
		 FROM import_batches WHERE id = ?`, id).Scan(
		&b.ID, &b.UUID, &b.EntityType, &b.Source, &cols, &b.KeyColumn,
		&b.HasBOM, &lineEnding, &b.TrailingNewline, &b.RowCount, &b.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("import batch %d: %w", id, model.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("find import batch %d: %w", id, err)
	}
	if err := json.Unmarshal([]byte(cols), &b.Columns); err != nil {
		return nil, fmt.Errorf("decode import batch %d columns: %w", id, err)
	}
	b.CRLF = lineEnding == "crlf"
	return &b, nil
}

// CreateEntity inserts a person or organization from raw core column values.
// The NOT NULL name column (first_name / name) is stored as "" when nil.
func (r *ImportRepo) CreateEntity(ctx context.Context, entityType string, core map[string]*string) (int64, error) {
	table, err := importTable(entityType)
	if err != nil {
		return 0, err
	}
	if err := checkCoreColumns(entityType, core); err != nil {
		return 0, err
	}
	cols := []string{"uuid"}
	args := []any{uuid.New().String()}
	for _, col := range model.CoreColumns[entityType] {
		v, ok := core[col]
		required := col == "first_name" || (entityType == "organization" && col == "name")
		if !ok && !required {
			continue
		}
		cols = append(cols, col)
		if v == nil && required {
			args = append(args, "")
		} else {
			args = append(args, v)
		}
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(cols)), ", ")
	result, err := r.db.ExecContext(ctx,
		fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", table, strings.Join(cols, ", "), placeholders), args...)
	if err != nil {
		return 0, fmt.Errorf("insert %s: %w", entityType, err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("last insert id: %w", err)
	}
	return id, nil
}

// UpdateEntity overwrites the given core columns of a non-archived entity.
func (r *ImportRepo) UpdateEntity(ctx context.Context, entityType string, id int64, core map[string]*string) error {
	table, err := importTable(entityType)
	if err != nil {
		return err
	}
	if err := checkCoreColumns(entityType, core); err != nil {
		return err
	}
	var set []string
	var args []any
	for _, col := range model.CoreColumns[entityType] {
		v, ok := core[col]
		if !ok {
			continue
		}
		set = append(set, col+" = ?")
		if v == nil && (col == "first_name" || (entityType == "organization" && col == "name")) {
			args = append(args, "")
		} else {
			args = append(args, v)
		}
	}
	set = append(set, "updated_at = datetime('now')")
	args = append(args, id)
	result, err := r.db.ExecContext(ctx,
		fmt.Sprintf("UPDATE %s SET %s WHERE id = ? AND archived = 0", table, strings.Join(set, ", ")), args...)
	if err != nil {
		return fmt.Errorf("update %s %d: %w", entityType, id, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%s %d: %w", entityType, id, model.ErrNotFound)
	}
	return nil
}

// FindEntityIDsByCore returns IDs of non-archived entities whose core field
// equals value. For people, field "name" matches first_name + " " + last_name.
func (r *ImportRepo) FindEntityIDsByCore(ctx context.Context, entityType, field, value string) ([]int64, error) {
	table, err := importTable(entityType)
	if err != nil {
		return nil, err
	}
	expr := field
	switch {
	case entityType == "person" && field == "name":
		expr = "first_name || COALESCE(' ' || last_name, '')"
	case !slices.Contains(model.CoreColumns[entityType], field):
		return nil, fmt.Errorf("unknown %s field %q: %w", entityType, field, model.ErrValidation)
	}
	rows, err := r.db.QueryContext(ctx,
		fmt.Sprintf("SELECT id FROM %s WHERE archived = 0 AND %s = ? ORDER BY id", table, expr), value)
	if err != nil {
		return nil, fmt.Errorf("find %s by %s: %w", entityType, field, err)
	}
	return scanIDs(rows)
}

// ListEntityRows returns every non-archived entity of a type, ordered by ID,
// with core columns and custom fields, plus the custom field keys in order of
// first appearance.
func (r *ImportRepo) ListEntityRows(ctx context.Context, entityType string) ([]*model.EntityRow, []string, error) {
	table, err := importTable(entityType)
	if err != nil {
		return nil, nil, err
	}
	rows, err := r.loadRows(ctx, entityType,
		fmt.Sprintf("SELECT e.id, %s FROM %s e WHERE e.archived = 0 ORDER BY e.id", prefixed("e.", entityType), table))
	if err != nil {
		return nil, nil, err
	}
	keys, err := r.attachCustom(ctx, entityType, rows,
		"SELECT entity_id, field_name, field_value FROM custom_fields WHERE entity_type = ? ORDER BY id", entityType)
	return rows, keys, err
}

// BatchRecords returns the rows of an import batch in original order with the
// cells each was imported from. They do not change when the entity is edited,
// re-imported or archived. Cells is nil for a batch imported before cells were
// recorded: read those with BatchRows.
func (r *ImportRepo) BatchRecords(ctx context.Context, b *model.ImportBatch) ([]*model.BatchRecord, error) {
	rs, err := r.db.QueryContext(ctx,
		"SELECT entity_id, cells FROM import_rows WHERE batch_id = ? ORDER BY position", b.ID)
	if err != nil {
		return nil, fmt.Errorf("list import batch %d rows: %w", b.ID, err)
	}
	defer rs.Close()
	var out []*model.BatchRecord
	for rs.Next() {
		var rec model.BatchRecord
		var cells sql.NullString
		if err := rs.Scan(&rec.EntityID, &cells); err != nil {
			return nil, fmt.Errorf("scan import batch %d row: %w", b.ID, err)
		}
		if cells.Valid {
			if err := json.Unmarshal([]byte(cells.String), &rec.Cells); err != nil {
				return nil, fmt.Errorf("decode import batch %d row cells: %w", b.ID, err)
			}
		}
		out = append(out, &rec)
	}
	return out, rs.Err()
}

// BatchRows returns the entities of an import batch in original row order.
// Rows whose entity has since been archived are omitted.
func (r *ImportRepo) BatchRows(ctx context.Context, b *model.ImportBatch) ([]*model.EntityRow, error) {
	table, err := importTable(b.EntityType)
	if err != nil {
		return nil, err
	}
	rows, err := r.loadRows(ctx, b.EntityType,
		fmt.Sprintf(`SELECT e.id, %s FROM import_rows ir JOIN %s e ON e.id = ir.entity_id AND e.archived = 0
		 WHERE ir.batch_id = ? ORDER BY ir.position`, prefixed("e.", b.EntityType), table), b.ID)
	if err != nil {
		return nil, err
	}
	_, err = r.attachCustom(ctx, b.EntityType, rows,
		`SELECT cf.entity_id, cf.field_name, cf.field_value FROM custom_fields cf
		 WHERE cf.entity_type = ? AND cf.entity_id IN (SELECT entity_id FROM import_rows WHERE batch_id = ?)
		 ORDER BY cf.id`, b.EntityType, b.ID)
	return rows, err
}

func prefixed(prefix, entityType string) string {
	cols := make([]string, len(model.CoreColumns[entityType]))
	for i, c := range model.CoreColumns[entityType] {
		cols[i] = prefix + c
	}
	return strings.Join(cols, ", ")
}

func (r *ImportRepo) loadRows(ctx context.Context, entityType, query string, args ...any) ([]*model.EntityRow, error) {
	rs, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list %s rows: %w", entityType, err)
	}
	defer rs.Close()

	coreCols := model.CoreColumns[entityType]
	var out []*model.EntityRow
	for rs.Next() {
		row := &model.EntityRow{Core: make(map[string]*string, len(coreCols)), Custom: map[string]string{}}
		vals := make([]sql.NullString, len(coreCols))
		dest := []any{&row.ID}
		for i := range vals {
			dest = append(dest, &vals[i])
		}
		if err := rs.Scan(dest...); err != nil {
			return nil, fmt.Errorf("scan %s row: %w", entityType, err)
		}
		for i, c := range coreCols {
			if vals[i].Valid {
				s := vals[i].String
				row.Core[c] = &s
			} else {
				row.Core[c] = nil
			}
		}
		out = append(out, row)
	}
	return out, rs.Err()
}

// attachCustom fills row.Custom and returns the keys seen, in order of first
// appearance, for entities present in rows.
func (r *ImportRepo) attachCustom(ctx context.Context, entityType string, rows []*model.EntityRow, query string, args ...any) ([]string, error) {
	byID := make(map[int64]*model.EntityRow, len(rows))
	for _, row := range rows {
		byID[row.ID] = row
	}
	rs, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list %s custom fields: %w", entityType, err)
	}
	defer rs.Close()
	var keys []string
	seen := map[string]bool{}
	for rs.Next() {
		var id int64
		var key, value string
		if err := rs.Scan(&id, &key, &value); err != nil {
			return nil, fmt.Errorf("scan custom field: %w", err)
		}
		row, ok := byID[id]
		if !ok {
			continue
		}
		row.Custom[key] = value
		if !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	return keys, rs.Err()
}
