// Package ui serves crm ui: a spreadsheet page (embedded static files and a
// vendored Tabulator grid, no build step) over thin JSON endpoints that call
// internal/service, the layer the CLI and the MCP tools use.
//
// Every /api request goes through httpguard (bearer token, Host and Origin
// allowlists, body cap); state-changing requests must also be same-site
// and carry a JSON (or, for the CSV import, text/csv) body, so a cross-site
// form or a simple request can never trigger them. The static files carry
// no data and are served without the token, behind the same Host and Origin
// checks.
package ui

import (
	"bytes"
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"strconv"

	"github.com/jdanielnd/crm-cli/internal/column"
	"github.com/jdanielnd/crm-cli/internal/csvio"
	"github.com/jdanielnd/crm-cli/internal/httpguard"
	"github.com/jdanielnd/crm-cli/internal/model"
	"github.com/jdanielnd/crm-cli/internal/provider"
	"github.com/jdanielnd/crm-cli/internal/run"
	"github.com/jdanielnd/crm-cli/internal/service"
	"github.com/jdanielnd/crm-cli/internal/table"
)

//go:embed static
var staticFiles embed.FS

// importSource is the provenance recorded on rows imported through the UI.
const importSource = "ui"

// contentSecurityPolicy lets the page run only its own scripts and talk only
// to its own origin; cell values are rendered as text, and this keeps a
// value that slipped through from running script.
const contentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// Server is the crm ui web server on one database.
type Server struct {
	svc  *service.Service
	runs *run.Tracker
}

// New creates the server; registry opens the providers of enrich and ai
// columns (provider.NewEnvRegistry in production). Close it to stop the runs
// it started.
func New(db *sql.DB, registry *provider.Registry) *Server {
	return &Server{svc: service.New(db, registry), runs: run.NewTracker()}
}

// Close interrupts the runs started from the UI (they stay resumable) and
// waits for them to record their state.
func (s *Server) Close() { s.runs.Close() }

// Serve listens on addr until ctx is cancelled (see httpguard.Serve).
func (s *Server) Serve(ctx context.Context, addr, token string, ready func(net.Addr)) error {
	return httpguard.Serve(ctx, addr, token, s.Handler, ready)
}

// Handler serves the static page and, behind the bearer token, the API.
func (s *Server) Handler(guard httpguard.Config) (http.Handler, error) {
	api := http.NewServeMux()
	api.HandleFunc("GET /api/tables", s.listTables)
	api.HandleFunc("GET /api/tables/{table}", s.viewTable)
	api.HandleFunc("GET /api/tables/{table}/export", s.exportTable)
	api.HandleFunc("GET /api/tables/{table}/runs", s.tableRuns)
	api.Handle("POST /api/tables/import", sameSite("text/csv", s.importTable))
	api.Handle("POST /api/tables/{table}/columns", sameSite("application/json", s.addColumn))
	api.Handle("POST /api/tables/{table}/estimate", sameSite("application/json", s.estimate))
	api.Handle("POST /api/tables/{table}/runs", sameSite("application/json", s.startRun))
	api.HandleFunc("GET /api/runs/{id}", s.runStatus)
	api.Handle("POST /api/runs/{id}/cancel", sameSite("application/json", s.cancelRun))
	guarded, err := httpguard.Wrap(guard, noStore(api))
	if err != nil {
		return nil, err
	}
	static, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("/api/", guarded)
	mux.Handle("/", httpguard.WrapPublic(guard, readOnly(http.FileServerFS(static))))
	return securityHeaders(mux), nil
}

// readOnly answers 405 to anything but GET and HEAD.
func readOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// sameSite guards a state-changing endpoint against cross-site requests:
// a browser's Sec-Fetch-Site must say same-origin (or none: typed by the
// user), and the body must be contentType. Neither application/json nor
// text/csv is a CORS "simple" type, so a foreign page cannot send one without
// a preflight, which this server never grants; the bearer token (checked
// before) is a second custom header no foreign page can know.
func sameSite(contentType string, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			writeError(w, http.StatusForbidden, "forbidden: cross-site request")
			return
		}
		if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != contentType {
			writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be "+contentType)
			return
		}
		next(w, r)
	})
}

type errorBody struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("crm ui: write response: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorBody{Error: msg})
}

