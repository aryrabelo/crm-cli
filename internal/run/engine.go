// Package run executes batch runs: every row × requested column goes through
// the column's provider waterfall on a worker pool, with retry/backoff on
// retryable errors, a hard budget, cache-first lookups and a per-item
// checkpoint so an interrupted or budget-stopped run can be resumed.
package run

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jdanielnd/crm-cli/internal/column"
	"github.com/jdanielnd/crm-cli/internal/model"
	"github.com/jdanielnd/crm-cli/internal/provider"
	"github.com/jdanielnd/crm-cli/internal/table"
	"github.com/jdanielnd/crm-cli/internal/waterfall"
)

// ErrBudgetExhausted halts a waterfall when the next call would break the budget.
var ErrBudgetExhausted = fmt.Errorf("budget exhausted: %w", waterfall.ErrHalt)

// Defaults for Options.
const (
	DefaultConcurrency = 4
	DefaultRetry       = 2
	DefaultBackoff     = 500 * time.Millisecond
)

// Options configures a run.
type Options struct {
	Table   string
	Columns []string
	// Concurrency is the number of rows processed in parallel.
	Concurrency int
	// Retry is how many times a retryable provider error is retried.
	Retry int
	// Backoff is the first retry delay; it doubles on every retry.
	Backoff time.Duration
	// Budget caps the total spend of the run. BudgetSet distinguishes an
	// explicit 0 from "not given".
	Budget    provider.Cost
	BudgetSet bool
	// Resume continues run Resume instead of starting a new run; only items
	// that are not done (pending, failed, skipped) are processed.
	Resume int64
	// Only restricts a new run to the rows matching every filter expression
	// (same syntax as table show --only).
	Only []string
	// Preview holds column definitions that are not saved yet; Estimate
	// treats them as if they were (a new column replaces nothing, an existing
	// name is redefined). Execute rejects it: save the columns first.
	Preview []*column.Column
	// LeaseTimeout is how long the executor's heartbeat may be silent before
	// another executor may take the run over (0: DefaultLeaseTimeout).
	LeaseTimeout time.Duration
}

// Engine runs and estimates batch runs.
type Engine struct {
	db       *sql.DB
	registry *provider.Registry
}

// NewEngine creates an Engine.
func NewEngine(db *sql.DB, registry *provider.Registry) *Engine {
	return &Engine{db: db, registry: registry}
}

type target struct {
	col *column.Column
	wf  *waterfall.Waterfall
	// dependent: the input column is computed earlier in the same run.
	dependent bool
}

type plan struct {
	ws       *table.Workspace
	columns  []*column.Column
	targets  []*target
	byColumn map[int64]*target
	rows     []*table.Row
	limiters map[string]*provider.Limiter
}

func (e *Engine) prepare(ctx context.Context, tableName string, names, only []string, preview []*column.Column) (*plan, error) {
	ts := table.NewStore(e.db)
	ws, err := ts.FindWorkspace(ctx, tableName)
	if err != nil {
		return nil, err
	}
	cols, err := ts.Columns(ctx, ws.ID)
	if err != nil {
		return nil, err
	}
	for _, pc := range preview {
		if i := slices.IndexFunc(cols, func(c *column.Column) bool { return c.Name == pc.Name }); i >= 0 {
			cols[i] = pc
		} else {
			cols = append(cols, pc)
		}
	}
	if len(names) == 0 {
		return nil, model.NewExitError(model.ErrValidation, "--columns is required (enrich or ai columns of table %s)", ws.Name)
	}
	filters, err := table.ParseFilters(only, cols)
	if err != nil {
		return nil, err
	}
	p := &plan{ws: ws, columns: cols, byColumn: map[int64]*target{}, limiters: map[string]*provider.Limiter{}}
	providers := provider.NewStore(e.db)
	cache := waterfall.NewCache(e.db)
	for i, name := range names {
		if slices.Contains(names[:i], name) {
			return nil, model.NewExitError(model.ErrValidation, "column %q is listed twice in --columns", name)
		}
		idx := slices.IndexFunc(cols, func(c *column.Column) bool { return c.Name == name })
		if idx < 0 {
			return nil, model.NewExitError(model.ErrNotFound, "table %s has no column %q", ws.Name, name)
		}
		c := cols[idx]
		if !c.Type.Runnable() {
			return nil, model.NewExitError(model.ErrValidation,
				"column %q is %s: only enrich and ai columns run (input is imported, formula is computed on read)", name, c.Type)
		}
		wf, configs, err := c.Waterfall(ctx, providers, e.registry, cache)
		if err != nil {
			return nil, err
		}
		for _, cfg := range configs {
			if _, ok := p.limiters[cfg.Name]; !ok {
				p.limiters[cfg.Name] = provider.NewLimiter(cfg.Rate)
			}
		}
		dependent := slices.ContainsFunc(c.Sources(cols), func(src string) bool { return slices.Contains(names[:i], src) })
		t := &target{col: c, wf: wf, dependent: dependent}
		p.targets = append(p.targets, t)
		p.byColumn[c.ID] = t
	}
	rows, err := ts.Rows(ctx, ws.ID)
	if err != nil {
		return nil, err
	}
	p.rows = table.NewView(cols, rows, filters).Rows
	return p, nil
}

