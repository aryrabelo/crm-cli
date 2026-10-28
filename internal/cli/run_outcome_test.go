package cli

import (
	"testing"

	"github.com/jdanielnd/crm-cli/internal/model"
	"github.com/jdanielnd/crm-cli/internal/run"
	"github.com/stretchr/testify/assert"
)

func TestRunOutcome(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  run.Run
		code int
	}{
		{"completed", run.Run{Status: run.StatusCompleted, Stats: run.Stats{OK: 3, Empty: 1}}, 0},
		{"completed with failed items", run.Run{Status: run.StatusCompleted, Stats: run.Stats{OK: 2, Failed: 1}}, 6},
		{"budget exhausted", run.Run{Status: run.StatusBudgetExhausted, Stats: run.Stats{Skipped: 2, Failed: 1}}, 5},
		{"interrupted", run.Run{Status: run.StatusInterrupted, Stats: run.Stats{Pending: 2, Skipped: 1}}, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := runOutcome(&tc.run)
			if tc.code == 0 {
				assert.NoError(t, err)
				return
			}
			assert.Equal(t, tc.code, model.ExitCode(err))
		})
	}
}
