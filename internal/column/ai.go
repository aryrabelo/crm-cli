package column

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/jdanielnd/crm-cli/internal/model"
	"github.com/jdanielnd/crm-cli/internal/provider"
	"github.com/jdanielnd/crm-cli/internal/value"
)

// Output kinds of an ai column (--output).
const (
	OutputText  = "text"
	OutputScore = "score"
	OutputEnum  = "enum"
	OutputBool  = "bool"
)

// OutputSpec is the typed answer of an ai column: text, score:MIN-MAX,
// enum:a|b|c or bool. Answers are stored as JSON strings, numbers or
// booleans, so --only filters compare them like any other cell.
type OutputSpec struct {
	Kind   string
	Min    float64
	Max    float64
	Values []string
}

var scoreRange = regexp.MustCompile(`^(-?\d+(?:\.\d+)?)-(-?\d+(?:\.\d+)?)$`)

// ParseOutput parses an --output spec.
func ParseOutput(s string) (*OutputSpec, error) {
	kind, arg, hasArg := strings.Cut(strings.TrimSpace(s), ":")
	invalid := func(why string) error {
		return model.NewExitError(model.ErrValidation, "invalid --output %q: %s", s, why)
	}
	spec := &OutputSpec{Kind: kind}
	switch kind {
	case OutputText, OutputBool:
		if hasArg {
			return nil, invalid(kind + " takes no argument")
		}
	case OutputScore:
		m := scoreRange.FindStringSubmatch(arg)
		if m == nil {
			return nil, invalid("use score:MIN-MAX (e.g. score:0-100)")
		}
		spec.Min, _ = strconv.ParseFloat(m[1], 64)
		spec.Max, _ = strconv.ParseFloat(m[2], 64)
		if spec.Min >= spec.Max {
			return nil, invalid("MIN must be below MAX")
		}
	case OutputEnum:
		for _, v := range strings.Split(arg, "|") {
			v = strings.TrimSpace(v)
			if v == "" {
				return nil, invalid("use enum:a|b|c with non-empty values")
			}
			for _, seen := range spec.Values {
				if strings.EqualFold(seen, v) {
					return nil, invalid(fmt.Sprintf("value %q is listed twice", v))
				}
			}
			spec.Values = append(spec.Values, v)
		}
		if len(spec.Values) < 2 {
			return nil, invalid("an enum needs at least two values")
		}
	default:
		return nil, model.NewExitError(model.ErrValidation, "unknown --output %q (use text, score:MIN-MAX, enum:a|b|c or bool)", s)
	}
	return spec, nil
}

func formatNumber(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// shape is the JSON example of the value in the instructions.
func (o *OutputSpec) shape() string {
	switch o.Kind {
	case OutputScore:
		return "<number>"
	case OutputEnum:
		return `"` + strings.Join(o.Values, `" | "`) + `"`
	case OutputBool:
		return "true | false"
	default:
		return `"<text>"`
	}
}

// describe tells the model what a valid value is.
func (o *OutputSpec) describe() string {
	switch o.Kind {
	case OutputScore:
		return fmt.Sprintf("a JSON number from %s to %s", formatNumber(o.Min), formatNumber(o.Max))
	case OutputEnum:
		return `exactly one of the strings "` + strings.Join(o.Values, `", "`) + `"`
	case OutputBool:
		return "a JSON boolean, true or false"
	default:
		return "a short, non-empty JSON string"
	}
}

// normalize checks an answered value and returns its stored form.
func (o *OutputSpec) normalize(v any) (any, error) {
	switch o.Kind {
	case OutputScore:
		var f float64
		switch x := v.(type) {
		case float64:
			f = x
		case string:
			parsed, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
			if err != nil {
				return nil, fmt.Errorf(`"value" must be a number, got %q`, x)
			}
			f = parsed
		default:
			return nil, fmt.Errorf(`"value" must be a number, got %s`, value.Text(v))
		}
		if f < o.Min || f > o.Max {
			return nil, fmt.Errorf(`"value" %s is outside %s-%s`, formatNumber(f), formatNumber(o.Min), formatNumber(o.Max))
		}
		return f, nil
	case OutputEnum:
		s, _ := v.(string)
		for _, allowed := range o.Values {
			if strings.EqualFold(strings.TrimSpace(s), allowed) {
				return allowed, nil
			}
		}
		return nil, fmt.Errorf(`"value" must be one of %q, got %s`, o.Values, value.Text(v))
	case OutputBool:
		switch x := v.(type) {
		case bool:
			return x, nil
		case string:
			switch strings.ToLower(strings.TrimSpace(x)) {
			case "true", "yes":
				return true, nil
			case "false", "no":
				return false, nil
			}
		}
		return nil, fmt.Errorf(`"value" must be true or false, got %s`, value.Text(v))
	default:
		s, ok := v.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return nil, errors.New(`"value" must be a non-empty string`)
		}
		return strings.TrimSpace(s), nil
	}
}