// ColumnEstimate is the projected work for one column.
type ColumnEstimate struct {
	Column    string
	Items     int
	CacheHits int
	// NoInput counts rows whose input cell is empty (no call is made).
	NoInput int
	Calls   int
	// Expected assumes the first provider called answers; Worst assumes
	// every uncached provider of the waterfall is called.
	Expected provider.Cost
	Worst    provider.Cost
}

// Estimate is the projected work of a run, per column and in total.
type Estimate struct {
	Table   string
	RunID   int64
	Columns []*ColumnEstimate
	Total   ColumnEstimate
}

// Estimate projects calls, cost and cache hits without calling providers or
// writing anything (--dry-run). With opts.Resume only the open items count.
func (e *Engine) Estimate(ctx context.Context, opts Options) (*Estimate, error) {
	names := opts.Columns
	var open map[int64]map[int64]bool // row → column → open
	if opts.Resume != 0 {
		if len(opts.Only) > 0 || len(opts.Preview) > 0 {
			return nil, model.NewExitError(model.ErrValidation, "a resumed run keeps its rows and columns: drop --only")
		}
		r, items, err := e.openItems(ctx, opts.Resume)
		if err != nil {
			return nil, err
		}
		names = r.Columns
		opts.Table = r.Table
		open = map[int64]map[int64]bool{}
		for _, it := range items {
			if open[it.RowID] == nil {
				open[it.RowID] = map[int64]bool{}
			}
			open[it.RowID][it.ColumnID] = true
		}
	}
	p, err := e.prepare(ctx, opts.Table, names, opts.Only, opts.Preview)
	if err != nil {
		return nil, err
	}
	est, err := p.estimate(ctx, open)
	if err != nil {
		return nil, err
	}
	est.RunID = opts.Resume
	return est, nil
}

// estimate projects the work of the plan; open (row → column → open)
// restricts it to the open items of a resumed run when non-nil.
func (p *plan) estimate(ctx context.Context, open map[int64]map[int64]bool) (*Estimate, error) {
	est := &Estimate{Table: p.ws.Name}
	for _, t := range p.targets {
		est.Columns = append(est.Columns, &ColumnEstimate{Column: t.col.Name})
	}
	for _, row := range p.rows {
		cells := column.Materialize(p.columns, row.Data)
		for i, t := range p.targets {
			if open != nil && !open[row.ID][t.col.ID] {
				continue
			}
			ce := est.Columns[i]
			ce.Items++
			in := t.col.Input(cells)
			if in.Value == "" && !t.dependent {
				ce.NoInput++
				continue
			}
			we, err := t.wf.Estimate(ctx, in, t.col.Config.Field, !t.dependent)
			if err != nil {
				return nil, err
			}
			if we.CacheHit {
				ce.CacheHits++
			}
			ce.Calls += we.Calls
			ce.Expected += we.Expected
			ce.Worst += we.Worst
		}
	}
	est.Total.Column = "TOTAL"
	for _, ce := range est.Columns {
		est.Total.Items += ce.Items
		est.Total.CacheHits += ce.CacheHits
		est.Total.NoInput += ce.NoInput
		est.Total.Calls += ce.Calls
		est.Total.Expected += ce.Expected
		est.Total.Worst += ce.Worst
	}
	return est, nil
}

