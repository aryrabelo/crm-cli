package repo_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/jdanielnd/crm-cli/internal/db"
	"github.com/jdanielnd/crm-cli/internal/db/repo"
	"github.com/jdanielnd/crm-cli/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	claimNow   = time.Date(2026, 11, 3, 12, 0, 0, 0, time.Local)
	claimToday = "2026-11-03"
)

const (
	locA = "test:repo:a"
	locB = "test:repo:b"
	locC = "test:repo:c"
)

type claimFixture struct {
	db     *sql.DB
	tasks  *repo.TaskRepo
	claims *repo.TaskClaimRepo
}

func newClaimFixture(t *testing.T) *claimFixture {
	t.Helper()
	d, err := db.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { d.Close() })
	return &claimFixture{db: d, tasks: repo.NewTaskRepo(d), claims: repo.NewTaskClaimRepo(d)}
}

// add creates a task; due "" means undated.
func (f *claimFixture) add(t *testing.T, title, due, priority string) int64 {
	t.Helper()
	in := model.CreateTaskInput{Title: title, Priority: priority}
	if due != "" {
		in.DueAt = &due
	}
	task, err := f.tasks.Create(context.Background(), in)
	require.NoError(t, err)
	return task.ID
}

func (f *claimFixture) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	_, err := f.db.Exec(q, args...)
	require.NoError(t, err, q)
}

func exitIs(t *testing.T, err error, sentinel error) {
	t.Helper()
	require.Error(t, err)
	assert.True(t, errors.Is(err, sentinel), "want %v, got %v", sentinel, err)
}

func strp(s string) *string { return &s }

func TestTaskList_UrgencyOrder(t *testing.T) {
	f := newClaimFixture(t)
	undated := f.add(t, "undated", "", "high")
	garbage := f.add(t, "unparseable", "someday", "high")
	todayLow := f.add(t, "today low", "2026-11-03", "low")
	todayHighNew := f.add(t, "today high new", "2026-11-03T18:00:00", "high")
	todayHighOld := f.add(t, "today high old", "2026-11-03", "high")
	overdue := f.add(t, "overdue", "2026-10-30", "low")
	tomorrow := f.add(t, "tomorrow", "2026-11-04", "high")
	f.exec(t, "UPDATE tasks SET created_at = '2026-01-01 00:00:00' WHERE id = ?", todayHighOld)
	f.exec(t, "UPDATE tasks SET created_at = '2026-02-01 00:00:00' WHERE id = ?", todayHighNew)

	tasks, err := f.tasks.FindAll(context.Background(), model.TaskFilters{})
	require.NoError(t, err)
	var got []int64
	for _, task := range tasks {
		got = append(got, task.ID)
	}
	// Most overdue first; priority breaks same-day ties (datetime and date
	// compare by calendar date); oldest created breaks equal priority;
	// undated and unparseable last, in id order.
	assert.Equal(t, []int64{overdue, todayHighOld, todayHighNew, todayLow, tomorrow, undated, garbage}, got)
}

