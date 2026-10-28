// Package importer turns a parsed CSV into people or organizations plus custom
// fields, and maps entities back to CSV cells for `crm rows`.
//
// Column mapping: a column whose header matches a known alias (name, email,
// phone, company, domain, ...) goes to the core field; every other column
// becomes a custom field keyed by its header. --map overrides both.
package importer

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jdanielnd/crm-cli/internal/csvio"
	"github.com/jdanielnd/crm-cli/internal/db/repo"
	"github.com/jdanielnd/crm-cli/internal/model"
)

// CoreFields lists the fields a CSV column can map to, per entity type.
// "name" on a person is virtual: split into first_name/last_name on import
// and joined back on export.
var CoreFields = map[string][]string{
	"person":       {"name", "first_name", "last_name", "email", "phone", "title", "company", "location", "notes", "summary"},
	"organization": {"name", "domain", "industry", "notes", "summary"},
}

// aliases maps normalized headers (see normalizeHeader) to core fields.
var aliases = map[string]map[string]string{
	"person": {
		"name": "name", "fullname": "name", "nome": "name", "nomecompleto": "name", "contactname": "name",
		"firstname": "first_name", "givenname": "first_name", "primeironome": "first_name",
		"lastname": "last_name", "surname": "last_name", "familyname": "last_name", "sobrenome": "last_name",
		"email": "email", "emailaddress": "email", "mail": "email",
		"phone": "phone", "phonenumber": "phone", "telefone": "phone", "tel": "phone", "mobile": "phone", "celular": "phone", "whatsapp": "phone",
		"title": "title", "jobtitle": "title", "cargo": "title",
		"company": "company", "companyname": "company", "empresa": "company",
		"location": "location",
		"notes":    "notes", "note": "notes", "notas": "notes",
		"summary": "summary", "resumo": "summary",
	},
	"organization": {
		"name": "name", "company": "name", "companyname": "name", "organization": "name", "organisation": "name", "org": "name", "empresa": "name", "nome": "name",
		"domain": "domain", "website": "domain", "site": "domain",
		"industry": "industry", "sector": "industry", "setor": "industry", "segmento": "industry",
		"notes": "notes", "note": "notes", "notas": "notes",
		"summary": "summary", "resumo": "summary",
	},
}

func normalizeHeader(h string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '_', '-', '.':
			return -1
		}
		return r
	}, strings.ToLower(strings.TrimSpace(h)))
}

func isCore(entityType, field string) bool {
	return slices.Contains(CoreFields[entityType], field)
}

func validationError(format string, args ...any) error {
	return model.NewExitError(model.ErrValidation, format, args...)
}

// ResolveColumns maps each CSV header to a core field or a custom field key.
// maps holds "csvcol=field" overrides; an empty field keeps the column as a
// custom field named after its header.
func ResolveColumns(entityType string, header []string, maps []string) ([]model.ImportColumn, error) {
	if _, ok := CoreFields[entityType]; !ok {
		return nil, validationError("cannot import as %q (use person or org)", entityType)
	}
	index := make(map[string]int, len(header))
	for i, h := range header {
		if h == "" {
			return nil, validationError("column %d has an empty header; every column needs a name", i+1)
		}
		if prev, dup := index[h]; dup {
			return nil, validationError("duplicate header %q (columns %d and %d); rename one of them", h, prev+1, i+1)
		}
		index[h] = i
	}

	cols := make([]model.ImportColumn, len(header))
	explicit := make([]bool, len(header))
	for _, m := range maps {
		eq := strings.LastIndex(m, "=")
		if eq < 0 {
			return nil, validationError("invalid --map %q: expected csvcol=field", m)
		}
		h, field := m[:eq], strings.TrimSpace(m[eq+1:])
		i, ok := index[h]
		if !ok {
			return nil, validationError("--map %q: no column named %q in the header", m, h)
		}
		if explicit[i] {
			return nil, validationError("column %q is mapped twice by --map", h)
		}
		explicit[i] = true
		switch {
		case field == "":
			cols[i] = model.ImportColumn{Header: h, Field: h, Custom: true}
		case isCore(entityType, field):
			cols[i] = model.ImportColumn{Header: h, Field: field}
		default:
			cols[i] = model.ImportColumn{Header: h, Field: field, Custom: true}
		}
	}

	usedCore := map[string]string{}
	for i, c := range cols {
		if !explicit[i] || c.Custom {
			continue
		}
		if prev, dup := usedCore[c.Field]; dup {
			return nil, validationError("columns %q and %q both map to %s; map one elsewhere with --map", prev, c.Header, c.Field)
		}
		usedCore[c.Field] = c.Header
	}
	for i, h := range header {
		if explicit[i] {
			continue
		}
		if field, ok := aliases[entityType][normalizeHeader(h)]; ok {
			if _, taken := usedCore[field]; !taken {
				usedCore[field] = h
				cols[i] = model.ImportColumn{Header: h, Field: field}
				continue
			}
		}
		cols[i] = model.ImportColumn{Header: h, Field: h, Custom: true}
	}

	usedCustom := map[string]string{}
	for _, c := range cols {
		if !c.Custom {
			continue
		}
		if prev, dup := usedCustom[c.Field]; dup {
			return nil, validationError("columns %q and %q both map to custom field %q", prev, c.Header, c.Field)
		}
		usedCustom[c.Field] = c.Header
	}

	_, hasName := usedCore["name"]
	if entityType == "person" {
		_, hasFirst := usedCore["first_name"]
		_, hasLast := usedCore["last_name"]
		if hasName && (hasFirst || hasLast) {
			return nil, validationError("column %q maps to name and cannot be combined with first_name/last_name columns", usedCore["name"])
		}
		if !hasName && !hasFirst {
			return nil, validationError("no column maps to the person's name; use --map <column>=name (or =first_name)")
		}
	} else if !hasName {
		return nil, validationError("no column maps to the organization's name; use --map <column>=name")
	}
	return cols, nil
}

