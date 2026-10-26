// Package column defines workspace column types and how one column is
// computed for one row: input cells are stored, formula cells are evaluated
// on read from a {{column}} template, enrich cells come from a provider
// waterfall during `crm run`.
package column

import (
	"context"
	"regexp"
	"strings"

	"github.com/jdanielnd/crm-cli/internal/model"
	"github.com/jdanielnd/crm-cli/internal/provider"
	"github.com/jdanielnd/crm-cli/internal/value"
	"github.com/jdanielnd/crm-cli/internal/waterfall"
)

// Type is a column type. The schema also reserves ai, signal and score for
// later slices; S1 implements these three.
type Type string

const (
	Input   Type = "input"
	Formula Type = "formula"
	Enrich  Type = "enrich"
)

// ParseType accepts the column types `crm column add` supports.
func ParseType(s string) (Type, error) {
	switch t := Type(s); t {
	case Input, Formula, Enrich:
		return t, nil
	default:
		return "", model.NewExitError(model.ErrValidation, "unknown column type %q (use input, formula or enrich)", s)
	}
}

// Config is the type-specific definition of a column.
type Config struct {
	// Expr is the formula template: text with {{column}} or {{column.json.path}}.
	Expr string `json:"expr,omitempty"`
	// Waterfall is the ordered provider list of an enrich column.
	Waterfall []string `json:"waterfall,omitempty"`
	// From is the column whose cell is the enrich input.
	From string `json:"from,omitempty"`
	// Field selects the stored value inside the provider answer ("company").
	// An answer whose field is empty falls through to the next provider.
	Field string `json:"field,omitempty"`
}

// Column is one declared column of a workspace.
type Column struct {
	ID          int64         `json:"id"`
	WorkspaceID int64         `json:"workspace_id"`
	Name        string        `json:"name"`
	Type        Type          `json:"type"`
	Config      Config        `json:"config"`
	CostEst     provider.Cost `json:"-"`
	Position    int           `json:"position"`
	CreatedAt   string        `json:"created_at"`
}

var placeholder = regexp.MustCompile(`\{\{\s*(.+?)\s*\}\}`)

// resolveRef splits a placeholder reference into a column name and a JSON
// path. The whole reference wins when it names a column ("a.b" may be a
// header); otherwise the longest column-name prefix before a dot is used.
func resolveRef(ref string, isColumn func(string) bool) (name, path string, ok bool) {
	if isColumn(ref) {
		return ref, "", true
	}
	for i := len(ref) - 1; i > 0; i-- {
		if ref[i] == '.' && isColumn(ref[:i]) {
			return ref[:i], ref[i+1:], true
		}
	}
	return "", "", false
}

// Validate checks c against the other active columns of its workspace
// (columns, in declaration order, not including c).
func (c *Column) Validate(columns []*Column) error {
	if strings.TrimSpace(c.Name) == "" {
		return model.NewExitError(model.ErrValidation, "column name is required")
	}
	exists := func(n string) bool {
		for _, o := range columns {
			if o.Name == n {
				return true
			}
		}
		return false
	}
	if exists(c.Name) {
		return model.NewExitError(model.ErrConflict, "column %q already exists", c.Name)
	}
	switch c.Type {
	case Input:
		if c.Config.Expr != "" || len(c.Config.Waterfall) > 0 || c.Config.From != "" || c.Config.Field != "" {
			return model.NewExitError(model.ErrValidation, "input column %q takes no --expr, --waterfall, --from or --field", c.Name)
		}
	case Formula:
		if c.Config.Expr == "" {
			return model.NewExitError(model.ErrValidation, "formula column %q needs --expr (e.g. \"{{first}} {{last}}\")", c.Name)
		}
		if len(c.Config.Waterfall) > 0 || c.Config.From != "" || c.Config.Field != "" {
			return model.NewExitError(model.ErrValidation, "formula column %q takes only --expr", c.Name)
		}
		for _, m := range placeholder.FindAllStringSubmatch(c.Config.Expr, -1) {
			if _, _, ok := resolveRef(m[1], exists); !ok {
				return model.NewExitError(model.ErrValidation, "formula %q: {{%s}} references no column", c.Name, m[1])
			}
		}
	case Enrich:
		if len(c.Config.Waterfall) == 0 {
			return model.NewExitError(model.ErrValidation, "enrich column %q needs --waterfall p1,p2,...", c.Name)
		}
		if c.Config.Expr != "" {
			return model.NewExitError(model.ErrValidation, "enrich column %q takes no --expr", c.Name)
		}
		if c.Config.From == "" {
			return model.NewExitError(model.ErrValidation, "enrich column %q needs --from <column> (or a table imported with --key)", c.Name)
		}
		if !exists(c.Config.From) {
			return model.NewExitError(model.ErrValidation, "enrich column %q: --from %q is not a column", c.Name, c.Config.From)
		}
	default:
		return model.NewExitError(model.ErrValidation, "column %q: type %q is not supported yet", c.Name, c.Type)
	}
	return nil
}

// EvalFormula renders a formula template against the row cells.
func EvalFormula(expr string, cells map[string]any) string {
	isColumn := func(n string) bool { _, ok := cells[n]; return ok }
	return placeholder.ReplaceAllStringFunc(expr, func(m string) string {
		ref := placeholder.FindStringSubmatch(m)[1]
		name, path, ok := resolveRef(ref, isColumn)
		if !ok {
			return ""
		}
		v, _ := value.Lookup(cells[name], path)
		return value.Text(v)
	})
}

// Materialize returns the row cells with every formula column evaluated, in
// declaration order (a formula may use an earlier formula). data is not
// modified. Columns missing from data read as empty.
func Materialize(columns []*Column, data map[string]any) map[string]any {
	cells := make(map[string]any, len(columns))
	for _, c := range columns {
		cells[c.Name] = data[c.Name]
	}
	for _, c := range columns {
		if c.Type == Formula {
			cells[c.Name] = EvalFormula(c.Config.Expr, cells)
		}
	}
	return cells
}

// EnrichInput is the provider input of an enrich column for one row.
func (c *Column) EnrichInput(cells map[string]any) provider.Input {
	return provider.Input{Value: strings.TrimSpace(value.Text(cells[c.Config.From]))}
}

// Waterfall opens the providers of an enrich column in order.
func (c *Column) Waterfall(ctx context.Context, providers *provider.Store, registry *provider.Registry, cache *waterfall.Cache) (*waterfall.Waterfall, []*provider.Config, error) {
	wf := &waterfall.Waterfall{Cache: cache}
	configs := make([]*provider.Config, 0, len(c.Config.Waterfall))
	for _, name := range c.Config.Waterfall {
		cfg, err := providers.Get(ctx, name)
		if err != nil {
			return nil, nil, err
		}
		a, err := registry.Open(cfg)
		if err != nil {
			return nil, nil, err
		}
		wf.Steps = append(wf.Steps, waterfall.Step{Name: name, Adapter: a})
		configs = append(configs, cfg)
	}
	return wf, configs, nil
}

// WorstCost is the cost of calling every provider of the waterfall once.
func WorstCost(configs []*provider.Config) provider.Cost {
	var total provider.Cost
	for _, cfg := range configs {
		total += cfg.Price
	}
	return total
}
