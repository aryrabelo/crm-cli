package run_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jdanielnd/crm-cli/internal/column"
	"github.com/jdanielnd/crm-cli/internal/provider"
	"github.com/jdanielnd/crm-cli/internal/run"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeChat is an OpenAI-compatible chat completions backend: it answers
// {"value": <score>} and reports one prompt token per 4 bytes, 20 completion
// tokens, and the cost it would bill at $1/M in, $2/M out.
type fakeChat struct {
	srv       *httptest.Server
	calls     atomic.Int64
	inFlight  atomic.Int64
	maxFlight atomic.Int64
	tokensIn  atomic.Int64
	tokensOut atomic.Int64
}

func newChat(t *testing.T, delay time.Duration) *fakeChat {
	t.Helper()
	f := &fakeChat{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := f.inFlight.Add(1)
		defer f.inFlight.Add(-1)
		for {
			m := f.maxFlight.Load()
			if n <= m || f.maxFlight.CompareAndSwap(m, n) {
				break
			}
		}
		f.calls.Add(1)
		var req struct {
			Messages []struct{ Content string } `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		size := 0
		for _, m := range req.Messages {
			size += len(m.Content)
		}
		in := int64(size / 4)
		f.tokensIn.Add(in)
		f.tokensOut.Add(20)
		time.Sleep(delay)
		json200(w, `{"choices":[{"message":{"content":"{\"value\": 42}"}}],"usage":{"prompt_tokens":`+
			strconv.FormatInt(in, 10)+`,"completion_tokens":20}}`)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (e *env) llm(t *testing.T, chat *fakeChat) {
	t.Helper()
	in, out := 1.0, 2.0
	require.NoError(t, provider.NewStore(e.db).Save(context.Background(), &provider.Config{
		Name: "llm", Kind: "llm", Enabled: true, Settings: provider.Settings{
			BaseURL: chat.srv.URL + "/v1", Model: "fake", PriceInPerMTok: &in, PriceOutPerMTok: &out, MaxTokens: 64,
		},
	}))
}

func aiColumn(e *env) *column.Column {
	return &column.Column{WorkspaceID: e.ws.ID, Name: "fit", Type: column.AI, Config: column.Config{
		Waterfall: []string{"llm"}, Prompt: "How well does {{name}} fit?", Output: "score:0-100",
	}}
}

// The budget holds under concurrency: every call reserves its worst case
// (prompt bound + max tokens, with the repair call) before it is made, so the
// actual spend never passes the budget even with 8 rows in flight.
func TestAI_BudgetNeverExceededUnderConcurrency(t *testing.T) {
	e := setup(t, 60)
	chat := newChat(t, 5*time.Millisecond)
	e.llm(t, chat)
	require.NoError(t, e.tables.AddColumn(context.Background(), aiColumn(e)))
	ctx := context.Background()

	est, err := e.engine.Estimate(ctx, run.Options{Table: "leads", Columns: []string{"fit"}})
	require.NoError(t, err)
	require.Equal(t, 60, est.Total.Calls)
	perRow := est.Total.Worst / 60
	budget := perRow * 3 // room for 3 calls in flight; the actual cost is far below the worst case

	res, err := e.engine.Execute(ctx, run.Options{Table: "leads", Columns: []string{"fit"}, Concurrency: 8,
		Budget: budget, BudgetSet: true})
	require.NoError(t, err)
	r := res.Run
	assert.Greater(t, chat.maxFlight.Load(), int64(1), "calls ran concurrently")
	assert.LessOrEqual(t, r.Spent, budget)
	assert.Equal(t, run.StatusBudgetExhausted, r.Status)
	assert.Positive(t, r.Stats.Skipped)
	assert.Equal(t, int(chat.calls.Load()), r.Stats.OK, "one call per answered row")
	assert.Equal(t, 60, r.Stats.OK+r.Stats.Skipped)
	assert.Equal(t, chat.tokensIn.Load(), r.Stats.TokensIn)
	assert.Equal(t, chat.tokensOut.Load(), r.Stats.TokensOut)
	billed := provider.Cost((chat.tokensIn.Load()*1_000_000 + chat.tokensOut.Load()*2_000_000 + 999_999) / 1_000_000)
	assert.InDelta(t, int64(billed), int64(r.Spent), float64(r.Stats.OK), "spent is the real usage × price (per-call rounding)")

	rows, err := e.tables.Rows(ctx, e.ws.ID)
	require.NoError(t, err)
	scored := 0
	for _, row := range rows {
		if row.Data["fit"] == 42.0 {
			scored++
		}
	}
	assert.Equal(t, r.Stats.OK, scored, "scores are stored as numbers")
}

func TestAI_OnlyFilterAndPreview(t *testing.T) {
	e := setup(t, 30)
	chat := newChat(t, 0)
	e.llm(t, chat)
	ctx := context.Background()
	c := aiColumn(e)

	est, err := e.engine.Estimate(ctx, run.Options{Table: "leads", Columns: []string{"fit"},
		Only: []string{"name~^user00"}, Preview: []*column.Column{c}})
	require.NoError(t, err, "an unsaved column can be estimated")
	assert.Equal(t, 10, est.Total.Items)
	assert.Equal(t, 10, est.Total.Calls)
	assert.Positive(t, est.Total.Worst)
	assert.Zero(t, e.count(t, "SELECT COUNT(*) FROM workspace_columns WHERE name = 'fit'"))

	_, err = e.engine.Execute(ctx, run.Options{Table: "leads", Columns: []string{"fit"}, Preview: []*column.Column{c}})
	require.Error(t, err, "preview columns are never run")

	require.NoError(t, e.tables.AddColumn(ctx, c))
	res, err := e.engine.Execute(ctx, run.Options{Table: "leads", Columns: []string{"fit"},
		Only: []string{"name~^user00"}, Budget: 1_000_000, BudgetSet: true})
	require.NoError(t, err)
	assert.Equal(t, 10, res.Run.Stats.Items)
	assert.Equal(t, 10, res.Run.Stats.OK)
	assert.EqualValues(t, 10, chat.calls.Load())

	// Re-run over all rows: the 10 answered rows are cache hits.
	res, err = e.engine.Execute(ctx, run.Options{Table: "leads", Columns: []string{"fit"}, Budget: 1_000_000, BudgetSet: true})
	require.NoError(t, err)
	assert.Equal(t, 30, res.Run.Stats.OK)
	assert.Equal(t, 10, res.Run.Stats.CacheHits)
	assert.EqualValues(t, 30, chat.calls.Load())
}
