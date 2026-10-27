package run

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jdanielnd/crm-cli/internal/model"
	"github.com/jdanielnd/crm-cli/internal/provider"
)

// DefaultLeaseTimeout is how long a run's heartbeat may be silent before
// another executor can take the run over. The holder renews it every
// LeaseTimeout/3 and on every checkpoint.
const DefaultLeaseTimeout = 30 * time.Second

// ErrLeaseLost means this executor no longer holds the run: its heartbeat
// went stale and another executor took the run over.
var ErrLeaseLost = errors.New("run lease lost")

// newOwner identifies one executor: host, process and a per-Execute nonce
// (two Execute calls in one process are different executors).
func newOwner() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("%s:%d:%s", host, os.Getpid(), uuid.NewString()[:8])
}

func unixMS(t time.Time) int64 { return t.UnixMilli() }

// Claim makes owner the only executor of run runID: it sets the budget,
// marks the run running and records the heartbeat, but only when nobody
// holds the run or the holder's heartbeat is older than stale. Exactly one
// of several concurrent claims succeeds; the others get a conflict (exit 4)
// naming the holder.
func (s *Store) Claim(ctx context.Context, runID int64, owner string, budget provider.Cost, now time.Time, stale time.Duration) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE runs SET owner = ?, heartbeat_at = ?, budget_micros = ?, status = ?, finished_at = NULL
		 WHERE id = ? AND (owner IS NULL OR heartbeat_at IS NULL OR heartbeat_at < ?)`,
		owner, unixMS(now), budget, StatusRunning, runID, unixMS(now.Add(-stale)))
	if err != nil {
		return fmt.Errorf("claim run %d: %w", runID, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("claim run %d: %w", runID, err)
	} else if n == 1 {
		return nil
	}
	var holder sql.NullString
	var beat sql.NullInt64
	err = s.db.QueryRowContext(ctx, "SELECT owner, heartbeat_at FROM runs WHERE id = ?", runID).Scan(&holder, &beat)
	if errors.Is(err, sql.ErrNoRows) {
		return model.NewExitError(model.ErrNotFound, "run %d not found", runID)
	}
	if err != nil {
		return fmt.Errorf("read run %d lease: %w", runID, err)
	}
	age := now.Sub(time.UnixMilli(beat.Int64)).Round(time.Second)
	return model.NewExitError(model.ErrConflict,
		"run %d is being executed by %s (last heartbeat %s ago); wait for it to finish, or retry once it has been silent for %s",
		runID, holder.String, age, stale)
}

// Heartbeat renews owner's lease on runID. It fails with ErrLeaseLost when
// another executor took the run over.
func (s *Store) Heartbeat(ctx context.Context, runID int64, owner string, now time.Time) error {
	res, err := s.db.ExecContext(ctx,
		"UPDATE runs SET heartbeat_at = ? WHERE id = ? AND owner = ?", unixMS(now), runID, owner)
	if err != nil {
		return fmt.Errorf("heartbeat run %d: %w", runID, err)
	}
	return oneRow(res, runID)
}

// Release gives up owner's lease without finishing the run (status
// interrupted, resumable at once).
func (s *Store) Release(ctx context.Context, runID int64, owner string) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE runs SET owner = NULL, heartbeat_at = NULL, status = ?, finished_at = datetime('now')
		 WHERE id = ? AND owner = ?`, StatusInterrupted, runID, owner); err != nil {
		return fmt.Errorf("release run %d: %w", runID, err)
	}
	return nil
}

func oneRow(res sql.Result, runID int64) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("run %d: %w", runID, err)
	}
	if n != 1 {
		return fmt.Errorf("run %d: %w (another executor took it over)", runID, ErrLeaseLost)
	}
	return nil
}
