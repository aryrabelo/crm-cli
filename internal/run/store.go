package run

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jdanielnd/crm-cli/internal/db/repo"
	"github.com/jdanielnd/crm-cli/internal/model"
	"github.com/jdanielnd/crm-cli/internal/provider"
	"github.com/jdanielnd/crm-cli/internal/table"
)

// Status is the state of a run.
type Status string

const (
	StatusRunning         Status = "running"
	StatusCompleted       Status = "completed"
	StatusBudgetExhausted Status = "budget_exhausted"
	StatusInterrupted     Status = "interrupted"
)

// Item statuses. ok and empty are done; pending, failed and skipped are
// picked up again by --resume.
const (
	ItemPending = "pending"
	ItemOK      = "ok"
	ItemEmpty   = "empty"
	ItemFailed  = "failed"
	ItemSkipped = "skipped"
)

// Stats aggregates the items of a run.
type Stats struct {
	Items     int   `json:"items"`
	OK        int   `json:"ok"`
	Empty     int   `json:"empty"`
	Failed    int   `json:"failed"`
	Skipped   int   `json:"skipped"`
	Pending   int   `json:"pending"`
	CacheHits int   `json:"cache_hits"`
	Calls     int   `json:"calls"`
	TokensIn  int64 `json:"tokens_in"`
	TokensOut int64 `json:"tokens_out"`
	// Uncertain counts the items with an unconfirmed cost; UncertainMicros
	// is that cost (included in the run's spent total).
	Uncertain       int            `json:"uncertain,omitempty"`
	UncertainMicros int64          `json:"uncertain_micros,omitempty"`
	DurationMS      int64          `json:"duration_ms"`
	Providers       map[string]int `json:"providers"`
}

// Run is one batch execution over rows × columns.
type Run struct {
	ID          int64         `json:"id"`
	UUID        string        `json:"uuid"`
	WorkspaceID int64         `json:"workspace_id"`
	Table       string        `json:"table"`
	Status      Status        `json:"status"`
	Columns     []string      `json:"columns"`
	Budget      provider.Cost `json:"-"`
	Spent       provider.Cost `json:"-"`
	Stats       Stats         `json:"stats"`
	StartedAt   string        `json:"started_at"`
	FinishedAt  *string       `json:"finished_at"`
}

// Item is one row × one column of a run.
type Item struct {
	ID          int64         `json:"id"`
	RunID       int64         `json:"run_id"`
	RowID       int64         `json:"row_id"`
	RowPosition int           `json:"row"`
	ColumnID    int64         `json:"column_id"`
	Column      string        `json:"column"`
	Status      string        `json:"status"`
	Attempts    int           `json:"attempts"`
	Calls       int           `json:"calls"`
	Value       *string       `json:"value"`
	Provider    *string       `json:"provider"`
	CacheHit    bool          `json:"cache_hit"`
	Error       *string       `json:"error"`
	MS          int64         `json:"ms"`
	Cost        provider.Cost `json:"-"`
	// Uncertain is the part of Cost that is not confirmed: calls that got no
	// response after the request was sent, charged at their estimate.
	Uncertain provider.Cost `json:"-"`
	// TokensIn and TokensOut are the llm tokens of the calls made (ai columns).
	TokensIn  int64 `json:"tokens_in"`
	TokensOut int64 `json:"tokens_out"`
}

// Store persists runs and run items.
type Store struct {
	db repo.DBTX
}

// NewStore creates a run Store.
func NewStore(db repo.DBTX) *Store { return &Store{db: db} }

const runColumns = `r.id, r.uuid, r.workspace_id, w.name, r.status, r.columns, r.budget_micros, r.spent_micros,
	r.stats, r.started_at, r.finished_at`

func scanRun(scan func(...any) error) (*Run, error) {
	var r Run
	var cols, stats string
	if err := scan(&r.ID, &r.UUID, &r.WorkspaceID, &r.Table, &r.Status, &cols, &r.Budget, &r.Spent,
		&stats, &r.StartedAt, &r.FinishedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(cols), &r.Columns); err != nil {
		return nil, fmt.Errorf("run %d: decode columns: %w", r.ID, err)
	}
	if err := json.Unmarshal([]byte(stats), &r.Stats); err != nil {
		return nil, fmt.Errorf("run %d: decode stats: %w", r.ID, err)
	}
	return &r, nil
}