func (e *Engine) openItems(ctx context.Context, runID int64) (*Run, []*Item, error) {
	rs := NewStore(e.db)
	r, err := e.resumable(ctx, rs, runID)
	if err != nil {
		return nil, nil, err
	}
	items, err := rs.Items(ctx, runID, true)
	if err != nil {
		return nil, nil, err
	}
	return r, items, nil
}

// resumable loads run runID and refuses it when a column it still has work
// for was removed since (crm column rm, possibly re-added under the same
// name): those items belong to a definition that no longer exists.
func (e *Engine) resumable(ctx context.Context, rs *Store, runID int64) (*Run, error) {
	r, err := rs.Get(ctx, runID)
	if err != nil {
		return nil, err
	}
	removed, err := rs.RemovedColumns(ctx, runID)
	if err != nil {
		return nil, err
	}
	if len(removed) > 0 {
		return nil, model.NewExitError(model.ErrValidation,
			"run %d cannot be resumed: column(s) %s of table %s were removed (or removed and added again) after the run started; start a new run: crm run %s --columns %s",
			r.ID, strings.Join(removed, ", "), r.Table, r.Table, strings.Join(r.Columns, ","))
	}
	return r, nil
}

// Result is the outcome of one Execute call.
type Result struct {
	Run *Run
	// Duration is the wall time of this invocation.
	Duration time.Duration
}

// Execute starts (or resumes) a run and blocks until every open item is done,
// the budget stops it, or ctx is cancelled (status interrupted; resumable).
func (e *Engine) Execute(ctx context.Context, opts Options) (*Result, error) {
	started := time.Now()
	if opts.Concurrency <= 0 {
		opts.Concurrency = DefaultConcurrency
	}
	if opts.Retry < 0 {
		opts.Retry = 0
	}
	if opts.Backoff <= 0 {
		opts.Backoff = DefaultBackoff
	}

	if len(opts.Preview) > 0 {
		return nil, model.NewExitError(model.ErrValidation, "preview columns can only be estimated; save them before running")
	}
	if opts.Resume != 0 && len(opts.Only) > 0 {
		return nil, model.NewExitError(model.ErrValidation, "a resumed run keeps its rows and columns: drop --only")
	}
	rs := NewStore(e.db)
	var r *Run
	var p *plan
	var err error
	if opts.Resume != 0 {
		if r, err = e.resumable(ctx, rs, opts.Resume); err != nil {
			return nil, err
		}
		if p, err = e.prepare(ctx, r.Table, r.Columns, nil, nil); err != nil {
			return nil, err
		}
		if opts.BudgetSet {
			r.Budget = opts.Budget
		}
	} else if r, p, err = e.create(ctx, opts); err != nil {
		return nil, err
	}

	// One executor per run: claim the lease before reading the open items
	// and the spent total, so a concurrent executor can neither process the
	// same items nor spend the same budget.
	lease := opts.LeaseTimeout
	if lease <= 0 {
		lease = DefaultLeaseTimeout
	}
	owner := newOwner()
	if err := rs.Claim(ctx, r.ID, owner, r.Budget, time.Now(), lease); err != nil {
		return nil, err
	}
	bg := context.WithoutCancel(ctx)
	runID, finished := r.ID, false
	defer func() {
		if !finished {
			_ = rs.Release(bg, runID, owner)
		}
	}()
	// Re-read the run under the lease: spent and stats may have changed
	// since it was loaded (a previous executor finished in between).
	if r, err = rs.Get(ctx, runID); err != nil {
		return nil, err
	}
	previous := r.Stats.DurationMS
	items, err := rs.Items(ctx, r.ID, true)
	if err != nil {
		return nil, err
	}
	jobs := groupByRow(p, items)

	w := &worker{
		engine: e,
		plan:   p,
		opts:   opts,
		budget: newBudget(r.Budget, r.Spent),
		runID:  r.ID,
		owner:  owner,
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var fatalOnce sync.Once
	var fatal error
	fail := func(err error) { fatalOnce.Do(func() { fatal = err; cancel() }) }

	beatDone := make(chan struct{})
	go func() {
		defer close(beatDone)
		tick := time.NewTicker(lease / 3)
		defer tick.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case now := <-tick.C:
				if err := rs.Heartbeat(bg, r.ID, owner, now); errors.Is(err, ErrLeaseLost) {
					fail(err)
					return
				}
			}
		}
	}()

	queue := make(chan *rowJob)
	var wg sync.WaitGroup
	for range opts.Concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range queue {
				if err := w.process(runCtx, job); err != nil {
					fail(err)
				}
			}
		}()
	}