// fail answers err: not found 404, conflict 409, validation 400, an oversized
// body 413; anything else is logged and answered 500 "internal error", so
// database and network details stay out of responses.
func fail(w http.ResponseWriter, r *http.Request, err error) {
	var tooLarge *http.MaxBytesError
	switch {
	case errors.Is(err, model.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, model.ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, model.ErrValidation):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.As(err, &tooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
	default:
		log.Printf("crm ui: %s %s: %v", r.Method, r.URL.Path, err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// decode reads a JSON request body into v (unknown fields are an error).
func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return err
		}
		return model.NewExitError(model.ErrValidation, "invalid JSON body: %s", err)
	}
	return nil
}

type tableSummary struct {
	Name    string  `json:"name"`
	Key     *string `json:"key_column"`
	Rows    int     `json:"rows"`
	Columns int     `json:"columns"`
}

func (s *Server) listTables(w http.ResponseWriter, r *http.Request) {
	list, err := s.svc.Tables(r.Context())
	if err != nil {
		fail(w, r, err)
		return
	}
	out := make([]tableSummary, len(list))
	for i, t := range list {
		out[i] = tableSummary{Name: t.Name, Key: t.KeyColumn, Rows: t.Rows, Columns: t.Columns}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tables": out})
}

// viewColumn is one column of the grid.
type viewColumn struct {
	Name       string        `json:"name"`
	Type       string        `json:"type"`
	Definition string        `json:"definition"`
	Config     column.Config `json:"config"`
	Runnable   bool          `json:"runnable"`
	CostEst    string        `json:"cost_est"`
}

// Cell statuses of computed columns. Pending is an item of a run that is
// not executing (interrupted: resumable); running, an open item of a run
// that is; uncertain, an item that paid a call that got no response.
const (
	cellPending   = "pending"
	cellRunning   = "running"
	cellOK        = "ok"
	cellEmpty     = "empty"
	cellError     = "error"
	cellSkipped   = "skipped"
	cellUncertain = "uncertain"
)

// cellState is the state of one computed cell: its newest run item.
type cellState struct {
	Status   string `json:"status"`
	Error    string `json:"error,omitempty"`
	Provider string `json:"provider,omitempty"`
	CacheHit bool   `json:"cache_hit,omitempty"`
	Cost     string `json:"cost"`
	RunID    int64  `json:"run_id"`
}

func newCellState(c *run.CellState, active bool) *cellState {
	cs := &cellState{CacheHit: c.CacheHit, Cost: c.Cost.String(), RunID: c.RunID}
	if c.Error != nil {
		cs.Error = *c.Error
	}
	if c.Provider != nil {
		cs.Provider = *c.Provider
	}
	switch {
	case c.Status == run.ItemPending && active:
		cs.Status = cellRunning
	case c.Status == run.ItemPending:
		cs.Status = cellPending
	case c.Uncertain > 0:
		cs.Status = cellUncertain
		if cs.Error == "" {
			cs.Error = fmt.Sprintf("%s of this cell's cost is uncertain: a call got no response after it was sent and was counted at its estimate", c.Uncertain)
		}
	case c.Status == run.ItemOK:
		cs.Status = cellOK
	case c.Status == run.ItemFailed:
		cs.Status = cellError
	case c.Status == run.ItemSkipped:
		cs.Status = cellSkipped
	default:
		cs.Status = cellEmpty
	}
	return cs
}

// viewRow is one row of the grid: cell values and states in column order
// (a state is null for input and formula columns and for cells no run
// covered yet).
type viewRow struct {
	ID       int64        `json:"id"`
	Position int          `json:"row"`
	Values   []any        `json:"values"`
	States   []*cellState `json:"states"`
}

// viewPayload is a table as the grid shows it.
type viewPayload struct {
	Table   string         `json:"table"`
	Key     *string        `json:"key_column"`
	Columns []viewColumn   `json:"columns"`
	Rows    []viewRow      `json:"rows"`
	Live    map[string]any `json:"live_run"`
	Latest  map[string]any `json:"latest_run"`
}

// runPayload is RunData plus whether the run is executing now.
func (s *Server) runPayload(r *run.Run, live *run.Run) map[string]any {
	if r == nil {
		return nil
	}
	m := service.RunData(r)
	active, err := s.runs.State(r.ID)
	m["active"] = active || (live != nil && live.ID == r.ID)
	m["cancellable"] = active
	m["error"] = ""
	if err != nil {
		m["error"] = errText(err)
	}
	return m
}

// errText is the client-facing message of a background run error.
func errText(err error) string {
	if errors.Is(err, model.ErrNotFound) || errors.Is(err, model.ErrConflict) || errors.Is(err, model.ErrValidation) {
		return err.Error()
	}
	return "internal error"
}

func (s *Server) viewTable(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ws, view, err := s.svc.View(ctx, r.PathValue("table"), r.URL.Query()["only"])
	if err != nil {
		fail(w, r, err)
		return
	}
	live, latest, err := s.svc.TableRuns(ctx, ws.Name)
	if err != nil {
		fail(w, r, err)
		return
	}
	states, err := s.svc.CellStates(ctx, ws.ID)
	if err != nil {
		fail(w, r, err)
		return
	}
	isActive := map[int64]bool{}
	activeRun := func(id int64) bool {
		a, ok := isActive[id]
		if !ok {
			a, _ = s.runs.State(id)
			a = a || (live != nil && live.ID == id)
			isActive[id] = a
		}
		return a
	}
	out := viewPayload{Table: ws.Name, Key: ws.KeyColumn, Columns: make([]viewColumn, len(view.Columns)),
		Rows: make([]viewRow, len(view.Rows)), Live: s.runPayload(live, live), Latest: s.runPayload(latest, live)}
	for i, c := range view.Columns {
		out.Columns[i] = viewColumn{Name: c.Name, Type: string(c.Type), Definition: service.Definition(c), Config: c.Config,
			Runnable: c.Type.Runnable(), CostEst: c.CostEst.String()}
	}
	for i, row := range view.Rows {
		vr := viewRow{ID: row.ID, Position: row.Position, Values: make([]any, len(view.Columns)),
			States: make([]*cellState, len(view.Columns))}
		for j, c := range view.Columns {
			vr.Values[j] = view.Cells[i][c.Name]
			if st := states[row.ID][c.Name]; st != nil && c.Type.Runnable() {
				vr.States[j] = newCellState(st, activeRun(st.RunID))
			}
		}
		out.Rows[i] = vr
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) exportTable(w http.ResponseWriter, r *http.Request) {
	ws, view, err := s.svc.View(r.Context(), r.PathValue("table"), r.URL.Query()["only"])
	if err != nil {
		fail(w, r, err)
		return
	}
	var buf bytes.Buffer
	if err := service.Export(&buf, view, "csv"); err != nil {
		fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	// Table names are [A-Za-z0-9_.-]: safe in a quoted file name.
	w.Header().Set("Content-Disposition", `attachment; filename="`+ws.Name+`.csv"`)
	if _, err := w.Write(buf.Bytes()); err != nil {
		log.Printf("crm ui: write export: %v", err)
	}
}

func (s *Server) tableRuns(w http.ResponseWriter, r *http.Request) {
	live, latest, err := s.svc.TableRuns(r.Context(), r.PathValue("table"))
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"live_run": s.runPayload(live, live), "latest_run": s.runPayload(latest, live)})
}

// importTable reads a CSV body into table ?as= (created on first import;
// ?key= fixes its key column then).
func (s *Server) importTable(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name := q.Get("as")
	if err := table.ValidateName(name); err != nil {
		fail(w, r, err)
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		fail(w, r, err)
		return
	}
	parsed, err := csvio.Parse(data)
	if err != nil {
		fail(w, r, model.NewExitError(model.ErrValidation, "invalid csv: %s", err))
		return
	}
	res, err := s.svc.ImportCSV(r.Context(), table.ImportOptions{Name: name, Key: q.Get("key"), Source: importSource}, parsed)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"table": res.Workspace.Name, "rows": res.Rows, "created": res.Created,
		"updated": res.Updated, "new_columns": len(res.NewColumns)})
}

