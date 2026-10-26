package table

import (
	"regexp"
	"slices"
	"strconv"

	"github.com/jdanielnd/crm-cli/internal/column"
	"github.com/jdanielnd/crm-cli/internal/model"
	"github.com/jdanielnd/crm-cli/internal/value"
)

// Filter is one --only condition: <column><op><value> with op one of
// = != > >= < <= ~ (regex) !~ (not regex). Comparisons are numeric when both
// sides are numbers, textual otherwise; ordering never matches an empty cell.
// "col!=" keeps rows where col is not empty.
type Filter struct {
	Column string
	Op     string
	Value  string
	re     *regexp.Regexp
}

// filterOps is ordered so two-character operators win over their prefixes.
var filterOps = []string{"!=", ">=", "<=", "!~", "=", ">", "<", "~"}

// ParseFilter parses expr against the given column names.
func ParseFilter(expr string, columns []string) (*Filter, error) {
	for i := 1; i < len(expr); i++ {
		for _, op := range filterOps {
			if len(expr)-i < len(op) || expr[i:i+len(op)] != op {
				continue
			}
			f := &Filter{Column: expr[:i], Op: op, Value: expr[i+len(op):]}
			if !slices.Contains(columns, f.Column) {
				return nil, model.NewExitError(model.ErrValidation, "--only %q: no column named %q", expr, f.Column)
			}
			if op == "~" || op == "!~" {
				re, err := regexp.Compile(f.Value)
				if err != nil {
					return nil, model.NewExitError(model.ErrValidation, "--only %q: invalid regex: %s", expr, err)
				}
				f.re = re
			}
			return f, nil
		}
	}
	return nil, model.NewExitError(model.ErrValidation, "invalid --only %q: expected <column><op><value> with op one of = != > >= < <= ~ !~", expr)
}

// Match reports whether the row cells satisfy the filter.
func (f *Filter) Match(cells map[string]any) bool {
	cell := value.Text(cells[f.Column])
	switch f.Op {
	case "=":
		return equal(cell, f.Value)
	case "!=":
		return !equal(cell, f.Value)
	case "~":
		return f.re.MatchString(cell)
	case "!~":
		return !f.re.MatchString(cell)
	}
	if cell == "" {
		return false
	}
	c := compare(cell, f.Value)
	switch f.Op {
	case ">":
		return c > 0
	case ">=":
		return c >= 0
	case "<":
		return c < 0
	default: // "<="
		return c <= 0
	}
}

func equal(a, b string) bool {
	if a == b {
		return true
	}
	x, errA := strconv.ParseFloat(a, 64)
	y, errB := strconv.ParseFloat(b, 64)
	return errA == nil && errB == nil && x == y
}

func compare(a, b string) int {
	x, errA := strconv.ParseFloat(a, 64)
	y, errB := strconv.ParseFloat(b, 64)
	if errA == nil && errB == nil {
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
		return 0
	}
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// ParseFilters parses every --only expression.
func ParseFilters(exprs []string, columns []*column.Column) ([]*Filter, error) {
	names := make([]string, len(columns))
	for i, c := range columns {
		names[i] = c.Name
	}
	out := make([]*Filter, 0, len(exprs))
	for _, e := range exprs {
		f, err := ParseFilter(e, names)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

// View is a filtered, materialized table: one cell map per kept row (formula
// columns evaluated) in row order.
type View struct {
	Columns []*column.Column
	Rows    []*Row
	Cells   []map[string]any
}

// NewView materializes rows and keeps those matching every filter.
func NewView(columns []*column.Column, rows []*Row, filters []*Filter) *View {
	v := &View{Columns: columns}
rowLoop:
	for _, r := range rows {
		cells := column.Materialize(columns, r.Data)
		for _, f := range filters {
			if !f.Match(cells) {
				continue rowLoop
			}
		}
		v.Rows = append(v.Rows, r)
		v.Cells = append(v.Cells, cells)
	}
	return v
}

// Header returns the column names in order.
func (v *View) Header() []string {
	h := make([]string, len(v.Columns))
	for i, c := range v.Columns {
		h[i] = c.Name
	}
	return h
}

// Project returns the view restricted to the named columns, in that order.
// No names keeps every column.
func (v *View) Project(names []string) (*View, error) {
	if len(names) == 0 {
		return v, nil
	}
	cols := make([]*column.Column, len(names))
	for i, n := range names {
		j := slices.IndexFunc(v.Columns, func(c *column.Column) bool { return c.Name == n })
		if j < 0 {
			return nil, model.NewExitError(model.ErrValidation, "no column named %q", n)
		}
		if slices.Contains(names[:i], n) {
			return nil, model.NewExitError(model.ErrValidation, "column %q is listed twice", n)
		}
		cols[i] = v.Columns[j]
	}
	return &View{Columns: cols, Rows: v.Rows, Cells: v.Cells}, nil
}

// Records renders every kept row as text cells (CSV/TSV/table output).
func (v *View) Records() [][]string {
	out := make([][]string, len(v.Cells))
	for i, cells := range v.Cells {
		rec := make([]string, len(v.Columns))
		for j, c := range v.Columns {
			rec[j] = value.Text(cells[c.Name])
		}
		out[i] = rec
	}
	return out
}
