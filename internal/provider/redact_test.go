package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRedactError(t *testing.T) {
	for in, want := range map[string]string{
		`Get "https://api.example.com/v1/find?key=SECRET&q=acme": dial tcp: connection refused`: `Get "https://api.example.com/v1/find?<redacted>": dial tcp: connection refused`,
		`Post "https://user:pw@host.example/x#frag": EOF`:                                       `Post "https://host.example/x": EOF`,
		`HTTP 401: {"error":"invalid api_key=SECRET"}`:                                          `HTTP 401: {"error":"invalid api_key=<redacted>"}`,
		`HTTP 401: {"token": "SECRET"}`:                                                         `HTTP 401: {"token": "<redacted>"}`,
		`HTTP 403: Authorization: Bearer SECRET rejected`:                                       `HTTP 403: Authorization: Bearer <redacted> rejected`,
		`exa: environment variable EXA_API_KEY is not set`:                                      `exa: environment variable EXA_API_KEY is not set`,
	} {
		assert.Equal(t, want, RedactError(in), in)
		assert.Equal(t, want, RedactError(RedactError(in)), "idempotent: %s", in)
	}
	long := RedactError(strings.Repeat("x", 1000))
	assert.LessOrEqual(t, len(long), MaxErrorBytes+len("…"))
}

// codebase-details M4: a transport error embeds the full request URL, and an
// error body may echo the credential; neither may reach the item error.
func TestCall_ErrorsNeverCarryRequestCredentials(t *testing.T) {
	const secret = "SECRET-KEY-123"
	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:1/find?key="+secret+"&q=acme", http.NoBody)
	require.NoError(t, err)
	_, _, err = call(context.Background(), http.DefaultClient, req)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secret)
	assert.Contains(t, err.Error(), "127.0.0.1:1/find?<redacted>")

	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"key ` + r.URL.Query().Get("key") + ` rejected for ` + r.Header.Get("X-Api-Key") + `"}`))
	}))
	defer echo.Close()
	req, err = http.NewRequest(http.MethodGet, echo.URL+"/find?key="+secret, http.NoBody)
	require.NoError(t, err)
	req.Header.Set("X-Api-Key", "HEADER-SECRET-456")
	_, _, err = call(context.Background(), http.DefaultClient, req)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secret)
	assert.NotContains(t, err.Error(), "HEADER-SECRET-456")
	assert.Contains(t, err.Error(), "HTTP 401")
}