// ResolveKey finds the column used to match existing entities. key may be a
// CSV header or a mapped field name (core field or custom key).
func ResolveKey(cols []model.ImportColumn, key string) (int, error) {
	for i, c := range cols {
		if c.Header == key {
			return i, nil
		}
	}
	for i, c := range cols {
		if c.Field == key {
			return i, nil
		}
	}
	return -1, validationError("--key %q matches no column header or mapped field", key)
}

// SplitName splits a full name at its first space. The remainder is kept
// verbatim (possibly empty) so JoinName restores the exact original string.
func SplitName(full string) (string, *string) {
	i := strings.IndexByte(full, ' ')
	if i < 0 {
		return full, nil
	}
	last := full[i+1:]
	return full[:i], &last
}

// JoinName is the inverse of SplitName.
func JoinName(first string, last *string) string {
	if last == nil {
		return first
	}
	return first + " " + *last
}

// Options configures an import run.
type Options struct {
	EntityType string
	Source     string
	Maps       []string
	Key        string
}

// RowAction describes what happened (or would happen) to one data row.
type RowAction struct {
	Row      int    `json:"row"`
	Action   string `json:"action"`
	EntityID int64  `json:"id,omitempty"`
	Key      string `json:"key,omitempty"`
}

// Result summarizes an import run.
type Result struct {
	Batch   *model.ImportBatch
	Created int
	Updated int
	Actions []RowAction
}

