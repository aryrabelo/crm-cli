package bench

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jdanielnd/crm-cli/internal/csvio"
	"github.com/jdanielnd/crm-cli/internal/db"
	"github.com/jdanielnd/crm-cli/internal/importer"
	crmmcp "github.com/jdanielnd/crm-cli/internal/mcp"
	"github.com/jdanielnd/crm-cli/internal/provider"
	"github.com/jdanielnd/crm-cli/internal/run"
	"github.com/jdanielnd/crm-cli/internal/service"
	"github.com/jdanielnd/crm-cli/internal/table"
	"github.com/jdanielnd/crm-cli/internal/value"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	gomcp "github.com/mark3labs/mcp-go/mcp"
)

// Transports a Runner can reach the server with.
const (
	InProcess = "inprocess"
	HTTP      = "http"
)

// httpToken is the bearer token of the bench HTTP server.
const httpToken = "bench-token"

// Result is the measure of one task in one mode.
type Result struct {
	Task string `json:"task"`
	Mode Mode   `json:"mode"`
	// NA is set (with the reason) when the task is not possible in Mode;
	// the measures are then zero.
	NA string `json:"na,omitempty"`
	// Calls counts MCP tool calls, Errors the ones answered with an error.
	Calls  int `json:"calls"`
	Errors int `json:"errors"`
	// ProviderCalls, TokensIn/Out and Cost sum the runs of the task.
	ProviderCalls int           `json:"provider_calls"`
	TokensIn      int64         `json:"tokens_in"`
	TokensOut     int64         `json:"tokens_out"`
	Cost          provider.Cost `json:"-"`
	// Bytes counts the JSON exchanged: tool arguments plus results.
	Bytes int           `json:"bytes"`
	Wall  time.Duration `json:"-"`
	Pass  bool          `json:"pass"`
	// Failure says why a task did not pass.
	Failure string `json:"failure,omitempty"`
}

// Runner executes tasks, each on a fresh in-memory database, through an MCP
// client on the chosen transport.
type Runner struct {
	Transport string
	fakes     *Fakes
}

// NewRunner starts the fake providers. Close stops them.
func NewRunner(transport string) (*Runner, error) {
	if transport != InProcess && transport != HTTP {
		return nil, fmt.Errorf("transport must be %s or %s, not %q", InProcess, HTTP, transport)
	}
	return &Runner{Transport: transport, fakes: NewFakes()}, nil
}

// Close stops the fake providers.
func (r *Runner) Close() { r.fakes.Close() }

// Run executes task t in mode m. An error means the harness could not run
// the task (bad task file, setup failure); a task that runs but fails its
// checks is a Result with Pass false.
func (r *Runner) Run(ctx context.Context, t *Task, m Mode) (*Result, error) {
	res := &Result{Task: t.Name, Mode: m}
	plan := t.Plan(m)
	if plan == nil {
		res.NA = t.PerEntityNA
		return res, nil
	}
	d, err := db.Open(":memory:")
	if err != nil {
		return nil, err
	}
	defer d.Close()
	if err := r.fakes.Provision(ctx, d); err != nil {
		return nil, fmt.Errorf("task %s: provision providers: %w", t.Name, err)
	}
	if err := setup(ctx, d, t, m); err != nil {
		return nil, fmt.Errorf("task %s: setup: %w", t.Name, err)
	}
	srv := crmmcp.New(d, "bench", crmmcp.WithRegistry(r.fakes.Registry()))
	defer srv.Close()
	c, stop, err := r.connect(ctx, srv)
	if err != nil {
		return nil, fmt.Errorf("task %s: connect: %w", t.Name, err)
	}
	defer stop()

	ex := &executor{task: t, mode: m, srv: srv, client: c, res: res}
	started := time.Now()
	stepErr := ex.steps(ctx, plan.Steps, nil)
	res.Wall = time.Since(started)
	if err := measureRuns(ctx, d, res); err != nil {
		return nil, err
	}
	if stepErr != nil {
		res.Failure = stepErr.Error()
		return res, nil
	}
	for i, c := range plan.Checks {
		if err := ex.check(ctx, d, c); err != nil {
			res.Failure = fmt.Sprintf("check %d: %s", i+1, err)
			return res, nil
		}
	}
	res.Pass = true
	return res, nil
}