func TestNext(t *testing.T) {
	ctx := context.Background()

	t.Run("skips held and waiting, takes free rows", func(t *testing.T) {
		f := newClaimFixture(t)
		first := f.add(t, "first", "2026-11-01", "medium")
		second := f.add(t, "second", "2026-11-02", "medium")
		third := f.add(t, "third", "2026-11-03", "medium")

		got, _, err := f.claims.Next(ctx, locA, claimNow, claimToday)
		require.NoError(t, err)
		assert.Equal(t, first, got.ID)
		_, _, err = f.claims.Wait(ctx, first, locA, "https://mail/draft", nil, claimNow)
		require.NoError(t, err)

		got, _, err = f.claims.Next(ctx, locB, claimNow, claimToday)
		require.NoError(t, err)
		assert.Equal(t, second, got.ID, "waiting first is skipped")
		got, _, err = f.claims.Next(ctx, locC, claimNow, claimToday)
		require.NoError(t, err)
		assert.Equal(t, third, got.ID, "held second is skipped")

		_, _, err = f.claims.Release(ctx, second, strp(locB), strp("try later"), claimNow)
		require.NoError(t, err)
		got, c, err := f.claims.Next(ctx, locB, claimNow.Add(time.Minute), claimToday)
		require.NoError(t, err)
		assert.Equal(t, second, got.ID, "a free row is picked again")
		assert.Equal(t, "try later", *c.Note, "the note survives the re-hold")
		assert.Equal(t, locB, *c.PreviousHolder)
	})

	t.Run("future excluded, unparseable included", func(t *testing.T) {
		f := newClaimFixture(t)
		f.add(t, "tomorrow", "2026-11-04", "high")
		garbage := f.add(t, "unparseable", "soon", "low")
		got, _, err := f.claims.Next(ctx, locA, claimNow, claimToday)
		require.NoError(t, err)
		assert.Equal(t, garbage, got.ID)
		_, _, err = f.claims.Next(ctx, locB, claimNow, claimToday)
		exitIs(t, err, model.ErrNotFound)
	})

	t.Run("repeat returns the current hold unchanged", func(t *testing.T) {
		f := newClaimFixture(t)
		id := f.add(t, "one", "", "medium")
		f.add(t, "two", "", "medium")
		_, first, err := f.claims.Next(ctx, locA, claimNow, claimToday)
		require.NoError(t, err)
		got, again, err := f.claims.Next(ctx, locA, claimNow.Add(time.Hour), claimToday)
		require.NoError(t, err)
		assert.Equal(t, id, got.ID)
		assert.Equal(t, first.UpdatedAt, again.UpdatedAt, "a repeated next does not refresh updated_at")
		assert.Equal(t, "1h0m", again.Idle)
	})

	t.Run("empty queue", func(t *testing.T) {
		f := newClaimFixture(t)
		_, _, err := f.claims.Next(ctx, locA, claimNow, claimToday)
		exitIs(t, err, model.ErrNotFound)
	})

	t.Run("invalid locator", func(t *testing.T) {
		f := newClaimFixture(t)
		_, _, err := f.claims.Next(ctx, "agent", claimNow, claimToday)
		exitIs(t, err, model.ErrValidation)
	})
}

func TestClaim(t *testing.T) {
	ctx := context.Background()
	f := newClaimFixture(t)
	held := f.add(t, "held", "", "medium")
	waiting := f.add(t, "waiting", "", "medium")
	future := f.add(t, "future", "2027-01-01", "medium")

	_, _, err := f.claims.Claim(ctx, held, locA, claimNow)
	require.NoError(t, err)
	_, _, err = f.claims.Claim(ctx, waiting, locB, claimNow)
	require.NoError(t, err)
	_, _, err = f.claims.Wait(ctx, waiting, locB, "https://draft", nil, claimNow)
	require.NoError(t, err)

	_, _, err = f.claims.Claim(ctx, held, locC, claimNow)
	exitIs(t, err, model.ErrConflict)
	assert.ErrorContains(t, err, locA, "the conflict names the holder")

	_, _, err = f.claims.Claim(ctx, waiting, locC, claimNow)
	exitIs(t, err, model.ErrConflict)
	assert.ErrorContains(t, err, "waiting")

	_, _, err = f.claims.Claim(ctx, future, locA, claimNow)
	exitIs(t, err, model.ErrConflict)
	assert.ErrorContains(t, err, "already holds task")

	task, c, err := f.claims.Claim(ctx, future, locC, claimNow)
	require.NoError(t, err, "a future-dated task can be claimed")
	assert.Equal(t, future, task.ID)
	assert.Equal(t, model.ClaimHeld, c.State)

	again, c2, err := f.claims.Claim(ctx, future, locC, claimNow.Add(time.Hour))
	require.NoError(t, err, "claiming your own hold returns it")
	assert.Equal(t, future, again.ID)
	assert.Equal(t, c.UpdatedAt, c2.UpdatedAt, "and does not refresh updated_at")

	_, _, err = f.claims.Claim(ctx, 9999, locC, claimNow)
	exitIs(t, err, model.ErrNotFound)
}

func TestTakeOver(t *testing.T) {
	ctx := context.Background()
	f := newClaimFixture(t)
	id := f.add(t, "work", "", "medium")
	other := f.add(t, "other", "", "medium")
	_, _, err := f.claims.Claim(ctx, id, locA, claimNow)
	require.NoError(t, err)

	_, _, err = f.claims.TakeOver(ctx, id, locB, claimNow.Add(time.Hour))
	exitIs(t, err, model.ErrConflict)
	assert.ErrorContains(t, err, "not stale")

	// A note at 1h59m keeps the hold alive for another 2h.
	_, _, err = f.claims.Note(ctx, id, locA, "still drafting", claimNow.Add(119*time.Minute))
	require.NoError(t, err)
	_, _, err = f.claims.TakeOver(ctx, id, locB, claimNow.Add(2*time.Hour))
	exitIs(t, err, model.ErrConflict)

	stale := claimNow.Add(119*time.Minute + 2*time.Hour)
	_, _, err = f.claims.Claim(ctx, other, locC, claimNow)
	require.NoError(t, err)
	_, _, err = f.claims.TakeOver(ctx, id, locC, stale)
	exitIs(t, err, model.ErrConflict)
	assert.ErrorContains(t, err, "already holds task")

	_, c, err := f.claims.TakeOver(ctx, id, locB, stale)
	require.NoError(t, err, "exactly 2h idle is stale")
	assert.Equal(t, locB, *c.Holder)
	assert.Equal(t, locA, *c.PreviousHolder)
	assert.Equal(t, "still drafting", *c.Note)

	_, _, err = f.claims.TakeOver(ctx, other+1000, locB, stale)
	exitIs(t, err, model.ErrNotFound)
}