// Create inserts a run and one pending item per row × column.
func (s *Store) Create(ctx context.Context, r *Run, rows []*table.Row, columnIDs []int64) error {
	cols, err := json.Marshal(r.Columns)
	if err != nil {
		return fmt.Errorf("encode run columns: %w", err)
	}
	r.UUID = uuid.New().String()
	r.Status = StatusRunning
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO runs (uuid, workspace_id, status, columns, budget_micros) VALUES (?, ?, ?, ?, ?)",
		r.UUID, r.WorkspaceID, r.Status, string(cols), r.Budget)
	if err != nil {
		return fmt.Errorf("create run: %w", err)
	}
	if r.ID, err = res.LastInsertId(); err != nil {
		return fmt.Errorf("last insert id: %w", err)
	}
	for _, row := range rows {
		for _, colID := range columnIDs {
			if _, err := s.db.ExecContext(ctx,
				"INSERT INTO run_items (run_id, row_id, column_id) VALUES (?, ?, ?)", r.ID, row.ID, colID); err != nil {
				return fmt.Errorf("create run item: %w", err)
			}
		}
	}
	return nil
}

// Get returns run id (exit code 3 when missing).
func (s *Store) Get(ctx context.Context, id int64) (*Run, error) {
	r, err := scanRun(s.db.QueryRowContext(ctx,
		"SELECT "+runColumns+" FROM runs r JOIN workspaces w ON w.id = r.workspace_id WHERE r.id = ?", id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, model.NewExitError(model.ErrNotFound, "run %d not found", id)
	}
	if err != nil {
		return nil, fmt.Errorf("get run %d: %w", id, err)
	}
	return r, nil
}

// List returns runs, newest first.
func (s *Store) List(ctx context.Context) ([]*Run, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+runColumns+" FROM runs r JOIN workspaces w ON w.id = r.workspace_id ORDER BY r.id DESC")
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close()
	var out []*Run
	for rows.Next() {
		r, err := scanRun(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate runs: %w", err)
	}
	return out, nil
}

// Items returns the items of a run ordered by row position, then column
// order. With openOnly, only items that still need work (pending, failed,
// skipped) of columns that still exist are returned; RemovedColumns lists
// the columns whose open items were left behind.
func (s *Store) Items(ctx context.Context, runID int64, openOnly bool) ([]*Item, error) {
	q := `SELECT i.id, i.run_id, i.row_id, wr.position, i.column_id, c.name, i.status, i.attempts, i.calls,
		i.value, i.provider, i.cache_hit, i.error, i.ms, i.cost_micros, i.uncertain_micros, i.tokens_in, i.tokens_out
		FROM run_items i
		JOIN workspace_rows wr ON wr.id = i.row_id
		JOIN workspace_columns c ON c.id = i.column_id
		WHERE i.run_id = ?`
	if openOnly {
		q += " AND i.status NOT IN ('ok', 'empty') AND c.archived = 0"
	}
	q += " ORDER BY wr.position, i.id"
	rows, err := s.db.QueryContext(ctx, q, runID)
	if err != nil {
		return nil, fmt.Errorf("list run items: %w", err)
	}
	defer rows.Close()
	var out []*Item
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.ID, &it.RunID, &it.RowID, &it.RowPosition, &it.ColumnID, &it.Column, &it.Status,
			&it.Attempts, &it.Calls, &it.Value, &it.Provider, &it.CacheHit, &it.Error, &it.MS, &it.Cost, &it.Uncertain,
			&it.TokensIn, &it.TokensOut); err != nil {
			return nil, fmt.Errorf("scan run item: %w", err)
		}
		out = append(out, &it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate run items: %w", err)
	}
	return out, nil
}