// connect starts an initialized MCP client on srv; stop closes it (and the
// HTTP server).
func (r *Runner) connect(ctx context.Context, srv *crmmcp.Server) (*client.Client, func(), error) {
	var c *client.Client
	stop := func() {}
	var err error
	switch r.Transport {
	case HTTP:
		hs := httptest.NewServer(srv.Handler(httpToken))
		stop = hs.Close
		c, err = client.NewStreamableHttpClient(hs.URL+crmmcp.HTTPPath,
			transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + httpToken}))
	default:
		c, err = client.NewInProcessClient(srv.MCP())
	}
	if err != nil {
		stop()
		return nil, nil, err
	}
	closeAll := func() { _ = c.Close(); stop() }
	if err := c.Start(ctx); err != nil {
		closeAll()
		return nil, nil, err
	}
	init := gomcp.InitializeRequest{}
	init.Params.ProtocolVersion = gomcp.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = gomcp.Implementation{Name: "crm-bench", Version: "1"}
	if _, err := c.Initialize(ctx, init); err != nil {
		closeAll()
		return nil, nil, err
	}
	return c, closeAll, nil
}

// setup writes the starting state of mode m directly (not counted).
func setup(ctx context.Context, d *sql.DB, t *Task, m Mode) error {
	switch m {
	case Batch:
		s := t.Setup.Table
		if s == nil {
			return nil
		}
		parsed, err := readCSV(t.path(s.CSV))
		if err != nil {
			return err
		}
		_, err = service.New(d, nil).ImportCSV(ctx, table.ImportOptions{Name: s.Name, Key: s.Key, Source: s.CSV}, parsed)
		return err
	default:
		s := t.Setup.Entities
		if s == nil {
			return nil
		}
		parsed, err := readCSV(t.path(s.CSV))
		if err != nil {
			return err
		}
		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := importer.Run(ctx, tx, importer.Options{EntityType: s.Type, Source: s.CSV}, parsed); err != nil {
			return err
		}
		return tx.Commit()
	}
}

func readCSV(path string) (*csvio.Table, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return csvio.Parse(data)
}

// measureRuns adds the provider calls, tokens and spend of every run of the
// task to res.
func measureRuns(ctx context.Context, d *sql.DB, res *Result) error {
	runs, err := run.NewStore(d).List(ctx)
	if err != nil {
		return err
	}
	for _, r := range runs {
		res.ProviderCalls += r.Stats.Calls
		res.TokensIn += r.Stats.TokensIn
		res.TokensOut += r.Stats.TokensOut
		res.Cost += r.Spent
	}
	return nil
}

type executor struct {
	task    *Task
	mode    Mode
	srv     *crmmcp.Server
	client  *client.Client
	res     *Result
	results []any
}

// steps runs steps; row is the current Foreach row (nil outside one).
func (e *executor) steps(ctx context.Context, steps []Step, row map[string]string) error {
	var last any
	for i, st := range steps {
		if st.Foreach != "" {
			if row != nil {
				return fmt.Errorf("step %d: foreach cannot be nested", i+1)
			}
			if err := e.foreach(ctx, st); err != nil {
				return err
			}
			continue
		}
		out, err := e.call(ctx, st, row, last)
		if err != nil {
			return err
		}
		last = out
	}
	return nil
}

func (e *executor) foreach(ctx context.Context, st Step) error {
	parsed, err := readCSV(e.task.path(st.Foreach))
	if err != nil {
		return err
	}
	for _, rec := range parsed.Rows {
		row := make(map[string]string, len(parsed.Header))
		for j, h := range parsed.Header {
			row[h] = rec[j]
		}
		if err := e.steps(ctx, st.Calls, row); err != nil {
			return err
		}
	}
	return nil
}

// call makes one tool call and returns its decoded result.
func (e *executor) call(ctx context.Context, st Step, row map[string]string, last any) (any, error) {
	if e.mode == PerEntity && !e.srv.IsEntityTool(st.Tool) {
		return nil, fmt.Errorf("%s is not a one-entity tool (per-entity plans may only use the upstream tools)", st.Tool)
	}
	args, err := e.expand(st.Args, row, last)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", st.Tool, err)
	}
	argMap, _ := args.(map[string]any)
	req := gomcp.CallToolRequest{}
	req.Params.Name, req.Params.Arguments = st.Tool, argMap
	raw, _ := json.Marshal(argMap)
	e.res.Bytes += len(raw)
	e.res.Calls++
	out, err := e.client.CallTool(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", st.Tool, err)
	}
	text := ""
	for _, c := range out.Content {
		if tc, ok := c.(gomcp.TextContent); ok {
			text += tc.Text
		}
	}
	e.res.Bytes += len(text)
	var decoded any = text
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		decoded = text
	}
	e.results = append(e.results, decoded)
	if out.IsError {
		e.res.Errors++
		if !st.AllowError {
			return nil, fmt.Errorf("%s answered an error: %s", st.Tool, text)
		}
	}
	return decoded, nil
}

var placeholder = regexp.MustCompile(`\{\{\s*((?:file|rows):[^}]+|row\.[^}]+|last(?:\.[^}]*)?)\s*\}\}`)

