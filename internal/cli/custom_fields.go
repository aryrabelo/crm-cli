package cli

import (
	"fmt"
	"io"
	"slices"

	"github.com/jdanielnd/crm-cli/internal/format"
	"github.com/jdanielnd/crm-cli/internal/model"
)

var customFieldColumns = []format.ColumnDef{
	{Header: "Custom Field", Field: "key"},
	{Header: "Value", Field: "value"},
}

// outputWithCustomFields prints a single entity plus its custom fields:
// JSON nests them under "custom_fields"; CSV/TSV append "custom_fields.<key>"
// columns; table prints a second key/value table below the entity.
func outputWithCustomFields(w io.Writer, f format.Format, data map[string]any, columns []format.ColumnDef, fields []*model.CustomField) error {
	if flagQuiet {
		return format.Output(w, f, []map[string]any{data}, columns, true)
	}

	switch f {
	case format.FormatJSON:
		custom := make(map[string]string, len(fields))
		for _, cf := range fields {
			custom[cf.Key] = cf.Value
		}
		data["custom_fields"] = custom
		return format.Output(w, f, []map[string]any{data}, columns, false)
	case format.FormatCSV, format.FormatTSV:
		cols := slices.Clone(columns)
		for _, cf := range fields {
			name := "custom_fields." + cf.Key
			cols = append(cols, format.ColumnDef{Header: name, Field: name})
			data[name] = cf.Value
		}
		return format.Output(w, f, []map[string]any{data}, cols, false)
	default:
		if err := format.Output(w, f, []map[string]any{data}, columns, false); err != nil {
			return err
		}
		if len(fields) == 0 {
			return nil
		}
		kv := make([]map[string]any, len(fields))
		for i, cf := range fields {
			kv[i] = map[string]any{"key": cf.Key, "value": cf.Value}
		}
		fmt.Fprintln(w)
		return format.Output(w, f, kv, customFieldColumns, false)
	}
}
