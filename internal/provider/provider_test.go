package provider_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jdanielnd/crm-cli/internal/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func open(t *testing.T, cfg *provider.Config, vars map[string]string) provider.Adapter {
	t.Helper()
	cfg.Enabled = true
	a, err := provider.NewRegistry(http.DefaultClient, env(vars)).Open(cfg)
	require.NoError(t, err)
	return a
}

func TestCost(t *testing.T) {
	cases := map[string]provider.Cost{"0": 0, "0.005": 5000, "$1.50": 1_500_000, "2": 2_000_000}
	for in, want := range cases {
		got, err := provider.ParseUSD(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	_, err := provider.ParseUSD("-1")
	assert.Error(t, err)
	assert.Equal(t, "$0.005", provider.Cost(5000).String())
	assert.Equal(t, "$1.50", provider.Cost(1_500_000).String())
	assert.Equal(t, "$0.00", provider.Cost(0).String())
}

func TestExa_SearchAndContents(t *testing.T) {
	var gotPath, gotKey string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotKey = r.URL.Path, r.Header.Get("x-api-key")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		_, _ = w.Write([]byte(`{"requestId":"x","results":[{"url":"https://acme.com","title":"Acme"}]}`))
	}))
	defer srv.Close()

	a := open(t, &provider.Config{Name: "exa", Kind: "exa", AuthRef: "EXA_API_KEY", Price: 5000,
		Settings: provider.Settings{NumResults: 3}},
		map[string]string{"EXA_API_KEY": "k1", "CRM_PROVIDER_EXA_URL": srv.URL})
	out, cost, err := a.Enrich(context.Background(), provider.Input{Value: "acme corp"})
	require.NoError(t, err)
	assert.Equal(t, provider.Cost(5000), cost)
	assert.Equal(t, provider.Cost(5000), a.Estimate(provider.Input{Value: "x"}))
	assert.JSONEq(t, `[{"url":"https://acme.com","title":"Acme"}]`, string(out))
	assert.Equal(t, "/search", gotPath)
	assert.Equal(t, "k1", gotKey)
	assert.Equal(t, "acme corp", gotBody["query"])
	assert.EqualValues(t, 3, gotBody["numResults"])

	c := open(t, &provider.Config{Name: "exa-contents", Kind: "exa", AuthRef: "EXA_API_KEY",
		Settings: provider.Settings{Endpoint: "contents", BaseURL: srv.URL}}, map[string]string{"EXA_API_KEY": "k1"})
	_, _, err = c.Enrich(context.Background(), provider.Input{Value: "acme.com"})
	require.NoError(t, err)
	assert.Equal(t, "/contents", gotPath)
	assert.Equal(t, []any{"https://acme.com"}, gotBody["urls"])
}

func TestExa_MissingKeyIsNotRetryable(t *testing.T) {
	a := open(t, &provider.Config{Name: "exa", Kind: "exa", AuthRef: "EXA_API_KEY"}, nil)
	_, cost, err := a.Enrich(context.Background(), provider.Input{Value: "x"})
	require.ErrorContains(t, err, "EXA_API_KEY is not set")
	assert.Zero(t, cost)
	retry, _ := provider.Retryable(err)
	assert.False(t, retry)
}

func TestJina_ReturnsDataObject(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"code":200,"data":{"title":"Acme","content":"hello"}}`))
	}))
	defer srv.Close()
	a := open(t, &provider.Config{Name: "jina", Kind: "jina", AuthRef: "JINA_API_KEY"},
		map[string]string{"CRM_PROVIDER_JINA_URL": srv.URL})
	out, _, err := a.Enrich(context.Background(), provider.Input{Value: "acme.com/about"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"title":"Acme","content":"hello"}`, string(out))
	assert.Equal(t, "/https://acme.com/about", gotPath)
	assert.Empty(t, gotAuth, "key is optional")
}

