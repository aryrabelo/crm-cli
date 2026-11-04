package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jdanielnd/crm-cli/internal/model"
)

// TaskClaimRepo holds and releases tasks for parallel agents (fork: task
// claims). Every write is one autocommit statement: it takes the write
// lock before it reads, so concurrent processes wait on busy_timeout and
// then see the committed state. A statement that changes no row means a
// precondition failed; the task is then read once to explain why.
type TaskClaimRepo struct {
	db DBTX
}

// NewTaskClaimRepo creates a new TaskClaimRepo.
func NewTaskClaimRepo(db DBTX) *TaskClaimRepo {
	return &TaskClaimRepo{db: db}
}

// claimUpdatedLayout is the layout of claim_updated_at (UTC, seconds).
const claimUpdatedLayout = "2006-01-02T15:04:05Z"

const claimSelectColumns = `c.state, c.holder, c.previous_holder, c.updated_at, c.note, c.link`

// claimCols receives the nullable claim columns of a LEFT JOIN.
type claimCols struct {
	state, holder, previous, note, link sql.NullString
	updatedAt                           sql.NullInt64
}

func (c *claimCols) dest() []any {
	return []any{&c.state, &c.holder, &c.previous, &c.updatedAt, &c.note, &c.link}
}

// claim builds the claim of the scanned row; nil when the task has none.
func (c *claimCols) claim(now time.Time) *model.Claim {
	if !c.state.Valid {
		return nil
	}
	updated := time.UnixMilli(c.updatedAt.Int64)
	idle := now.Sub(updated)
	return &model.Claim{
		State:          c.state.String,
		Holder:         nullStr(c.holder),
		PreviousHolder: nullStr(c.previous),
		Note:           nullStr(c.note),
		Link:           nullStr(c.link),
		UpdatedAt:      updated.UTC().Format(claimUpdatedLayout),
		Idle:           model.FormatIdle(idle),
		IdleFor:        idle,
	}
}

func nullStr(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	v := s.String
	return &v
}

// extraScan lets scanTask read a row that carries more columns after the
// task's: they land in extra.
type extraScan struct {
	row   interface{ Scan(...any) error }
	extra []any
}

func (e extraScan) Scan(dest ...any) error { return e.row.Scan(append(dest, e.extra...)...) }

func checkLocator(as string) error {
	if !model.ValidLocator(as) {
		return model.NewExitError(model.ErrValidation,
			"invalid locator %q: want <runtime>:<where>:<session> (at least three non-empty colon-separated segments, at most %d bytes, no control characters)",
			as, model.MaxLocatorBytes)
	}
	return nil
}

// load reads task id with its claim, archived and completed tasks included.
func (r *TaskClaimRepo) load(ctx context.Context, id int64, now time.Time) (*model.Task, *model.Claim, error) {
	var cc claimCols
	t, err := scanTask(extraScan{r.db.QueryRowContext(ctx,
		`SELECT `+taskSelectColumns+`, `+claimSelectColumns+`
		 FROM tasks t LEFT JOIN task_claims c ON c.task_id = t.id
		 WHERE t.id = ?`, id), cc.dest()})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, model.NewExitError(model.ErrNotFound, "task %d not found", id)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read task %d claim: %w", id, err)
	}
	return t, cc.claim(now), nil
}

// loadOpen is load, with exit 3 for a completed or archived task.
func (r *TaskClaimRepo) loadOpen(ctx context.Context, id int64, now time.Time) (*model.Task, *model.Claim, error) {
	t, c, err := r.load(ctx, id, now)
	if err != nil {
		return nil, nil, err
	}
	switch {
	case t.Archived:
		return nil, nil, model.NewExitError(model.ErrNotFound, "task %d not found (deleted)", id)
	case t.Completed:
		return nil, nil, model.NewExitError(model.ErrNotFound, "task %d is already completed", id)
	}
	return t, c, nil
}

// heldBy returns the id of the task as holds, or 0.
func (r *TaskClaimRepo) heldBy(ctx context.Context, as string) (int64, error) {
	var id int64
	err := r.db.QueryRowContext(ctx,
		"SELECT task_id FROM task_claims WHERE holder = ? AND state = 'held'", as).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read hold of %s: %w", as, err)
	}
	return id, nil
}