func TestNoteWaitRelease(t *testing.T) {
	ctx := context.Background()
	f := newClaimFixture(t)
	id := f.add(t, "email recruiter", "", "medium")
	_, _, err := f.claims.Claim(ctx, id, locA, claimNow)
	require.NoError(t, err)

	_, _, err = f.claims.Note(ctx, id, locB, "x", claimNow)
	exitIs(t, err, model.ErrConflict)
	_, _, err = f.claims.Wait(ctx, id, locB, "https://draft", nil, claimNow)
	exitIs(t, err, model.ErrConflict)
	_, _, err = f.claims.Release(ctx, id, strp(locB), nil, claimNow)
	exitIs(t, err, model.ErrConflict)
	_, _, err = f.claims.Release(ctx, id, nil, nil, claimNow)
	exitIs(t, err, model.ErrValidation)
	_, _, err = f.claims.Wait(ctx, id, locA, " ", nil, claimNow)
	exitIs(t, err, model.ErrValidation)

	_, c, err := f.claims.Note(ctx, id, locA, "creating Gmail draft", claimNow.Add(time.Minute))
	require.NoError(t, err)
	assert.Equal(t, "creating Gmail draft", *c.Note)
	assert.Equal(t, "0m", c.Idle)

	_, c, err = f.claims.Wait(ctx, id, locA, "https://draft", strp("draft ready"), claimNow.Add(2*time.Minute))
	require.NoError(t, err)
	assert.Equal(t, model.ClaimWaiting, c.State)
	assert.Nil(t, c.Holder)
	assert.Equal(t, locA, *c.PreviousHolder)
	assert.Equal(t, "https://draft", *c.Link)
	assert.Equal(t, "draft ready", *c.Note)

	waiting, err := f.tasks.FindAll(ctx, model.TaskFilters{Waiting: true})
	require.NoError(t, err)
	require.Len(t, waiting, 1)
	assert.Equal(t, id, waiting[0].ID)

	_, c, err = f.claims.Release(ctx, id, nil, strp("first draft rejected"), claimNow.Add(3*time.Minute))
	require.NoError(t, err, "anyone may release a waiting task")
	assert.Equal(t, model.ClaimFree, c.State)
	assert.Nil(t, c.Link)
	assert.Nil(t, c.Holder)
	assert.Equal(t, locA, *c.PreviousHolder)
	assert.Equal(t, "first draft rejected", *c.Note)

	_, _, err = f.claims.Release(ctx, id, nil, nil, claimNow)
	exitIs(t, err, model.ErrConflict)
	assert.ErrorContains(t, err, "already free")

	// Held → free by the holder copies the holder to previous_holder.
	_, _, err = f.claims.Claim(ctx, id, locB, claimNow.Add(4*time.Minute))
	require.NoError(t, err)
	_, c, err = f.claims.Release(ctx, id, strp(locB), nil, claimNow.Add(5*time.Minute))
	require.NoError(t, err)
	assert.Equal(t, locB, *c.PreviousHolder)
	assert.Equal(t, "first draft rejected", *c.Note, "release without --note keeps the note")
}