// Run writes table into the database through db (typically a transaction the
// caller commits, or rolls back for a dry run).
func Run(ctx context.Context, db repo.DBTX, opts Options, table *csvio.Table) (*Result, error) {
	cols, err := ResolveColumns(opts.EntityType, table.Header, opts.Maps)
	if err != nil {
		return nil, err
	}

	keyIdx := -1
	var keyColumn *string
	if opts.Key != "" {
		keyIdx, err = ResolveKey(cols, opts.Key)
		if err != nil {
			return nil, err
		}
		keyColumn = &cols[keyIdx].Header
		seen := make(map[string]int, len(table.Rows))
		for i, rec := range table.Rows {
			v := rec[keyIdx]
			if v == "" {
				return nil, validationError("row %d: key column %q is empty", i+1, cols[keyIdx].Header)
			}
			if prev, dup := seen[v]; dup {
				return nil, validationError("rows %d and %d have the same key %q in column %q", prev, i+1, v, cols[keyIdx].Header)
			}
			seen[v] = i + 1
		}
	}

	imports := repo.NewImportRepo(db)
	customs := repo.NewCustomFieldRepo(db)

	batch := &model.ImportBatch{
		EntityType:      opts.EntityType,
		Source:          opts.Source,
		Columns:         cols,
		KeyColumn:       keyColumn,
		HasBOM:          table.Dialect.BOM,
		CRLF:            table.Dialect.CRLF,
		TrailingNewline: table.Dialect.TrailingNewline,
		RowCount:        len(table.Rows),
	}
	if err := imports.CreateBatch(ctx, batch); err != nil {
		return nil, err
	}

	res := &Result{Batch: batch}
	for i, rec := range table.Rows {
		core := make(map[string]*string)
		for j, c := range cols {
			if c.Custom {
				continue
			}
			v := rec[j]
			if c.Field == "name" && opts.EntityType == "person" {
				first, last := SplitName(v)
				core["first_name"] = &first
				core["last_name"] = last
				continue
			}
			if v == "" {
				core[c.Field] = nil
			} else {
				core[c.Field] = &v
			}
		}

		action := RowAction{Row: i + 1, Action: "create"}
		var id int64
		if keyIdx >= 0 {
			action.Key = rec[keyIdx]
			id, err = findByKey(ctx, imports, customs, opts.EntityType, cols[keyIdx], rec[keyIdx])
			if err != nil {
				return nil, fmt.Errorf("row %d: %w", i+1, err)
			}
		}
		if id != 0 {
			action.Action = "update"
			if err := imports.UpdateEntity(ctx, opts.EntityType, id, core); err != nil {
				return nil, fmt.Errorf("row %d: %w", i+1, err)
			}
			res.Updated++
		} else {
			id, err = imports.CreateEntity(ctx, opts.EntityType, core)
			if err != nil {
				return nil, fmt.Errorf("row %d: %w", i+1, err)
			}
			res.Created++
		}
		action.EntityID = id

		for j, c := range cols {
			if !c.Custom {
				continue
			}
			if rec[j] == "" {
				if action.Action == "update" {
					if err := customs.Delete(ctx, opts.EntityType, id, c.Field); err != nil {
						return nil, fmt.Errorf("row %d: %w", i+1, err)
					}
				}
				continue
			}
			if err := customs.Set(ctx, opts.EntityType, id, c.Field, rec[j]); err != nil {
				return nil, fmt.Errorf("row %d: %w", i+1, err)
			}
		}

		if err := imports.AddRow(ctx, batch.ID, i+1, id, rec); err != nil {
			return nil, err
		}
		res.Actions = append(res.Actions, action)
	}
	return res, nil
}

func findByKey(ctx context.Context, imports *repo.ImportRepo, customs *repo.CustomFieldRepo, entityType string, col model.ImportColumn, value string) (int64, error) {
	var ids []int64
	var err error
	if col.Custom {
		ids, err = customs.FindEntityIDs(ctx, entityType, col.Field, value)
	} else {
		ids, err = imports.FindEntityIDsByCore(ctx, entityType, col.Field, value)
	}
	if err != nil {
		return 0, err
	}
	switch len(ids) {
	case 0:
		return 0, nil
	case 1:
		return ids[0], nil
	default:
		return 0, model.NewExitError(model.ErrConflict, "key %s=%q matches %d existing %s records (ids %v); resolve the duplicates first", col.Field, value, len(ids), entityType, ids)
	}
}

// Cell returns the CSV value of column c for an entity row.
func Cell(entityType string, c model.ImportColumn, row *model.EntityRow) string {
	if c.Custom {
		return row.Custom[c.Field]
	}
	if entityType == "person" && c.Field == "name" {
		first := ""
		if v := row.Core["first_name"]; v != nil {
			first = *v
		}
		return JoinName(first, row.Core["last_name"])
	}
	if v := row.Core[c.Field]; v != nil {
		return *v
	}
	return ""
}

// ExportColumns returns the columns for exporting all entities of a type
// without a source batch: raw core columns, then custom keys. A custom key
// whose header collides with the id, a core column or an earlier custom
// header is prefixed with "custom." until the header is unique.
func ExportColumns(entityType string, customKeys []string) []model.ImportColumn {
	core := model.CoreColumns[entityType]
	cols := make([]model.ImportColumn, 0, len(core)+len(customKeys))
	for _, f := range core {
		cols = append(cols, model.ImportColumn{Header: f, Field: f})
	}
	emitted := map[string]bool{"id": true}
	for _, f := range core {
		emitted[f] = true
	}
	for _, k := range customKeys {
		h := k
		for emitted[h] {
			h = "custom." + h
		}
		emitted[h] = true
		cols = append(cols, model.ImportColumn{Header: h, Field: k, Custom: true})
	}
	return cols
}