func describe(c *model.Claim) string {
	switch {
	case c == nil || c.State == model.ClaimFree:
		return "free"
	case c.State == model.ClaimWaiting:
		return fmt.Sprintf("waiting on the owner (link %s, idle %s)", deref(c.Link), c.Idle)
	default:
		s := fmt.Sprintf("held by %s (idle %s", deref(c.Holder), c.Idle)
		if c.Note != nil {
			s += ", note: " + *c.Note
		}
		return s + ")"
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func affected(res sql.Result, what string) (bool, error) {
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("%s: %w", what, err)
	}
	return n == 1, nil
}

// Next holds the most urgent free task due by today (local date) or
// undated for as. A locator that already holds a task gets that task back
// (a retried next after a lost response), unchanged.
func (r *TaskClaimRepo) Next(ctx context.Context, as string, now time.Time, today string) (*model.Task, *model.Claim, error) {
	if err := checkLocator(as); err != nil {
		return nil, nil, err
	}
	var id int64
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO task_claims (task_id, state, holder, updated_at)
		 SELECT t.id, 'held', ?, ?
		 FROM tasks t LEFT JOIN task_claims c ON c.task_id = t.id
		 WHERE t.completed = 0 AND t.archived = 0
		   AND (c.task_id IS NULL OR c.state = 'free')
		   AND (date(t.due_at) IS NULL OR date(t.due_at) <= ?)
		   AND NOT EXISTS (SELECT 1 FROM task_claims h WHERE h.holder = ? AND h.state = 'held')
		 ORDER BY `+taskUrgencyOrder+`
		 LIMIT 1
		 ON CONFLICT (task_id) DO UPDATE
		   SET state = 'held', holder = excluded.holder, updated_at = excluded.updated_at
		   WHERE task_claims.state = 'free'
		 RETURNING task_id`,
		as, now.UnixMilli(), today, as).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if id, err = r.heldBy(ctx, as); err != nil {
			return nil, nil, err
		}
		if id == 0 {
			return nil, nil, model.NewExitError(model.ErrNotFound,
				"no task to pick: every open task is done, held, waiting or due after %s", today)
		}
	case err != nil:
		return nil, nil, fmt.Errorf("pick next task: %w", err)
	}
	return r.load(ctx, id, now)
}

// Claim holds task id for as: an open task that is free (or has no claim)
// while as holds nothing. A locator that already holds this task gets it
// back, unchanged. A concurrent release can free the task between the
// failed statement and the read that explains it; the claim then runs
// again, 3 tries in all, like run.Store.Claim.
func (r *TaskClaimRepo) Claim(ctx context.Context, id int64, as string, now time.Time) (*model.Task, *model.Claim, error) {
	if err := checkLocator(as); err != nil {
		return nil, nil, err
	}
	for range 3 {
		res, err := r.db.ExecContext(ctx,
			`INSERT INTO task_claims (task_id, state, holder, updated_at)
			 SELECT t.id, 'held', ?, ? FROM tasks t
			 WHERE t.id = ? AND t.completed = 0 AND t.archived = 0
			   AND NOT EXISTS (SELECT 1 FROM task_claims h WHERE h.holder = ? AND h.state = 'held')
			 ON CONFLICT (task_id) DO UPDATE
			   SET state = 'held', holder = excluded.holder, updated_at = excluded.updated_at
			   WHERE task_claims.state = 'free'`,
			as, now.UnixMilli(), id, as)
		if err != nil {
			return nil, nil, fmt.Errorf("claim task %d: %w", id, err)
		}
		if ok, err := affected(res, fmt.Sprintf("claim task %d", id)); err != nil || ok {
			if err != nil {
				return nil, nil, err
			}
			return r.load(ctx, id, now)
		}
		t, c, err := r.loadOpen(ctx, id, now)
		if err != nil {
			return nil, nil, err
		}
		if c != nil && c.State == model.ClaimHeld && deref(c.Holder) == as {
			return t, c, nil
		}
		if c != nil && c.State != model.ClaimFree {
			hint := "pick another task"
			if c.State == model.ClaimHeld {
				hint += fmt.Sprintf(", or take it over with --take-over once it has been idle for %s", model.FormatIdle(model.StaleAfter))
			}
			return nil, nil, model.NewExitError(model.ErrConflict, "task %d is %s; %s", id, describe(c), hint)
		}
		other, err := r.heldBy(ctx, as)
		if err != nil {
			return nil, nil, err
		}
		if other != 0 {
			return nil, nil, alreadyHolds(as, other)
		}
	}
	return nil, nil, model.NewExitError(model.ErrConflict, "task %d changed while it was being claimed; run the claim again", id)
}

func alreadyHolds(as string, other int64) error {
	return model.NewExitError(model.ErrConflict,
		"%s already holds task %d; finish it with done, wait or release before holding another", as, other)
}

// TakeOver replaces the holder of a stale hold (idle StaleAfter or more)
// with as, keeping the old holder as previous_holder. Of two concurrent
// take-overs the first sets updated_at to now, so the second fails.
func (r *TaskClaimRepo) TakeOver(ctx context.Context, id int64, as string, now time.Time) (*model.Task, *model.Claim, error) {
	if err := checkLocator(as); err != nil {
		return nil, nil, err
	}
	res, err := r.db.ExecContext(ctx,
		`UPDATE task_claims
		 SET previous_holder = holder, holder = ?, updated_at = ?
		 WHERE task_id = ? AND state = 'held' AND updated_at <= ?
		   AND NOT EXISTS (SELECT 1 FROM task_claims h WHERE h.holder = ? AND h.state = 'held')`,
		as, now.UnixMilli(), id, now.Add(-model.StaleAfter).UnixMilli(), as)
	if err != nil {
		return nil, nil, fmt.Errorf("take over task %d: %w", id, err)
	}
	if ok, err := affected(res, fmt.Sprintf("take over task %d", id)); err != nil || ok {
		if err != nil {
			return nil, nil, err
		}
		return r.load(ctx, id, now)
	}
	t, c, err := r.loadOpen(ctx, id, now)
	if err != nil {
		return nil, nil, err
	}
	switch {
	case c == nil || c.State == model.ClaimFree:
		return nil, nil, model.NewExitError(model.ErrConflict, "task %d is free; claim it without --take-over", id)
	case c.State == model.ClaimWaiting:
		return nil, nil, model.NewExitError(model.ErrConflict,
			"task %d is %s; only a held task can be taken over (release it first)", id, describe(c))
	case deref(c.Holder) == as:
		return t, c, nil
	case c.IdleFor < model.StaleAfter:
		return nil, nil, model.NewExitError(model.ErrConflict,
			"task %d is %s; it is not stale until it has been idle for %s", id, describe(c), model.FormatIdle(model.StaleAfter))
	}
	other, err := r.heldBy(ctx, as)
	if err != nil {
		return nil, nil, err
	}
	if other != 0 {
		return nil, nil, alreadyHolds(as, other)
	}
	return nil, nil, model.NewExitError(model.ErrConflict, "task %d changed while it was being taken over; it is now %s", id, describe(c))
}

// notHolder explains a holder-only write that changed nothing.
func (r *TaskClaimRepo) notHolder(ctx context.Context, id int64, as, op string, now time.Time) error {
	_, c, err := r.loadOpen(ctx, id, now)
	if err != nil {
		return err
	}
	return model.NewExitError(model.ErrConflict, "cannot %s task %d as %s: it is %s; only its holder can", op, id, as, describe(c))
}

// Note overwrites the progress note of a hold of as and refreshes its
// idle time.
func (r *TaskClaimRepo) Note(ctx context.Context, id int64, as, note string, now time.Time) (*model.Task, *model.Claim, error) {
	if err := checkLocator(as); err != nil {
		return nil, nil, err
	}
	res, err := r.db.ExecContext(ctx,
		`UPDATE task_claims SET note = ?, updated_at = ?
		 WHERE task_id = ? AND state = 'held' AND holder = ?`,
		note, now.UnixMilli(), id, as)
	if err != nil {
		return nil, nil, fmt.Errorf("note task %d: %w", id, err)
	}
	if ok, err := affected(res, fmt.Sprintf("note task %d", id)); err != nil {
		return nil, nil, err
	} else if !ok {
		return nil, nil, r.notHolder(ctx, id, as, "note", now)
	}
	return r.load(ctx, id, now)
}

// Wait moves a hold of as to waiting on the owner, with the artifact link;
// as becomes the previous holder and is free to hold another task.
func (r *TaskClaimRepo) Wait(ctx context.Context, id int64, as, link string, note *string, now time.Time) (*model.Task, *model.Claim, error) {
	if err := checkLocator(as); err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(link) == "" {
		return nil, nil, model.NewExitError(model.ErrValidation, "wait needs a link to the artifact the owner must act on (--link)")
	}
	res, err := r.db.ExecContext(ctx,
		`UPDATE task_claims
		 SET state = 'waiting', link = ?, note = COALESCE(?, note),
		     previous_holder = holder, holder = NULL, updated_at = ?
		 WHERE task_id = ? AND state = 'held' AND holder = ?`,
		link, note, now.UnixMilli(), id, as)
	if err != nil {
		return nil, nil, fmt.Errorf("wait task %d: %w", id, err)
	}
	if ok, err := affected(res, fmt.Sprintf("wait task %d", id)); err != nil {
		return nil, nil, err
	} else if !ok {
		return nil, nil, r.notHolder(ctx, id, as, "move to waiting", now)
	}
	return r.load(ctx, id, now)
}

// Release frees a task: a held task by its holder only (as required), a
// waiting task by anyone (as may be nil). The link is cleared; a held
// task's holder becomes the previous holder.
func (r *TaskClaimRepo) Release(ctx context.Context, id int64, as *string, note *string, now time.Time) (*model.Task, *model.Claim, error) {
	if as != nil {
		if err := checkLocator(*as); err != nil {
			return nil, nil, err
		}
	}
	res, err := r.db.ExecContext(ctx,
		`UPDATE task_claims
		 SET previous_holder = CASE WHEN state = 'held' THEN holder ELSE previous_holder END,
		     state = 'free', holder = NULL, link = NULL, note = COALESCE(?, note), updated_at = ?
		 WHERE task_id = ? AND (state = 'waiting' OR (state = 'held' AND holder = ?))`,
		note, now.UnixMilli(), id, as)
	if err != nil {
		return nil, nil, fmt.Errorf("release task %d: %w", id, err)
	}
	if ok, err := affected(res, fmt.Sprintf("release task %d", id)); err != nil {
		return nil, nil, err
	} else if ok {
		return r.load(ctx, id, now)
	}
	_, c, err := r.loadOpen(ctx, id, now)
	if err != nil {
		return nil, nil, err
	}
	switch {
	case c == nil || c.State == model.ClaimFree:
		return nil, nil, model.NewExitError(model.ErrConflict, "task %d is already free", id)
	case as == nil:
		return nil, nil, model.NewExitError(model.ErrValidation,
			"task %d is %s; releasing a held task needs --as with the holder's locator", id, describe(c))
	default:
		return nil, nil, model.NewExitError(model.ErrConflict,
			"cannot release task %d as %s: it is %s; only its holder can", id, *as, describe(c))
	}
}

// forTasksChunk bounds the ids bound in one ForTasks query, well under
// SQLite's limit on bound parameters (32766).
const forTasksChunk = 500

// ForTasks returns the claims of the given tasks, keyed by task id; tasks
// without a claim are absent. Any number of ids works: they are looked up
// forTasksChunk at a time.
func (r *TaskClaimRepo) ForTasks(ctx context.Context, ids []int64, now time.Time) (map[int64]*model.Claim, error) {
	out := make(map[int64]*model.Claim, len(ids))
	for len(ids) > 0 {
		n := min(len(ids), forTasksChunk)
		if err := r.forTasks(ctx, ids[:n], now, out); err != nil {
			return nil, err
		}
		ids = ids[n:]
	}
	return out, nil
}

// forTasks adds the claims of ids (at least one) to out.
func (r *TaskClaimRepo) forTasks(ctx context.Context, ids []int64, now time.Time, out map[int64]*model.Claim) error {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT c.task_id, `+claimSelectColumns+` FROM task_claims c
		 WHERE c.task_id IN (?`+strings.Repeat(", ?", len(ids)-1)+`)`, args...)
	if err != nil {
		return fmt.Errorf("read task claims: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var cc claimCols
		if err := rows.Scan(append([]any{&id}, cc.dest()...)...); err != nil {
			return fmt.Errorf("scan task claim: %w", err)
		}
		out[id] = cc.claim(now)
	}
	return rows.Err()
}

// Stale returns the holds idle StaleAfter or longer, longest idle first.
func (r *TaskClaimRepo) Stale(ctx context.Context, now time.Time) ([]model.StaleHold, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT c.task_id, t.title, c.holder, c.updated_at, c.note
		 FROM task_claims c JOIN tasks t ON t.id = c.task_id
		 WHERE c.state = 'held' AND c.updated_at <= ?
		 ORDER BY c.updated_at ASC, c.task_id ASC`,
		now.Add(-model.StaleAfter).UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("read stale holds: %w", err)
	}
	defer rows.Close()
	out := []model.StaleHold{}
	for rows.Next() {
		var h model.StaleHold
		var updated int64
		var note sql.NullString
		if err := rows.Scan(&h.TaskID, &h.Title, &h.Holder, &updated, &note); err != nil {
			return nil, fmt.Errorf("scan stale hold: %w", err)
		}
		h.Idle = model.FormatIdle(now.Sub(time.UnixMilli(updated)))
		h.Note = nullStr(note)
		out = append(out, h)
	}
	return out, rows.Err()
}