// validateAI checks an ai column against the other columns of its table.
func (c *Column) validateAI(exists func(string) bool, columns []*Column) error {
	if c.Config.Expr != "" || c.Config.From != "" || c.Config.Field != "" {
		return model.NewExitError(model.ErrValidation, "ai column %q takes no --expr, --from or --field", c.Name)
	}
	if len(c.Config.Waterfall) == 0 {
		return model.NewExitError(model.ErrValidation, "ai column %q needs an llm provider (--waterfall llm)", c.Name)
	}
	if strings.TrimSpace(c.Config.Prompt) == "" {
		return model.NewExitError(model.ErrValidation, "ai column %q needs --prompt <file.md|text>", c.Name)
	}
	if _, err := ParseOutput(c.Config.Output); err != nil {
		return err
	}
	if c.Config.FieldLimit < 0 {
		return model.NewExitError(model.ErrValidation, "ai column %q: field limit must be 0 (no cap) or more", c.Name)
	}
	refs := placeholder.FindAllStringSubmatch(c.Config.Prompt, -1)
	if len(refs) == 0 {
		return model.NewExitError(model.ErrValidation, "ai column %q: the prompt uses no {{column}} placeholder, so every row would get the same answer", c.Name)
	}
	for _, m := range refs {
		if _, _, ok := resolveRef(m[1], exists); !ok {
			return model.NewExitError(model.ErrValidation, "ai column %q: prompt placeholder {{%s}} references no column", c.Name, m[1])
		}
	}
	if r := c.Config.Reason; r != "" {
		if r == c.Name {
			return model.NewExitError(model.ErrValidation, "ai column %q cannot be its own reason column", c.Name)
		}
		for _, o := range columns {
			if o.Name == r && o.Type == Input {
				return nil
			}
		}
		return model.NewExitError(model.ErrValidation, "ai column %q: reason column %q must be an existing input column", c.Name, r)
	}
	return nil
}

// aiInput renders the prompt for one row. A row where every placeholder is
// empty has no input (no call is made).
func (c *Column) aiInput(cells map[string]any) provider.Input {
	isColumn := func(n string) bool { _, ok := cells[n]; return ok }
	for _, m := range placeholder.FindAllStringSubmatch(c.Config.Prompt, -1) {
		name, path, ok := resolveRef(m[1], isColumn)
		if !ok {
			continue
		}
		if v, _ := value.Lookup(cells[name], path); !value.Empty(v) {
			return provider.Input{Value: render(c.Config.Prompt, cells, c.Config.FieldLimit)}
		}
	}
	return provider.Input{}
}

// contract is what the llm must answer for this column: {"value": ...} plus
// "reason" when the column has a reason column.
func (c *Column) contract() (provider.Contract, error) {
	spec, err := ParseOutput(c.Config.Output)
	if err != nil {
		return provider.Contract{}, err
	}
	explain := c.Config.Reason != ""
	var b strings.Builder
	fmt.Fprintf(&b, "You compute the cell %q of one table row. The user message holds the task and the row's fields.\n", c.Name)
	b.WriteString("Reply with a single JSON object and no other text:\n")
	if explain {
		fmt.Fprintf(&b, "{\"value\": %s, \"reason\": \"<one or two sentences>\"}\n", spec.shape())
	} else {
		fmt.Fprintf(&b, "{\"value\": %s}\n", spec.shape())
	}
	fmt.Fprintf(&b, "- value: %s.\n", spec.describe())
	if explain {
		b.WriteString("- reason: one or two sentences justifying the value, citing by name the row fields it is based on.\n")
	}
	return provider.Contract{
		Instructions: b.String(),
		Check: func(answer map[string]any) (any, error) {
			raw, ok := answer["value"]
			if !ok {
				return nil, errors.New(`missing "value"`)
			}
			v, err := spec.normalize(raw)
			if err != nil {
				return nil, err
			}
			out := map[string]any{"value": v}
			if explain {
				reason, _ := answer["reason"].(string)
				if strings.TrimSpace(reason) == "" {
					return nil, errors.New(`missing "reason"`)
				}
				out["reason"] = strings.TrimSpace(reason)
			}
			return out, nil
		},
	}, nil
}