func TestGitHub_UserAndNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/octocat" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
			return
		}
		assert.Equal(t, "Bearer t0k", r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"login":"octocat","company":"@github"}`))
	}))
	defer srv.Close()
	a := open(t, &provider.Config{Name: "github", Kind: "github", AuthRef: "GITHUB_TOKEN", Price: 0},
		map[string]string{"GITHUB_TOKEN": "t0k", "CRM_PROVIDER_GITHUB_URL": srv.URL})

	out, _, err := a.Enrich(context.Background(), provider.Input{Value: "https://github.com/OctoCat"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"login":"octocat","company":"@github"}`, string(out))
	assert.Equal(t, a.Key(provider.Input{Value: "@octocat"}), a.Key(provider.Input{Value: "github.com/octocat/"}))

	out, _, err = a.Enrich(context.Background(), provider.Input{Value: "nobody-here"})
	require.NoError(t, err, "404 is no data, not an error")
	assert.Equal(t, "null", string(out))
}

func TestHTTP_TemplatePathAndAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "secret", r.Header.Get("X-Key"))
		if r.Method == http.MethodPost {
			b, _ := io.ReadAll(r.Body)
			assert.JSONEq(t, `{"q":"a \"b\""}`, string(b))
		} else {
			assert.Equal(t, "a b&c", r.URL.Query().Get("q"))
		}
		_, _ = w.Write([]byte(`{"data":[{"email":"a@x.com"}]}`))
	}))
	defer srv.Close()
	vars := map[string]string{"K": "secret"}

	get := open(t, &provider.Config{Name: "h", Kind: "http", AuthRef: "K", Price: 10,
		Settings: provider.Settings{URL: srv.URL + "/find?q={{input}}", Path: "data.0.email", AuthHeader: "X-Key"}}, vars)
	out, cost, err := get.Enrich(context.Background(), provider.Input{Value: "a b&c"})
	require.NoError(t, err)
	assert.Equal(t, `"a@x.com"`, string(out))
	assert.Equal(t, provider.Cost(10), cost)

	post := open(t, &provider.Config{Name: "h2", Kind: "http", AuthRef: "K",
		Settings: provider.Settings{URL: srv.URL, Body: `{"q":"{{input}}"}`, Path: "data.9.email", AuthHeader: "X-Key"}}, vars)
	out, _, err = post.Enrich(context.Background(), provider.Input{Value: `a "b"`})
	require.NoError(t, err)
	assert.Equal(t, "null", string(out), "missing path is no data")
}

func TestHTTP_ErrorClassification(t *testing.T) {
	var status atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s := int(status.Load())
		if s == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "2")
		}
		w.WriteHeader(s)
	}))
	defer srv.Close()
	a := open(t, &provider.Config{Name: "h", Kind: "http", Price: 10, Settings: provider.Settings{URL: srv.URL}}, nil)

	for _, tc := range []struct {
		status    int
		retryable bool
		hint      time.Duration
	}{
		{http.StatusTooManyRequests, true, 2 * time.Second},
		{http.StatusInternalServerError, true, 0},
		{http.StatusBadGateway, true, 0},
		{http.StatusBadRequest, false, 0},
		{http.StatusUnauthorized, false, 0},
	} {
		status.Store(int32(tc.status))
		_, cost, err := a.Enrich(context.Background(), provider.Input{Value: "x"})
		require.Error(t, err, tc.status)
		assert.Zero(t, cost, "failed calls cost nothing")
		retry, hint := provider.Retryable(err)
		assert.Equal(t, tc.retryable, retry, tc.status)
		assert.Equal(t, tc.hint, hint, tc.status)
	}
}

func TestRegistry_RejectsDisabledAndUnknownKind(t *testing.T) {
	r := provider.NewRegistry(http.DefaultClient, env(nil))
	_, err := r.Open(&provider.Config{Name: "x", Kind: "github"})
	assert.ErrorContains(t, err, "disabled")
	_, err = r.Open(&provider.Config{Name: "x", Kind: "apollo", Enabled: true})
	assert.ErrorContains(t, err, "unknown kind")
	_, err = r.Open(&provider.Config{Name: "x", Kind: "http", Enabled: true})
	assert.ErrorContains(t, err, "url template")
}

func TestLimiter_SpacesCalls(t *testing.T) {
	l := provider.NewLimiter(50) // 20ms apart
	start := time.Now()
	for range 4 {
		require.NoError(t, l.Wait(context.Background()))
	}
	assert.GreaterOrEqual(t, time.Since(start), 55*time.Millisecond)
	require.NoError(t, provider.NewLimiter(0).Wait(context.Background()))
}
