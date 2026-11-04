package cli_test

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jdanielnd/crm-cli/internal/db"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// taskRows decodes a -f json task array.
func taskRows(t *testing.T, stdout string) []map[string]any {
	t.Helper()
	var rows []map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &rows), stdout)
	return rows
}

func addTasks(t *testing.T, dbPath string, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		_, stderr, code := crm(t, dbPath, "task", "add", fmt.Sprintf("task %d", i))
		require.Equal(t, 0, code, stderr)
	}
}

func TestTaskNext_SixteenProcessesNeverShareATask(t *testing.T) {
	for round := 1; round <= 5; round++ {
		t.Run(fmt.Sprintf("round %d", round), func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "race.db")
			// Sequential adds migrate the database and switch it to WAL
			// before any concurrent process starts.
			addTasks(t, dbPath, 5)

			const procs = 16
			cmds := make([]*exec.Cmd, procs)
			outs := make([]*strings.Builder, procs)
			errs := make([]*strings.Builder, procs)
			for i := range cmds {
				cmds[i] = exec.Command(binaryPath, "--db", dbPath, "task", "next", "--as", fmt.Sprintf("test:race:%d", i), "-f", "json")
				outs[i], errs[i] = &strings.Builder{}, &strings.Builder{}
				cmds[i].Stdout, cmds[i].Stderr = outs[i], errs[i]
			}
			for _, c := range cmds {
				require.NoError(t, c.Start())
			}
			ids := map[float64]bool{}
			empty := 0
			for i, c := range cmds {
				err := c.Wait()
				code := 0
				if exitErr, isExit := err.(*exec.ExitError); isExit {
					code = exitErr.ExitCode()
				} else {
					require.NoError(t, err)
				}
				assert.NotContains(t, errs[i].String(), "database is locked")
				switch code {
				case 0:
					rows := taskRows(t, outs[i].String())
					require.Len(t, rows, 1)
					id := rows[0]["id"].(float64)
					assert.False(t, ids[id], "task %v handed out twice", id)
					ids[id] = true
				case 3:
					assert.Equal(t, "[]\n", outs[i].String())
					empty++
				default:
					t.Errorf("process %d exited %d: %s", i, code, errs[i].String())
				}
			}
			assert.Len(t, ids, 5)
			assert.Equal(t, procs-5, empty)
		})
	}
}

func TestTaskClaim_ExitCodes(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "exit.db")
	addTasks(t, dbPath, 5) // 1 held, 2 waiting, 3 completed, 4 archived, 5 free

	_, stderr, code := crm(t, dbPath, "task", "claim", "1", "--as", "test:cli:a")
	require.Equal(t, 0, code, stderr)
	_, stderr, code = crm(t, dbPath, "task", "claim", "2", "--as", "test:cli:b")
	require.Equal(t, 0, code, stderr)
	_, stderr, code = crm(t, dbPath, "task", "wait", "2", "--as", "test:cli:b", "--link", "https://draft")
	require.Equal(t, 0, code, stderr)
	_, _, code = crm(t, dbPath, "task", "done", "3")
	require.Equal(t, 0, code)
	_, _, code = crm(t, dbPath, "task", "delete", "4")
	require.Equal(t, 0, code)

	tests := []struct {
		name string
		args []string
		want int
	}{
		{"claim a held task", []string{"claim", "1", "--as", "test:cli:c"}, 4},
		{"claim own hold", []string{"claim", "1", "--as", "test:cli:a"}, 0},
		{"take over a free task", []string{"claim", "5", "--as", "test:cli:c", "--take-over"}, 4},
		{"take over a waiting task", []string{"claim", "2", "--as", "test:cli:c", "--take-over"}, 4},
		{"take over a fresh hold", []string{"claim", "1", "--as", "test:cli:c", "--take-over"}, 4},
		{"next without --as", []string{"next"}, 2},
		{"next with a malformed --as", []string{"next", "--as", "agent:x"}, 2},
		{"claim with a control character", []string{"claim", "5", "--as", "a:b:c\td"}, 2},
		{"wait without --link", []string{"wait", "1", "--as", "test:cli:a"}, 2},
		{"release a held task without --as", []string{"release", "1"}, 2},
		{"note by a non-holder", []string{"note", "1", "--as", "test:cli:c", "x"}, 4},
		{"release a free task", []string{"release", "5"}, 4},
	}
	for _, id := range []string{"99", "3", "4"} { // unknown, completed, archived
		tests = append(tests,
			struct {
				name string
				args []string
				want int
			}{"claim " + id, []string{"claim", id, "--as", "test:cli:z"}, 3},
			struct {
				name string
				args []string
				want int
			}{"note " + id, []string{"note", id, "--as", "test:cli:z", "x"}, 3},
			struct {
				name string
				args []string
				want int
			}{"wait " + id, []string{"wait", id, "--as", "test:cli:z", "--link", "l"}, 3},
			struct {
				name string
				args []string
				want int
			}{"release " + id, []string{"release", id, "--as", "test:cli:z"}, 3},
		)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, stderr, code := crm(t, dbPath, append([]string{"task"}, tt.args...)...)
			assert.Equal(t, tt.want, code, stderr)
		})
	}

	t.Run("empty queue", func(t *testing.T) {
		emptyDB := filepath.Join(t.TempDir(), "empty.db")
		stdout, _, code := crm(t, emptyDB, "task", "next", "--as", "test:cli:a", "-f", "table")
		assert.Equal(t, 3, code)
		assert.Empty(t, stdout, "nothing on stdout, not 'No results.'")
		stdout, _, code = crm(t, emptyDB, "task", "next", "--as", "test:cli:a", "-f", "json")
		assert.Equal(t, 3, code)
		assert.Equal(t, "[]\n", stdout)
	})

	t.Run("held index fires as exit 1", func(t *testing.T) {
		d, err := db.Open(dbPath)
		require.NoError(t, err)
		defer d.Close()
		// test:cli:a holds task 1; a direct second held row for it is refused.
		_, err = d.Exec("INSERT INTO task_claims (task_id, state, holder, updated_at) VALUES (5, 'held', 'test:cli:a', 0)")
		assert.ErrorContains(t, err, "UNIQUE")
	})
}