func (s *Server) addColumn(w http.ResponseWriter, r *http.Request) {
	var spec service.ColumnSpec
	if err := decode(r, &spec); err != nil {
		fail(w, r, err)
		return
	}
	c, err := s.svc.AddColumn(r.Context(), r.PathValue("table"), spec)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"column": service.ColumnData(c, 0, 0)})
}

// estimateRequest names the columns to estimate (or a column not saved yet,
// Preview, which is estimated alone when Columns is empty) and a row filter.
type estimateRequest struct {
	Columns []string            `json:"columns"`
	Only    []string            `json:"only"`
	Preview *service.ColumnSpec `json:"preview"`
}

func (s *Server) estimate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req estimateRequest
	if err := decode(r, &req); err != nil {
		fail(w, r, err)
		return
	}
	opts := run.Options{Table: r.PathValue("table"), Columns: req.Columns, Only: req.Only}
	if req.Preview != nil {
		c, err := s.svc.PreviewColumn(ctx, opts.Table, *req.Preview)
		if err != nil {
			fail(w, r, err)
			return
		}
		opts.Preview = []*column.Column{c}
		if len(opts.Columns) == 0 {
			opts.Columns = []string{c.Name}
		}
	}
	est, err := s.svc.Engine().Estimate(ctx, opts)
	if err != nil {
		fail(w, r, err)
		return
	}
	cols := make([]map[string]any, len(est.Columns))
	for i, ce := range est.Columns {
		cols[i] = service.EstimateData(ce)
	}
	writeJSON(w, http.StatusOK, map[string]any{"table": est.Table, "columns": cols, "total": service.EstimateData(&est.Total),
		"unbounded": est.Total.Unbounded > 0, "needs_budget": est.Total.Worst > 0 || est.Total.Unbounded > 0})
}

