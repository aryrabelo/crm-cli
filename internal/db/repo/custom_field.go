package repo

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jdanielnd/crm-cli/internal/model"
)

// DBTX is satisfied by both *sql.DB and *sql.Tx, so repositories built on it
// can run inside a caller-managed transaction.
type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// entityTables maps entity types to their table names. Values are constants,
// safe to interpolate into SQL.
var entityTables = map[string]string{
	"person":       "people",
	"organization": "organizations",
	"deal":         "deals",
	"interaction":  "interactions",
}

// CustomFieldRepo handles free-form key/value fields on any entity.
type CustomFieldRepo struct {
	db DBTX
}

// NewCustomFieldRepo creates a new CustomFieldRepo.
func NewCustomFieldRepo(db DBTX) *CustomFieldRepo {
	return &CustomFieldRepo{db: db}
}

func validateCustomFieldTarget(entityType, key string) error {
	if !model.ValidEntityType(entityType) {
		return fmt.Errorf("invalid entity type %q: %w", entityType, model.ErrValidation)
	}
	if key == "" {
		return fmt.Errorf("custom field key cannot be empty: %w", model.ErrValidation)
	}
	return nil
}

// Set creates or replaces the value of a custom field.
func (r *CustomFieldRepo) Set(ctx context.Context, entityType string, entityID int64, key, value string) error {
	if err := validateCustomFieldTarget(entityType, key); err != nil {
		return err
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO custom_fields (entity_type, entity_id, field_name, field_value) VALUES (?, ?, ?, ?)
		 ON CONFLICT(entity_type, entity_id, field_name) DO UPDATE SET field_value = excluded.field_value`,
		entityType, entityID, key, value)
	if err != nil {
		return fmt.Errorf("set custom field %q on %s %d: %w", key, entityType, entityID, err)
	}
	return nil
}

// Delete removes a custom field. No-op if it does not exist.
func (r *CustomFieldRepo) Delete(ctx context.Context, entityType string, entityID int64, key string) error {
	if err := validateCustomFieldTarget(entityType, key); err != nil {
		return err
	}
	_, err := r.db.ExecContext(ctx,
		"DELETE FROM custom_fields WHERE entity_type = ? AND entity_id = ? AND field_name = ?",
		entityType, entityID, key)
	if err != nil {
		return fmt.Errorf("delete custom field %q on %s %d: %w", key, entityType, entityID, err)
	}
	return nil
}

// GetForEntity returns an entity's custom fields in creation order.
func (r *CustomFieldRepo) GetForEntity(ctx context.Context, entityType string, entityID int64) ([]*model.CustomField, error) {
	return r.query(ctx,
		`SELECT id, entity_type, entity_id, field_name, field_value FROM custom_fields
		 WHERE entity_type = ? AND entity_id = ? ORDER BY id`,
		entityType, entityID)
}

// ListByEntityType returns every custom field of an entity type in creation order.
func (r *CustomFieldRepo) ListByEntityType(ctx context.Context, entityType string) ([]*model.CustomField, error) {
	return r.query(ctx,
		`SELECT id, entity_type, entity_id, field_name, field_value FROM custom_fields
		 WHERE entity_type = ? ORDER BY id`,
		entityType)
}

// FindEntityIDs returns IDs of non-archived entities whose custom field key equals value.
func (r *CustomFieldRepo) FindEntityIDs(ctx context.Context, entityType, key, value string) ([]int64, error) {
	if err := validateCustomFieldTarget(entityType, key); err != nil {
		return nil, err
	}
	table := entityTables[entityType]
	rows, err := r.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT cf.entity_id FROM custom_fields cf JOIN %s e ON e.id = cf.entity_id AND e.archived = 0
		 WHERE cf.entity_type = ? AND cf.field_name = ? AND cf.field_value = ? ORDER BY cf.entity_id`, table),
		entityType, key, value)
	if err != nil {
		return nil, fmt.Errorf("find %s by custom field %q: %w", entityType, key, err)
	}
	return scanIDs(rows)
}

func (r *CustomFieldRepo) query(ctx context.Context, query string, args ...any) ([]*model.CustomField, error) {
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list custom fields: %w", err)
	}
	defer rows.Close()

	var fields []*model.CustomField
	for rows.Next() {
		var f model.CustomField
		if err := rows.Scan(&f.ID, &f.EntityType, &f.EntityID, &f.Key, &f.Value); err != nil {
			return nil, fmt.Errorf("scan custom field: %w", err)
		}
		fields = append(fields, &f)
	}
	return fields, rows.Err()
}

func scanIDs(rows *sql.Rows) ([]int64, error) {
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