// expand resolves the placeholders of v (see Step).
func (e *executor) expand(v any, row map[string]string, last any) (any, error) {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, item := range x {
			ev, err := e.expand(item, row, last)
			if err != nil {
				return nil, err
			}
			out[k] = ev
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			ev, err := e.expand(item, row, last)
			if err != nil {
				return nil, err
			}
			out[i] = ev
		}
		return out, nil
	case string:
		if m := placeholder.FindStringSubmatchIndex(x); m != nil && m[0] == 0 && m[1] == len(x) {
			return e.resolve(x[m[2]:m[3]], row, last)
		}
		var firstErr error
		s := placeholder.ReplaceAllStringFunc(x, func(p string) string {
			ref := placeholder.FindStringSubmatch(p)[1]
			val, err := e.resolve(ref, row, last)
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return ""
			}
			return value.Text(val)
		})
		return s, firstErr
	default:
		return v, nil
	}
}

func (e *executor) resolve(ref string, row map[string]string, last any) (any, error) {
	switch {
	case strings.HasPrefix(ref, "file:"):
		b, err := os.ReadFile(e.task.path(strings.TrimPrefix(ref, "file:")))
		if err != nil {
			return nil, err
		}
		return string(b), nil
	case strings.HasPrefix(ref, "rows:"):
		parsed, err := readCSV(e.task.path(strings.TrimPrefix(ref, "rows:")))
		if err != nil {
			return nil, err
		}
		rows := make([]any, len(parsed.Rows))
		for i, rec := range parsed.Rows {
			obj := make(map[string]any, len(parsed.Header))
			for j, h := range parsed.Header {
				obj[h] = rec[j]
			}
			rows[i] = obj
		}
		return rows, nil
	case strings.HasPrefix(ref, "row."):
		if row == nil {
			return nil, fmt.Errorf("{{%s}} outside a foreach", ref)
		}
		col := strings.TrimPrefix(ref, "row.")
		v, ok := row[col]
		if !ok {
			return nil, fmt.Errorf("{{%s}}: the foreach file has no column %q", ref, col)
		}
		return v, nil
	default:
		if last == nil {
			return nil, fmt.Errorf("{{%s}}: no previous result", ref)
		}
		path := strings.TrimPrefix(strings.TrimPrefix(ref, "last"), ".")
		v, ok := value.Lookup(last, path)
		if !ok {
			return nil, fmt.Errorf("{{%s}}: no such value in the previous result", ref)
		}
		return v, nil
	}
}

func (e *executor) check(ctx context.Context, d *sql.DB, c Check) error {
	var got any
	switch {
	case c.SQL != "":
		var v any
		if err := d.QueryRowContext(ctx, c.SQL).Scan(&v); err != nil {
			return fmt.Errorf("sql: %w", err)
		}
		if b, ok := v.([]byte); ok {
			v = string(b)
		}
		got = v
	case c.Result != nil:
		i := *c.Result
		if i < 0 {
			i += len(e.results)
		}
		if i < 0 || i >= len(e.results) {
			return fmt.Errorf("result %d: the task made %d calls", *c.Result, len(e.results))
		}
		v, ok := value.Lookup(e.results[i], c.Path)
		if !ok {
			return fmt.Errorf("result %d has no %q", *c.Result, c.Path)
		}
		got = v
	default:
		return errors.New("a check needs sql or result")
	}
	return compare(c, got)
}

func compare(c Check, got any) error {
	if c.Len != nil {
		n := -1
		switch x := got.(type) {
		case []any:
			n = len(x)
		case string:
			n = strings.Count(strings.TrimRight(x, "\n"), "\n")
		}
		if n != *c.Len {
			return fmt.Errorf("length %d, want %d", n, *c.Len)
		}
	}
	if c.Equals != nil && value.Text(got) != value.Text(c.Equals) {
		return fmt.Errorf("got %s, want %s", value.Text(got), value.Text(c.Equals))
	}
	if c.Gte != nil || c.Lte != nil {
		f, err := strconv.ParseFloat(value.Text(got), 64)
		if err != nil {
			return fmt.Errorf("got %s, want a number", value.Text(got))
		}
		if c.Gte != nil && f < *c.Gte-1e-9 {
			return fmt.Errorf("got %s, want >= %s", value.Text(got), strconv.FormatFloat(*c.Gte, 'f', -1, 64))
		}
		if c.Lte != nil && f > *c.Lte+1e-9 {
			return fmt.Errorf("got %s, want <= %s", value.Text(got), strconv.FormatFloat(*c.Lte, 'f', -1, 64))
		}
	}
	return nil
}

// Ratio is a/b rounded to one decimal (0 when b is 0).
func Ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return math.Round(float64(a)/float64(b)*10) / 10
}
