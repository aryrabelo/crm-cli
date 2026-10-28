package model

// CustomField is a free-form key/value attached to an entity.
type CustomField struct {
	ID         int64  `json:"id"`
	EntityType string `json:"entity_type"`
	EntityID   int64  `json:"entity_id"`
	Key        string `json:"key"`
	Value      string `json:"value"`
}

// ImportColumn maps one CSV column to a destination.
// Custom=false: Field is a core field of the entity ("name", "email", ...).
// Custom=true: Field is the custom field key.
type ImportColumn struct {
	Header string `json:"header"`
	Field  string `json:"field"`
	Custom bool   `json:"custom"`
}

// ImportBatch records one `crm import` run.
type ImportBatch struct {
	ID              int64          `json:"id"`
	UUID            string         `json:"uuid"`
	EntityType      string         `json:"entity_type"`
	Source          string         `json:"source"`
	Columns         []ImportColumn `json:"columns"`
	KeyColumn       *string        `json:"key_column"`
	HasBOM          bool           `json:"has_bom"`
	CRLF            bool           `json:"crlf"`
	TrailingNewline bool           `json:"trailing_newline"`
	RowCount        int            `json:"row_count"`
	CreatedAt       string         `json:"created_at"`
}

// EntityRow is an entity's core column values (nil = NULL) plus its custom fields.
type EntityRow struct {
	ID     int64
	Core   map[string]*string
	Custom map[string]string
}

// BatchRecord is one data row of an import batch: the entity it landed on and
// the CSV cells it was imported from, one per batch column (nil for a batch
// imported before cells were recorded).
type BatchRecord struct {
	EntityID int64
	Cells    []string
}

// ImportableEntityTypes lists entity types `crm import` and `crm rows` accept.
var ImportableEntityTypes = []string{"person", "organization"}

// CoreColumns lists, per entity type, the database columns import/export read and write.
var CoreColumns = map[string][]string{
	"person":       {"first_name", "last_name", "email", "phone", "title", "company", "location", "notes", "summary"},
	"organization": {"name", "domain", "industry", "notes", "summary"},
}
