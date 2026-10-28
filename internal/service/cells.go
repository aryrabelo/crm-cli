package service

import (
	"context"
	"time"

	"github.com/jdanielnd/crm-cli/internal/provider"
	"github.com/jdanielnd/crm-cli/internal/run"
	"github.com/jdanielnd/crm-cli/internal/table"
)

// CellStates returns, by row id and column name, the newest run item of
// every row × column of workspace workspaceID that a run covered (what a
// spreadsheet view shows next to each computed cell). Errors are redacted.
func (s *Service) CellStates(ctx context.Context, workspaceID int64) (map[int64]map[string]*run.CellState, error) {
	list, err := run.NewStore(s.db).CellStates(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]map[string]*run.CellState)
	for _, c := range list {
		if c.Error != nil {
			// Redacted again on output: rows written by earlier builds
			// stored provider errors verbatim.
			redacted := provider.RedactError(*c.Error)
			c.Error = &redacted
		}
		if out[c.RowID] == nil {
			out[c.RowID] = map[string]*run.CellState{}
		}
		out[c.RowID][c.Column] = c
	}
	return out, nil
}

// TableRuns returns the run of table name that an executor holds now, in
// this process or another one (nil when none), and the table's newest run
// (nil when it has none), both with live item counts.
func (s *Service) TableRuns(ctx context.Context, name string) (live, latest *run.Run, err error) {
	ws, err := table.NewStore(s.db).FindWorkspace(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	rs := run.NewStore(s.db)
	if live, err = rs.Live(ctx, ws.ID, time.Now(), run.DefaultLeaseTimeout); err != nil {
		return nil, nil, err
	}
	if latest, err = rs.Latest(ctx, ws.ID); err != nil {
		return nil, nil, err
	}
	for _, r := range []*run.Run{live, latest} {
		if r != nil {
			if err := withLiveStats(ctx, rs, r); err != nil {
				return nil, nil, err
			}
		}
	}
	return live, latest, nil
}
