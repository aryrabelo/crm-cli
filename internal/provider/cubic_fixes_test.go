package provider_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jdanielnd/crm-cli/internal/model"
	"github.com/jdanielnd/crm-cli/internal/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseUSD_RejectsOutOfRange(t *testing.T) {
	for _, bad := range []string{"1e13", "9.3e12", "1e308", "nan", "inf", "-1"} {
		_, err := provider.ParseUSD(bad)
		assert.Error(t, err, bad)
	}
	c, err := provider.ParseUSD("1000000")
	require.NoError(t, err)
	assert.Equal(t, provider.Cost(1_000_000_000_000), c)
}

func TestParseAge_RejectsOverflow(t *testing.T) {
	_, err := provider.ParseAge("110000d")
	assert.Error(t, err)
	d, err := provider.ParseAge("100000d")
	require.NoError(t, err)
	assert.Positive(t, int64(d))
}

func TestValidate_Hardening(t *testing.T) {
	zero := 0.0
	nan := zero / zero
	inf := 1 / zero
	for name, c := range map[string]*provider.Config{
		"nan rate":      {Name: "p", Kind: "jina", Rate: nan},
		"inf rate":      {Name: "p", Kind: "jina", Rate: inf},
		"huge rate":     {Name: "p", Kind: "jina", Rate: 1e9},
		"tiny rate":     {Name: "p", Kind: "jina", Rate: 1e-12},
		"exa no auth":   {Name: "p", Kind: "exa"},
		"dotted name":   {Name: "foo.bar", Kind: "jina"},
		"upper name":    {Name: "Foo", Kind: "jina"},
		"underscore":    {Name: "foo_bar", Kind: "jina"},
		"max tokens":    {Name: "p", Kind: "llm", Settings: provider.Settings{MaxTokens: 1 << 40}},
		"huge price":    {Name: "p", Kind: "llm", Settings: provider.Settings{PriceInPerMTok: f64(1e308)}},
		"price too big": {Name: "p", Kind: "llm", Settings: provider.Settings{PriceOutPerMTok: f64(2e6)}},
	} {
		assert.ErrorIs(t, c.Validate(), model.ErrValidation, name)
	}
	for name, c := range map[string]*provider.Config{
		"exa":          {Name: "exa", Kind: "exa", AuthRef: "EXA_API_KEY", Rate: 5},
		"exa-contents": {Name: "exa-contents", Kind: "exa", AuthRef: "EXA_API_KEY"},
		"unlimited":    {Name: "p", Kind: "jina"},
	} {
		assert.NoError(t, c.Validate(), name)
	}
	assert.NotEqual(t, provider.URLOverrideEnv("foo-bar"), provider.URLOverrideEnv("foobar"))
}

func TestOpenLLM_RejectsOutOfRangeEnvPrice(t *testing.T) {
	cfg := &provider.Config{Name: "llm", Kind: "llm", Enabled: true, Settings: provider.Settings{PriceOutPerMTok: f64(1)}}
	_, err := provider.NewRegistry(http.DefaultClient, env(map[string]string{
		"CRM_LLM_MODEL": "m", "CRM_LLM_PRICE_IN": "1e308",
	})).OpenLLM(cfg)
	assert.ErrorIs(t, err, model.ErrValidation)
}

func TestGitHub_LoginIgnoresQueryAndFragment(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		_, _ = w.Write([]byte(`{"login":"octocat"}`))
	}))
	defer srv.Close()
	adapter := open(t, &provider.Config{Name: "github", Kind: "github", Settings: provider.Settings{BaseURL: srv.URL}}, nil)
	for _, in := range []string{"https://github.com/octocat?tab=repositories", "github.com/octocat#readme", "@Octocat"} {
		_, _, err := adapter.Enrich(context.Background(), provider.Input{Value: in})
		require.NoError(t, err, in)
		assert.Equal(t, adapter.Key(provider.Input{Value: "octocat"}), adapter.Key(provider.Input{Value: in}), in)
	}
	assert.Equal(t, []string{"/users/octocat", "/users/octocat", "/users/octocat"}, paths)
}