// startRequest starts a run of Columns over the rows matching Only. Budget
// is in USD ("0.50"); it may be empty only when the run cannot spend money.
type startRequest struct {
	Columns     []string `json:"columns"`
	Only        []string `json:"only"`
	Budget      string   `json:"budget"`
	Concurrency int      `json:"concurrency"`
}

// startRun creates the run (refused without a budget when it may spend
// money), then executes it in the background; poll GET /api/runs/{id}.
func (s *Server) startRun(w http.ResponseWriter, r *http.Request) {
	var req startRequest
	if err := decode(r, &req); err != nil {
		fail(w, r, err)
		return
	}
	if req.Concurrency < 0 {
		fail(w, r, model.NewExitError(model.ErrValidation, "concurrency must be at least 1"))
		return
	}
	opts := run.Options{Table: r.PathValue("table"), Columns: req.Columns, Only: req.Only, Concurrency: req.Concurrency,
		Retry: run.DefaultRetry}
	if req.Budget != "" {
		b, err := provider.ParseUSD(req.Budget)
		if err != nil {
			fail(w, r, model.NewExitError(model.ErrValidation, "budget: %s", err))
			return
		}
		opts.Budget, opts.BudgetSet = b, true
	}
	engine := s.svc.Engine()
	started, err := engine.Start(r.Context(), opts)
	if err != nil {
		fail(w, r, err)
		return
	}
	opts.Resume, opts.Only = started.ID, nil
	s.runs.Start(started.ID, func(ctx context.Context) error {
		_, err := engine.Execute(ctx, opts)
		return err
	})
	writeJSON(w, http.StatusAccepted, map[string]any{"run": s.runPayload(started, nil)})
}

func runID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return 0, model.NewExitError(model.ErrValidation, "invalid run id %q", r.PathValue("id"))
	}
	return id, nil
}

func (s *Server) runStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := runID(r)
	if err != nil {
		fail(w, r, err)
		return
	}
	rn, err := s.svc.RunStatus(ctx, id)
	if err != nil {
		fail(w, r, err)
		return
	}
	live, _, err := s.svc.TableRuns(ctx, rn.Table)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": s.runPayload(rn, live)})
}

// cancelRun stops a run this server executes; it is left interrupted and
// resumable (crm run --resume). A run executed by another process (crm run
// in a terminal) is stopped there.
func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request) {
	id, err := runID(r)
	if err != nil {
		fail(w, r, err)
		return
	}
	if _, err := s.svc.RunStatus(r.Context(), id); err != nil {
		fail(w, r, err)
		return
	}
	if !s.runs.Cancel(id) {
		fail(w, r, model.NewExitError(model.ErrConflict,
			"run %d is not executing in this crm ui (finished, or run by another process: stop it there)", id))
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"run_id": id, "cancelling": true})
}
