// Package column defines workspace column types and how one column is
// computed for one row: input cells are stored, formula cells are evaluated
// on read from a {{column}} template, enrich cells come from a provider
// waterfall and ai cells from an llm answering a {{column}} prompt, both
// during `crm run`.
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

// Type is a column type. The schema also reserves signal and score for
// later slices.
type Type string

const (
	Input   Type = "input"
	Formula Type = "formula"
	Enrich  Type = "enrich"
	AI      Type = "ai"
)

// ParseType accepts the column types `crm column add` supports.
func ParseType(s string) (Type, error) {
	switch t := Type(s); t {
	case Input, Formula, Enrich, AI:
		return t, nil
	default:
		return "", model.NewExitError(model.ErrValidation, "unknown column type %q (use input, formula, enrich or ai)", s)
	}
}

// Runnable reports whether `crm run` computes columns of type t.
func (t Type) Runnable() bool { return t == Enrich || t == AI }

// Config is the type-specific definition of a column.
type Config struct {
	// Expr is the formula template: text with {{column}} or {{column.json.path}}.
	Expr string `json:"expr,omitempty"`
	// Waterfall is the ordered provider list of an enrich or ai column.
	Waterfall []string `json:"waterfall,omitempty"`
	// From is the column whose cell is the enrich input.
	From string `json:"from,omitempty"`
	// Field selects the stored value inside the provider answer ("company").
	// An answer whose field is empty falls through to the next provider.
	Field string `json:"field,omitempty"`
	// Prompt is the ai prompt template, with {{column}} placeholders.
	Prompt string `json:"prompt,omitempty"`
	// Output is the ai answer type: text, score:MIN-MAX, enum:a|b|c or bool.
	Output string `json:"output,omitempty"`
	// Reason is the input column that receives the 1–2 sentence reason the
	// llm gives with each ai answer (--explain); empty means no reason.
	Reason string `json:"reason,omitempty"`
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
	aiConfig := c.Config.Prompt != "" || c.Config.Output != "" || c.Config.Reason != ""
	switch c.Type {
	case Input:
		if c.Config.Expr != "" || len(c.Config.Waterfall) > 0 || c.Config.From != "" || c.Config.Field != "" || aiConfig {
			return model.NewExitError(model.ErrValidation, "input column %q takes no definition flags", c.Name)
		}
	case Formula:
		if c.Config.Expr == "" {
			return model.NewExitError(model.ErrValidation, "formula column %q needs --expr (e.g. \"{{first}} {{last}}\")", c.Name)
		}
		if len(c.Config.Waterfall) > 0 || c.Config.From != "" || c.Config.Field != "" || aiConfig {
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
		if c.Config.Expr != "" || aiConfig {
			return model.NewExitError(model.ErrValidation, "enrich column %q takes no --expr, --prompt, --output or --explain", c.Name)
		}
		if c.Config.From == "" {
			return model.NewExitError(model.ErrValidation, "enrich column %q needs --from <column> (or a table imported with --key)", c.Name)
		}
		if !exists(c.Config.From) {
			return model.NewExitError(model.ErrValidation, "enrich column %q: --from %q is not a column", c.Name, c.Config.From)
		}
	case AI:
		return c.validateAI(exists, columns)
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

// Input is the provider input of a runnable column for one row: the --from
// cell of an enrich column, the rendered prompt of an ai column. An empty
// Value means the row has no input (no call is made).
func (c *Column) Input(cells map[string]any) provider.Input {
	if c.Type == AI {
		return c.aiInput(cells)
	}
	return provider.Input{Value: strings.TrimSpace(value.Text(cells[c.Config.From]))}
}

// Sources returns the columns a runnable column reads its input from.
func (c *Column) Sources(columns []*Column) []string {
	switch c.Type {
	case Enrich:
		return []string{c.Config.From}
	case AI:
		isColumn := func(n string) bool {
			for _, o := range columns {
				if o.Name == n {
					return true
				}
			}
			return false
		}
		var out []string
		for _, m := range placeholder.FindAllStringSubmatch(c.Config.Prompt, -1) {
			if name, _, ok := resolveRef(m[1], isColumn); ok {
				out = append(out, name)
			}
		}
		return out
	default:
		return nil
	}
}

// Cells maps a run answer to the row cells it fills: the column itself and,
// for an ai column with a reason column, the reason. A nil answer (no data)
// clears them.
func (c *Column) Cells(answer any) map[string]any {
	if c.Type != AI {
		return map[string]any{c.Name: answer}
	}
	obj, _ := answer.(map[string]any)
	cells := map[string]any{c.Name: obj["value"]}
	if c.Config.Reason != "" {
		cells[c.Config.Reason] = obj["reason"]
	}
	return cells
}

// Waterfall opens the providers of a runnable column in order: enrich
// adapters, or llm providers bound to the ai column's answer contract.
func (c *Column) Waterfall(ctx context.Context, providers *provider.Store, registry *provider.Registry, cache *waterfall.Cache) (*waterfall.Waterfall, []*provider.Config, error) {
	var contract provider.Contract
	if c.Type == AI {
		var err error
		if contract, err = c.contract(); err != nil {
			return nil, nil, err
		}
	}
	wf := &waterfall.Waterfall{Cache: cache}
	configs := make([]*provider.Config, 0, len(c.Config.Waterfall))
	for _, name := range c.Config.Waterfall {
		cfg, err := providers.Get(ctx, name)
		if err != nil {
			return nil, nil, err
		}
		var a provider.Adapter
		if c.Type == AI {
			l, err := registry.OpenLLM(cfg)
			if err != nil {
				return nil, nil, err
			}
			a = l.Bind(contract)
		} else if a, err = registry.Open(cfg); err != nil {
			return nil, nil, err
		}
		wf.Steps = append(wf.Steps, waterfall.Step{Name: name, Adapter: a})
		configs = append(configs, cfg)
	}
	return wf, configs, nil
}