func TestClosedTasks_NotFoundAndTriggerClears(t *testing.T) {
	ctx := context.Background()
	for _, close := range []string{"complete", "archive"} {
		t.Run(close, func(t *testing.T) {
			f := newClaimFixture(t)
			id := f.add(t, "work", "", "medium")
			_, _, err := f.claims.Claim(ctx, id, locA, claimNow)
			require.NoError(t, err)
			if close == "complete" {
				_, err = f.tasks.Complete(ctx, id)
			} else {
				err = f.tasks.Archive(ctx, id)
			}
			require.NoError(t, err)

			var n int
			require.NoError(t, f.db.QueryRow("SELECT COUNT(*) FROM task_claims WHERE task_id = ?", id).Scan(&n))
			assert.Zero(t, n, "the trigger removed the claim")

			for name, op := range map[string]func() error{
				"claim":   func() error { _, _, err := f.claims.Claim(ctx, id, locB, claimNow); return err },
				"note":    func() error { _, _, err := f.claims.Note(ctx, id, locA, "x", claimNow); return err },
				"wait":    func() error { _, _, err := f.claims.Wait(ctx, id, locA, "l", nil, claimNow); return err },
				"release": func() error { _, _, err := f.claims.Release(ctx, id, strp(locA), nil, claimNow); return err },
				"take":    func() error { _, _, err := f.claims.TakeOver(ctx, id, locB, claimNow); return err },
			} {
				t.Run(name, func(t *testing.T) {
					exitIs(t, op(), model.ErrNotFound)
				})
			}

			next := f.add(t, "next", "", "medium")
			got, _, err := f.claims.Next(ctx, locA, claimNow, claimToday)
			require.NoError(t, err, "the locator can hold a new task")
			assert.Equal(t, next, got.ID)
		})
	}
}

func TestStaleAndForTasks(t *testing.T) {
	ctx := context.Background()
	f := newClaimFixture(t)
	old := f.add(t, "old", "", "medium")
	fresh := f.add(t, "fresh", "", "medium")
	free := f.add(t, "free", "", "medium")
	_, _, err := f.claims.Claim(ctx, old, locA, claimNow)
	require.NoError(t, err)
	_, _, err = f.claims.Note(ctx, old, locA, "drafting", claimNow)
	require.NoError(t, err)
	_, _, err = f.claims.Claim(ctx, fresh, locB, claimNow.Add(time.Hour))
	require.NoError(t, err)

	later := claimNow.Add(2*time.Hour + 41*time.Minute)
	holds, err := f.claims.Stale(ctx, later)
	require.NoError(t, err)
	require.Len(t, holds, 1)
	assert.Equal(t, model.StaleHold{TaskID: old, Title: "old", Holder: locA, Idle: "2h41m", Note: strp("drafting")}, holds[0])

	holds, err = f.claims.Stale(ctx, claimNow)
	require.NoError(t, err)
	assert.NotNil(t, holds)
	assert.Empty(t, holds)

	claims, err := f.claims.ForTasks(ctx, []int64{old, fresh, free}, later)
	require.NoError(t, err)
	assert.Len(t, claims, 2)
	assert.Equal(t, "2h41m", claims[old].Idle)
	assert.Equal(t, time.UnixMilli(claimNow.UnixMilli()).UTC().Format("2006-01-02T15:04:05Z"), claims[old].UpdatedAt)
	assert.NotContains(t, claims, free)
}

// A list without limit can pass more ids than SQLite binds in one statement
// (32766); claims on both sides of the cut are still found.
func TestForTasks_MoreIDsThanBoundParameters(t *testing.T) {
	ctx := context.Background()
	f := newClaimFixture(t)
	first := f.add(t, "first", "", "medium")
	last := f.add(t, "last", "", "medium")
	_, _, err := f.claims.Claim(ctx, first, locA, claimNow)
	require.NoError(t, err)
	_, _, err = f.claims.Claim(ctx, last, locB, claimNow)
	require.NoError(t, err)

	ids := []int64{first}
	for i := int64(0); len(ids) < 40000; i++ {
		ids = append(ids, 1_000_000+i) // ids no task has
	}
	ids = append(ids, last)

	claims, err := f.claims.ForTasks(ctx, ids, claimNow)
	require.NoError(t, err)
	assert.Len(t, claims, 2)
	assert.Equal(t, locA, *claims[first].Holder)
	assert.Equal(t, locB, *claims[last].Holder)
}

func TestHeldIndexIsTheLastGuard(t *testing.T) {
	f := newClaimFixture(t)
	a := f.add(t, "a", "", "medium")
	b := f.add(t, "b", "", "medium")
	f.exec(t, "INSERT INTO task_claims (task_id, state, holder, updated_at) VALUES (?, 'held', ?, 0)", a, locA)
	_, err := f.db.Exec("INSERT INTO task_claims (task_id, state, holder, updated_at) VALUES (?, 'held', ?, 0)", b, locA)
	assert.ErrorContains(t, err, "UNIQUE")
	_, err = f.db.Exec("INSERT INTO task_claims (task_id, state, updated_at) VALUES (?, 'held', 0)", b)
	assert.ErrorContains(t, err, "CHECK", "held needs a holder")
	_, err = f.db.Exec("INSERT INTO task_claims (task_id, state, updated_at) VALUES (?, 'waiting', 0)", b)
	assert.ErrorContains(t, err, "CHECK", "waiting needs a link")
}
