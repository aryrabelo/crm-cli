// Package bench measures what an agent pays to get CRM work done over MCP:
// tool calls, provider calls, llm tokens, cost and wall time per task. Tasks
// are JSON files (see bench/ in the repository) executed by an MCP client
// against an in-process or HTTP crm server whose providers are local fakes,
// so a run is deterministic and free. The same task can be run with the
// batch data-plane tools or with the upstream one-entity tools only, to show
// the call-count gap.
package bench

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/jdanielnd/crm-cli/internal/model"
)

// Mode selects the tool set a task is solved with.
type Mode string

const (
	// Batch solves tasks with the batch data-plane tools.
	Batch Mode = "batch"
	// PerEntity solves tasks with the upstream one-entity tools only.
	PerEntity Mode = "per-entity"
)

// Task is one benchmark task, loaded from a JSON file.
type Task struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Setup       Setup  `json:"setup"`
	// Batch and PerEntity are the solutions of the task per mode. A nil
	// PerEntity is not possible with the one-entity tools: PerEntityNA
	// says why.
	Batch       *Plan  `json:"batch"`
	PerEntity   *Plan  `json:"per_entity,omitempty"`
	PerEntityNA string `json:"per_entity_na,omitempty"`

	// dir is the directory of the task file; file paths resolve against it.
	dir string
}

// Setup is the state a task starts from, written directly (not counted):
// Table is used in batch mode, Entities in per-entity mode.
type Setup struct {
	Table    *TableSetup  `json:"table,omitempty"`
	Entities *EntitySetup `json:"entities,omitempty"`
}

// TableSetup imports a CSV file into a table.
type TableSetup struct {
	Name string `json:"name"`
	CSV  string `json:"csv"`
	Key  string `json:"key,omitempty"`
}

// EntitySetup imports a CSV file as people or organizations (crm import).
type EntitySetup struct {
	Type string `json:"type"`
	CSV  string `json:"csv"`
}

// Plan is the tool calls that solve a task in one mode, and the checks
// that decide whether they did.
type Plan struct {
	Steps  []Step  `json:"steps"`
	Checks []Check `json:"checks"`
}

// Step is one tool call, or (Foreach) a list of calls repeated per row of
// a CSV file. String arguments are templates:
//
//	{{file:path}}   the content of a file (relative to the task file)
//	{{rows:path}}   the rows of a CSV file as an array of objects
//	{{row.col}}     the cell col of the current Foreach row
//	{{last.path}}   a value of the previous result in the same row
//
// A string that is exactly one placeholder takes the value's JSON type.
type Step struct {
	Tool    string         `json:"tool,omitempty"`
	Args    map[string]any `json:"args,omitempty"`
	Foreach string         `json:"foreach,omitempty"`
	Calls   []Step         `json:"calls,omitempty"`
	// AllowError keeps going when the tool answers with an error (e.g. a
	// duplicate rejected by crm_person_create); the error is still counted.
	AllowError bool `json:"allow_error,omitempty"`
}

// Check is one pass condition: SQL is a query returning one value, or
// Result/Path select a value of a tool result (Result indexes the results
// of the task in call order; negative counts from the end). The value must
// equal Equals, be at least Gte / at most Lte, or (Len) have that length.
type Check struct {
	SQL    string   `json:"sql,omitempty"`
	Result *int     `json:"result,omitempty"`
	Path   string   `json:"path,omitempty"`
	Equals any      `json:"equals,omitempty"`
	Gte    *float64 `json:"gte,omitempty"`
	Lte    *float64 `json:"lte,omitempty"`
	Len    *int     `json:"len,omitempty"`
}

// Plan returns the plan of mode m (nil when the task is not possible in m).
func (t *Task) Plan(m Mode) *Plan {
	if m == PerEntity {
		return t.PerEntity
	}
	return t.Batch
}

// path resolves a file path of the task.
func (t *Task) path(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(t.dir, p)
}

// LoadTasks reads every *.json task in dir, sorted by file name.
func LoadTasks(dir string) ([]*Task, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	if len(files) == 0 {
		return nil, model.NewExitError(model.ErrNotFound, "no bench tasks (*.json) in %s (run from the repository root or pass --tasks)", dir)
	}
	slices.Sort(files)
	tasks := make([]*Task, 0, len(files))
	names := map[string]string{}
	for _, f := range files {
		t, err := loadTask(f)
		if err != nil {
			return nil, err
		}
		if prev, dup := names[t.Name]; dup {
			return nil, model.NewExitError(model.ErrValidation, "tasks %s and %s are both named %q", prev, f, t.Name)
		}
		names[t.Name] = f
		tasks = append(tasks, t)
	}
	return tasks, nil
}

func loadTask(file string) (*Task, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("read task: %w", err)
	}
	var t Task
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, model.NewExitError(model.ErrValidation, "task %s: %s", file, err)
	}
	t.dir = filepath.Dir(file)
	switch {
	case t.Name == "":
		return nil, model.NewExitError(model.ErrValidation, "task %s has no name", file)
	case t.Batch == nil:
		return nil, model.NewExitError(model.ErrValidation, "task %s has no batch plan", file)
	case t.PerEntity == nil && t.PerEntityNA == "":
		return nil, model.NewExitError(model.ErrValidation, "task %s: give a per_entity plan or per_entity_na", file)
	}
	for _, p := range []*Plan{t.Batch, t.PerEntity} {
		if p != nil && len(p.Checks) == 0 {
			return nil, model.NewExitError(model.ErrValidation, "task %s: every plan needs checks", file)
		}
	}
	return &t, nil
}