func TestGitHub_NonJSONIsNotCharged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>oops`))
	}))
	defer srv.Close()
	adapter := open(t, &provider.Config{Name: "github", Kind: "github", Price: 10, Settings: provider.Settings{BaseURL: srv.URL}}, nil)
	_, cost, err := adapter.Enrich(context.Background(), provider.Input{Value: "octocat"})
	require.Error(t, err)
	assert.Zero(t, cost)
}

func TestCall_SchemelessURLIsNotRetryable(t *testing.T) {
	h := open(t, &provider.Config{Name: "h", Kind: "http", Price: 10, Settings: provider.Settings{URL: "example.com/?q={{input}}"}}, nil)
	_, _, err := h.Enrich(context.Background(), provider.Input{Value: "x"})
	require.Error(t, err)
	retry, _ := provider.Retryable(err)
	assert.False(t, retry)
	assert.Contains(t, err.Error(), "http://")
}

func TestCall_OversizedResponseIsExplicit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`"` + strings.Repeat("a", 16<<20) + `"`))
	}))
	defer srv.Close()
	h := open(t, &provider.Config{Name: "h", Kind: "http", Settings: provider.Settings{URL: srv.URL}}, nil)
	_, _, err := h.Enrich(context.Background(), provider.Input{Value: "x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "response exceeds 16 MiB")
	retry, _ := provider.Retryable(err)
	assert.False(t, retry)
}

func TestHTTP_ResponseErrorRedactedAndStringBooleans(t *testing.T) {
	body := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	ctx := context.Background()

	errPath := open(t, &provider.Config{Name: "h", Kind: "http", AuthRef: "H_KEY", Price: 10,
		Settings: provider.Settings{URL: srv.URL, ErrorPath: "error", AuthHeader: "X-Api-Key"}}, map[string]string{"H_KEY": "SUPERSECRET-123"})
	body = `{"error":"bad key SUPERSECRET-123"}`
	_, _, err := errPath.Enrich(ctx, provider.Input{Value: "x"})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "SUPERSECRET-123")

	okPath := open(t, &provider.Config{Name: "h", Kind: "http", Price: 10, Settings: provider.Settings{URL: srv.URL, OKPath: "success"}}, nil)
	for _, v := range []string{`"false"`, `"0"`, `" False "`, `false`, `0`} {
		body = `{"success":` + v + `}`
		_, cost, err := okPath.Enrich(ctx, provider.Input{Value: "x"})
		assert.Error(t, err, v)
		assert.Equal(t, provider.Cost(10), cost, v)
	}
	for _, v := range []string{`"true"`, `"yes"`, `1`, `true`, `12345678901234567890`} {
		body = `{"success":` + v + `}`
		_, _, err := okPath.Enrich(ctx, provider.Input{Value: "x"})
		assert.NoError(t, err, v)
	}

	// A 2xx body that is not JSON was served, so it is charged.
	body = `<html>`
	_, cost, err := okPath.Enrich(ctx, provider.Input{Value: "x"})
	require.Error(t, err)
	assert.Equal(t, provider.Cost(10), cost)
}

func TestHTTP_KeyDependsOnErrorAndOKPath(t *testing.T) {
	mk := func(s provider.Settings) provider.Hash {
		s.URL = "http://x/{{input}}"
		return open(t, &provider.Config{Name: "h", Kind: "http", Settings: s}, nil).Key(provider.Input{Value: "a"})
	}
	base := mk(provider.Settings{})
	assert.NotEqual(t, base, mk(provider.Settings{ErrorPath: "error"}))
	assert.NotEqual(t, base, mk(provider.Settings{OKPath: "ok"}))
}

func TestRedact_CompoundKeysBodySecretsAndBoundary(t *testing.T) {
	for _, in := range []string{
		`client_secret=SECRETVALUE`, `{"refresh_token":"SECRETVALUE"}`, `x_api_key: SECRETVALUE`, `DB_PASSWORD=SECRETVALUE`,
	} {
		out := provider.RedactError(in)
		assert.NotContains(t, out, "SECRETVALUE", in)
		assert.Equal(t, out, provider.RedactError(out), "idempotent: %s", in)
	}

	// Secret in the POST body only, echoed by the error response straddling
	// the 200-byte snippet boundary.
	secret := "BODYONLY-SECRET-987"
	pad := strings.Repeat("p", 190)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(pad + secret + " tail"))
	}))
	defer srv.Close()
	h := open(t, &provider.Config{Name: "h", Kind: "http",
		Settings: provider.Settings{URL: srv.URL, Body: `{"token_value":"` + secret + `","q":"{{input}}"}`}}, nil)
	_, _, err := h.Enrich(context.Background(), provider.Input{Value: "x"})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secret)
	assert.NotContains(t, err.Error(), "BODYONLY")
}

func TestLLM_UsageValidationAndEstimate(t *testing.T) {
	for name, usage := range map[string]string{
		"empty":    `{}`,
		"negative": `{"prompt_tokens":-1000000,"completion_tokens":-1000000}`,
		"partial":  `{"prompt_tokens":1}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"value\":\"yes\"}"}}],"usage":` + usage + `}`))
		}))
		a := openLLM(t, srv.URL).Bind(yesNo)
		ctx, u := provider.WithUsage(context.Background())
		_, cost, err := a.Enrich(ctx, provider.Input{Value: "hi"})
		srv.Close()
		require.NoError(t, err, name)
		assert.Positive(t, int64(cost), name)
		in, out := u.Tokens()
		assert.Positive(t, in, name)
		assert.EqualValues(t, 50, out, name) // worst case: max tokens
	}

	// Very cheap tokens: the estimate must not undercut two separately
	// rounded calls.
	cfg := &provider.Config{Name: "llm", Kind: "llm", Enabled: true,
		Settings: provider.Settings{PriceInPerMTok: f64(0.000001), PriceOutPerMTok: f64(0.000001), MaxTokens: 1}}
	l, err := provider.NewRegistry(http.DefaultClient, env(map[string]string{"CRM_LLM_MODEL": "m"})).OpenLLM(cfg)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, int64(l.Bind(yesNo).Estimate(provider.Input{Value: "x"})), int64(2))
}

func TestLimiter_ClampsInterval(t *testing.T) {
	assert.NoError(t, provider.NewLimiter(1e12).Wait(context.Background()))
	assert.NotPanics(t, func() { provider.NewLimiter(1e-300) })
}
