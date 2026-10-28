package provider_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jdanielnd/crm-cli/internal/model"
	"github.com/jdanielnd/crm-cli/internal/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Design review P1-8: an API that reports errors with HTTP 200 must not have
// its error body taken (and cached) as the answer.
func TestHTTP_ErrorPathAndOKPath(t *testing.T) {
	body := `{"error":"quota exceeded","success":false}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	ctx := context.Background()

	plain := open(t, &provider.Config{Name: "h", Kind: "http", Price: 10, Settings: provider.Settings{URL: srv.URL}}, nil)
	out, _, err := plain.Enrich(ctx, provider.Input{Value: "x"})
	require.NoError(t, err)
	assert.JSONEq(t, body, string(out), "without error_path the error body is the answer")

	errPath := open(t, &provider.Config{Name: "h", Kind: "http", Price: 10,
		Settings: provider.Settings{URL: srv.URL, ErrorPath: "error"}}, nil)
	_, cost, err := errPath.Enrich(ctx, provider.Input{Value: "x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "error in response (error: quota exceeded)")
	assert.Equal(t, provider.Cost(10), cost, "the call was served, so it is charged")
	retry, _ := provider.Retryable(err)
	assert.False(t, retry)

	okPath := open(t, &provider.Config{Name: "h", Kind: "http", Price: 10,
		Settings: provider.Settings{URL: srv.URL, OKPath: "success"}}, nil)
	_, _, err = okPath.Enrich(ctx, provider.Input{Value: "x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "response not ok (success: false)")

	body = `{"error":null,"success":true,"data":"Acme"}`
	for _, a := range []provider.Adapter{errPath, okPath} {
		out, _, err := a.Enrich(ctx, provider.Input{Value: "x"})
		require.NoError(t, err)
		assert.JSONEq(t, body, string(out))
	}
}

func TestSettings_TTL(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"": provider.DefaultCacheTTL, "0": 0, "7d": 7 * 24 * time.Hour, "12h": 12 * time.Hour,
	} {
		got, err := provider.Settings{CacheTTL: in}.TTL()
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	for _, bad := range []string{"-1h", "xd", "soon"} {
		_, err := provider.Settings{CacheTTL: bad}.TTL()
		assert.Error(t, err, bad)
	}
	err := (&provider.Config{Name: "e", Kind: "exa", Settings: provider.Settings{ErrorPath: "error"}}).Validate()
	assert.ErrorIs(t, err, model.ErrValidation)
}