// RemovedColumns returns the names of the columns of run runID that have
// open items but were removed (crm column rm) since the run started, even
// when a column with the same name was added again.
func (s *Store) RemovedColumns(ctx context.Context, runID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT c.name FROM run_items i JOIN workspace_columns c ON c.id = i.column_id
		 WHERE i.run_id = ? AND c.archived = 1 AND i.status NOT IN ('ok', 'empty') ORDER BY c.name`, runID)
	if err != nil {
		return nil, fmt.Errorf("run %d removed columns: %w", runID, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan run column: %w", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate run columns: %w", err)
	}
	return out, nil
}

// SaveItem checkpoints one item and adds its cost to the run's spent total.
// Run it inside a transaction together with the row cell update.
func (s *Store) SaveItem(ctx context.Context, it *Item) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE run_items SET status = ?, attempts = attempts + ?, calls = calls + ?, value = ?, provider = ?,
		   cache_hit = ?, error = ?, ms = ?, cost_micros = cost_micros + ?, uncertain_micros = uncertain_micros + ?,
		   tokens_in = tokens_in + ?, tokens_out = tokens_out + ?, updated_at = datetime('now')
		 WHERE id = ?`,
		it.Status, it.Attempts, it.Calls, it.Value, it.Provider, it.CacheHit, it.Error, it.MS, it.Cost, it.Uncertain,
		it.TokensIn, it.TokensOut, it.ID); err != nil {
		return fmt.Errorf("save run item %d: %w", it.ID, err)
	}
	if it.Cost != 0 {
		if _, err := s.db.ExecContext(ctx,
			"UPDATE runs SET spent_micros = spent_micros + ? WHERE id = ?", it.Cost, it.RunID); err != nil {
			return fmt.Errorf("update run %d spent: %w", it.RunID, err)
		}
	}
	return nil
}

// Stats aggregates the current item states of a run (duration is not
// derived from items; callers carry it over).
func (s *Store) Stats(ctx context.Context, runID int64) (Stats, error) {
	st := Stats{Providers: map[string]int{}}
	rows, err := s.db.QueryContext(ctx,
		`SELECT status, COALESCE(provider, ''), COUNT(*), SUM(cache_hit), SUM(calls), SUM(tokens_in), SUM(tokens_out)
		 FROM run_items WHERE run_id = ? GROUP BY status, provider`, runID)
	if err != nil {
		return st, fmt.Errorf("run %d stats: %w", runID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var status, prov string
		var n, hits, calls int
		var tokIn, tokOut int64
		if err := rows.Scan(&status, &prov, &n, &hits, &calls, &tokIn, &tokOut); err != nil {
			return st, fmt.Errorf("scan run stats: %w", err)
		}
		st.Items += n
		st.CacheHits += hits
		st.Calls += calls
		st.TokensIn += tokIn
		st.TokensOut += tokOut
		switch status {
		case ItemOK:
			st.OK += n
		case ItemEmpty:
			st.Empty += n
		case ItemFailed:
			st.Failed += n
		case ItemSkipped:
			st.Skipped += n
		default:
			st.Pending += n
		}
		if prov != "" {
			st.Providers[prov] += n
		}
	}
	if err := rows.Err(); err != nil {
		return st, fmt.Errorf("iterate run stats: %w", err)
	}
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*), COALESCE(SUM(uncertain_micros), 0) FROM run_items WHERE run_id = ? AND uncertain_micros > 0",
		runID).Scan(&st.Uncertain, &st.UncertainMicros); err != nil {
		return st, fmt.Errorf("run %d uncertain cost: %w", runID, err)
	}
	return st, nil
}

// Finish records the final status and stats of a run invocation and
// releases owner's lease. It fails with ErrLeaseLost when owner no longer
// holds the run (another executor took it over).
func (s *Store) Finish(ctx context.Context, runID int64, owner string, status Status, st Stats) error {
	enc, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("encode run stats: %w", err)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status = ?, stats = ?, finished_at = datetime('now'), owner = NULL, heartbeat_at = NULL
		 WHERE id = ? AND owner = ?`,
		status, string(enc), runID, owner)
	if err != nil {
		return fmt.Errorf("finish run %d: %w", runID, err)
	}
	return oneRow(res, runID)
}