feed:
	for _, job := range jobs {
		select {
		case queue <- job:
		case <-runCtx.Done():
			break feed
		}
	}
	close(queue)
	wg.Wait()
	cancel()
	<-beatDone

	if errors.Is(fatal, ErrLeaseLost) {
		finished = true // the new holder owns the run's state
		return nil, fatal
	}
	// Final bookkeeping must land even when the run was interrupted.
	st, err := rs.Stats(bg, r.ID)
	if err != nil {
		return nil, err
	}
	elapsed := time.Since(started)
	st.DurationMS = previous + elapsed.Milliseconds()
	status := StatusCompleted
	switch {
	case ctx.Err() != nil || fatal != nil || st.Pending > 0:
		status = StatusInterrupted
	case st.Skipped > 0:
		status = StatusBudgetExhausted
	}
	if err := rs.Finish(bg, r.ID, owner, status, st); err != nil {
		return nil, err
	}
	finished = true
	if fatal != nil {
		return nil, fatal
	}
	final, err := rs.Get(bg, r.ID)
	if err != nil {
		return nil, err
	}
	return &Result{Run: final, Duration: elapsed}, nil
}

func (e *Engine) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// create prepares a new run, refuses a run that may spend money without a
// budget, and inserts the run with one pending item per row × column.
func (e *Engine) create(ctx context.Context, opts Options) (*Run, *plan, error) {
	p, err := e.prepare(ctx, opts.Table, opts.Columns, opts.Only, nil)
	if err != nil {
		return nil, nil, err
	}
	est, err := p.estimate(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	if !opts.BudgetSet && est.Total.Worst > 0 {
		return nil, nil, model.NewExitError(model.ErrValidation,
			"this run may spend up to %s (expected %s); set --budget USD (preview with --dry-run)",
			est.Total.Worst, est.Total.Expected)
	}
	r := &Run{WorkspaceID: p.ws.ID, Table: p.ws.Name, Columns: opts.Columns, Budget: opts.Budget}
	colIDs := make([]int64, len(p.targets))
	for i, t := range p.targets {
		colIDs[i] = t.col.ID
	}
	if err := e.inTx(ctx, func(tx *sql.Tx) error { return NewStore(tx).Create(ctx, r, p.rows, colIDs) }); err != nil {
		return nil, nil, err
	}
	return r, p, nil
}

// Start creates a new run without processing it: every item is pending and
// the run is "running". Execute with Options.Resume set to the run's ID
// processes it (typically on another goroutine, while callers poll the store).
func (e *Engine) Start(ctx context.Context, opts Options) (*Run, error) {
	if opts.Resume != 0 || len(opts.Preview) > 0 {
		return nil, model.NewExitError(model.ErrValidation, "Start creates a new run: drop the resume run and preview columns")
	}
	r, _, err := e.create(ctx, opts)
	return r, err
}

type rowJob struct {
	row   *table.Row
	items []*Item
}

// groupByRow orders open items by row, and within a row by --columns order,
// so a column can use a value computed earlier in the same row.
func groupByRow(p *plan, items []*Item) []*rowJob {
	rows := make(map[int64]*table.Row, len(p.rows))
	for _, r := range p.rows {
		rows[r.ID] = r
	}
	order := make(map[int64]int, len(p.targets))
	for i, t := range p.targets {
		order[t.col.ID] = i
	}
	var jobs []*rowJob
	byRow := map[int64]*rowJob{}
	for _, it := range items {
		j := byRow[it.RowID]
		if j == nil {
			j = &rowJob{row: rows[it.RowID]}
			byRow[it.RowID] = j
			jobs = append(jobs, j)
		}
		j.items = append(j.items, it)
	}
	for _, j := range jobs {
		slices.SortFunc(j.items, func(a, b *Item) int { return order[a.ColumnID] - order[b.ColumnID] })
	}
	return jobs
}

type worker struct {
	engine *Engine
	plan   *plan
	opts   Options
	budget *budget
	// runID and owner identify the lease every checkpoint renews.
	runID int64
	owner string
}

// call is the waterfall CallFunc: reserve budget, respect the provider's
// rate, call, settle, and retry retryable errors with exponential backoff.
func (w *worker) call(ctx context.Context, step waterfall.Step, in provider.Input) (provider.Output, provider.Cost, int, error) {
	est := step.Adapter.Estimate(in)
	var total provider.Cost
	attempts := 0
	for {
		if !w.budget.reserve(est) {
			return nil, total, attempts, ErrBudgetExhausted
		}
		if err := w.plan.limiters[step.Name].Wait(ctx); err != nil {
			w.budget.settle(est, 0)
			return nil, total, attempts, err
		}
		out, cost, err := step.Adapter.Enrich(ctx, in)
		attempts++
		w.budget.settle(est, cost)
		total += cost
		if err == nil {
			return out, total, attempts, nil
		}
		if ctx.Err() != nil {
			return nil, total, attempts, ctx.Err()
		}
		retry, hint := provider.Retryable(err)
		if !retry || attempts > w.opts.Retry {
			return nil, total, attempts, err
		}
		if err := sleep(ctx, backoff(w.opts.Backoff, attempts-1, hint)); err != nil {
			return nil, total, attempts, err
		}
	}
}

// process runs the open items of one row in column order and checkpoints
// each. It returns an error only for database failures.
func (w *worker) process(ctx context.Context, job *rowJob) error {
	data := make(map[string]any, len(job.row.Data))
	for k, v := range job.row.Data {
		data[k] = v
	}
	for _, it := range job.items {
		if ctx.Err() != nil {
			return nil
		}
		t := w.plan.byColumn[it.ColumnID]
		if t == nil {
			// Not a column of this plan (removed since the run started;
			// resumable refuses such runs). Leave the item untouched.
			continue
		}
		start := time.Now()
		in := t.col.Input(column.Materialize(w.plan.columns, data))

		it.Attempts, it.Calls, it.Cost, it.CacheHit = 0, 0, 0, false
		it.TokensIn, it.TokensOut = 0, 0
		it.Value, it.Provider, it.Error = nil, nil, nil
		var cells map[string]any // nil: no cell is written
		if in.Value == "" {
			it.Status = ItemEmpty
			cells = t.col.Cells(nil)
		} else {
			callCtx, usage := provider.WithUsage(ctx)
			res, err := t.wf.Run(callCtx, in, t.col.Config.Field, w.call)
			it.TokensIn, it.TokensOut = usage.Tokens()
			it.Attempts, it.Calls, it.Cost, it.CacheHit = res.Attempts, res.Calls, res.Cost, res.CacheHit
			switch {
			case errors.Is(err, ErrBudgetExhausted):
				it.Status = ItemSkipped
				it.Error = ptr("budget exhausted")
			case err != nil && ctx.Err() != nil:
				it.Status = ItemPending
			case err != nil:
				it.Status = ItemFailed
				it.Error = ptr(provider.RedactError(err.Error()))
			case res.Answered():
				enc, err := json.Marshal(res.Value)
				if err != nil {
					return fmt.Errorf("encode value: %w", err)
				}
				it.Status = ItemOK
				it.Value = ptr(string(enc))
				it.Provider = ptr(res.Provider)
				cells = t.col.Cells(res.Value)
			case len(res.Errors) > 0:
				it.Status = ItemFailed
				it.Error = ptr(provider.RedactError(strings.Join(res.Errors, "; ")))
			default:
				it.Status = ItemEmpty
				cells = t.col.Cells(nil)
			}
		}
		it.MS = time.Since(start).Milliseconds()

		bg := context.WithoutCancel(ctx)
		err := w.engine.inTx(bg, func(tx *sql.Tx) error {
			// The checkpoint renews the lease and lands only while this
			// executor still holds the run, so a taken-over run is never
			// written (or its spend counted) twice.
			rs := NewStore(tx)
			if err := rs.Heartbeat(bg, w.runID, w.owner, time.Now()); err != nil {
				return err
			}
			if err := rs.SaveItem(bg, it); err != nil {
				return err
			}
			if cells != nil {
				return table.NewStore(tx).SetCells(bg, job.row.ID, cells)
			}
			return nil
		})
		if err != nil {
			return err
		}
		for name, v := range cells {
			data[name] = v
		}
	}
	return nil
}

func ptr(s string) *string { return &s }