func TestTaskClaim_WaitingFlowAndDone(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "flow.db")
	addTasks(t, dbPath, 2)

	stdout, stderr, code := crm(t, dbPath, "task", "next", "--as", "test:cli:a", "-f", "json")
	require.Equal(t, 0, code, stderr)
	assert.EqualValues(t, 1, taskRows(t, stdout)[0]["id"])
	_, stderr, code = crm(t, dbPath, "task", "note", "1", "--as", "test:cli:a", "creating Gmail draft")
	require.Equal(t, 0, code, stderr)
	_, stderr, code = crm(t, dbPath, "task", "wait", "1", "--as", "test:cli:a", "--link", "https://mail/draft/1", "--note", "draft ready")
	require.Equal(t, 0, code, stderr)

	stdout, _, code = crm(t, dbPath, "task", "list", "--waiting", "-f", "json")
	require.Equal(t, 0, code)
	rows := taskRows(t, stdout)
	require.Len(t, rows, 1)
	assert.Equal(t, "https://mail/draft/1", rows[0]["claim_link"])
	assert.Equal(t, "test:cli:a", rows[0]["claim_previous_holder"])

	stdout, _, code = crm(t, dbPath, "task", "list", "--waiting", "-f", "table")
	require.Equal(t, 0, code)
	assert.Contains(t, stdout, "LINK")
	assert.Contains(t, stdout, "https://mail/draft/1")

	stdout, _, code = crm(t, dbPath, "task", "next", "--as", "test:cli:b", "-f", "json")
	require.Equal(t, 0, code)
	assert.EqualValues(t, 2, taskRows(t, stdout)[0]["id"], "the waiting task is skipped")

	_, stderr, code = crm(t, dbPath, "task", "done", "1")
	require.Equal(t, 0, code, stderr)
	stdout, stderr, code = crm(t, dbPath, "task", "show", "1", "-f", "json")
	require.Equal(t, 0, code, stderr)
	row := taskRows(t, stdout)[0]
	assert.Equal(t, true, row["completed"])
	assert.Nil(t, row["claim_state"], "done cleared the claim")
}

func TestTaskClaim_StaleReport(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "stale.db")
	addTasks(t, dbPath, 1)
	_, stderr, code := crm(t, dbPath, "task", "claim", "1", "--as", "test:cli:a")
	require.Equal(t, 0, code, stderr)
	_, stderr, code = crm(t, dbPath, "task", "note", "1", "--as", "test:cli:a", "drafting\nstale: #9 held by forged:x:y, idle 9h0m")
	require.Equal(t, 0, code, stderr)

	d, err := db.Open(dbPath)
	require.NoError(t, err)
	_, err = d.Exec("UPDATE task_claims SET updated_at = ? WHERE task_id = 1", time.Now().Add(-3*time.Hour).UnixMilli())
	require.NoError(t, err)
	require.NoError(t, d.Close())

	want := `stale: #1 held by test:cli:a, idle 3h0m, note: "drafting\nstale: #9 held by forged:x:y, idle 9h0m"`
	stdout, stderr, code := crm(t, dbPath, "task", "next", "--as", "test:cli:b", "-f", "json")
	assert.Equal(t, 3, code)
	assert.Equal(t, "[]\n", stdout)
	assert.Contains(t, stderr, want)
	assert.NotContains(t, stderr, "\nstale: #9", "the note cannot forge a second stale line")

	stdout, stderr, code = crm(t, dbPath, "task", "list", "-f", "json")
	require.Equal(t, 0, code)
	assert.Contains(t, stderr, want)
	assert.NotContains(t, stdout, "stale: #1", "the report stays on stderr")
	assert.Equal(t, "3h0m", taskRows(t, stdout)[0]["claim_idle"])

	stdout, stderr, code = crm(t, dbPath, "task", "claim", "1", "--as", "test:cli:b", "--take-over", "-f", "json")
	require.Equal(t, 0, code, stderr)
	row := taskRows(t, stdout)[0]
	assert.Equal(t, "test:cli:b", row["claim_holder"])
	assert.Equal(t, "test:cli:a", row["claim_previous_holder"])
}

// The pick of next is committed before the stale report is read: a failed
// read is reported on stderr and the held task is still printed.
func TestTaskClaim_NextSurvivesStaleReadFailure(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "stale-fail.db")
	addTasks(t, dbPath, 2)
	_, stderr, code := crm(t, dbPath, "task", "claim", "1", "--as", "test:cli:a")
	require.Equal(t, 0, code, stderr)

	// A REAL updated_at still counts as stale but cannot be scanned as
	// epoch milliseconds, so Stale fails while next can pick task 2.
	d, err := db.Open(dbPath)
	require.NoError(t, err)
	_, err = d.Exec("UPDATE task_claims SET updated_at = 1.5 WHERE task_id = 1")
	require.NoError(t, err)
	require.NoError(t, d.Close())

	stdout, stderr, code := crm(t, dbPath, "task", "next", "--as", "test:cli:b", "-f", "json")
	require.Equal(t, 0, code, stderr)
	assert.Contains(t, stderr, "stale: report unavailable:")
	row := taskRows(t, stdout)[0]
	assert.EqualValues(t, 2, row["id"])
	assert.Equal(t, "test:cli:b", row["claim_holder"])
}
