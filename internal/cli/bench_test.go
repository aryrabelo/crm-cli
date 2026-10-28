package cli_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PLANO S5 aceite: crm bench run executes the 10 repository tasks and prints
// calls, cost and time per task; --baseline per-entity shows the call gap.
func TestBenchRun_RepositoryTasks(t *testing.T) {
	tasks := filepath.Join("..", "..", "bench")
	dbPath := filepath.Join(t.TempDir(), "unused.db")

	out, stderr, code := crm(t, dbPath, "bench", "run", "--tasks", tasks, "-f", "json")
	require.Equal(t, 0, code, stderr)
	rows := jsonRows(t, out)
	require.Len(t, rows, 11, "10 tasks + totals")
	for _, r := range rows[:10] {
		assert.Equal(t, "pass", r["check"], "%s: %v", r["task"], r["failure"])
		assert.Positive(t, r["calls"])
		assert.Contains(t, r, "cost_usd")
		assert.Contains(t, r, "wall_ms")
	}
	total := rows[10]
	assert.Equal(t, "TOTAL", total["task"])
	assert.Equal(t, "10/10 pass", total["check"])
	assert.EqualValues(t, 17, total["calls"])

	for _, transport := range []string{"inprocess", "http"} {
		t.Run(transport, func(t *testing.T) {
			out, stderr, code := crm(t, dbPath, "bench", "run", "--tasks", tasks, "--baseline", "per-entity",
				"--transport", transport, "-f", "json")
			require.Equal(t, 0, code, stderr)
			assert.Contains(t, stderr, "find-dedupe-people with per-entity tools only: check 1: got 35, want 30")
			byTask := map[string]map[string]any{}
			for _, r := range jsonRows(t, out) {
				byTask[r["task"].(string)] = r
			}
			imp := byTask["import-135-csv"]
			assert.EqualValues(t, 1, imp["batch_calls"])
			assert.EqualValues(t, 135, imp["per_entity_calls"])
			assert.Equal(t, "135.0x", imp["gap"])
			for task, r := range byTask {
				assert.Contains(t, r, "batch_provider_calls", task)
			}
			assert.EqualValues(t, 40, byTask["update-20-titles"]["per_entity_calls"])
			assert.Equal(t, "n/a", byTask["qualify-20"]["per_entity_calls"])
			assert.Equal(t, "FAIL", byTask["find-dedupe-people"]["per_entity_check"])
			assert.Equal(t, "pass", byTask["find-dedupe-people"]["batch_check"])
		})
	}

	_, stderr, code = crm(t, dbPath, "bench", "run", "--tasks", t.TempDir())
	assert.Equal(t, 3, code)
	assert.Contains(t, stderr, "no bench tasks")
	_, stderr, code = crm(t, dbPath, "bench", "run", "--tasks", tasks, "--baseline", "nope")
	assert.Equal(t, 2, code)
	assert.Contains(t, stderr, "--baseline must be per-entity")
}
