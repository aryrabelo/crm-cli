package provider_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jdanielnd/crm-cli/internal/model"
	"github.com/jdanielnd/crm-cli/internal/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type chatReq struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	MaxTokens      int `json:"max_tokens"`
	ResponseFormat struct {
		Type string `json:"type"`
	} `json:"response_format"`
}

// fakeLLM answers chat completions with replies[i] for request i (the last
// reply repeats), reporting usage of 100 prompt and 10 completion tokens.
func fakeLLM(t *testing.T, replies ...string) (*httptest.Server, *[]chatReq, *atomic.Int64) {
	t.Helper()
	var reqs []chatReq
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/chat/completions", r.URL.Path)
		assert.Equal(t, "Bearer sk-test", r.Header.Get("Authorization"))
		var req chatReq
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		reqs = append(reqs, req)
		i := int(n.Add(1)) - 1
		reply := replies[min(i, len(replies)-1)]
		enc, _ := json.Marshal(reply)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":` + string(enc) +
			`}}],"usage":{"prompt_tokens":100,"completion_tokens":10}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &reqs, &n
}

func f64(f float64) *float64 { return &f }

func openLLM(t *testing.T, base string) *provider.LLM {
	t.Helper()
	cfg := &provider.Config{Name: "llm", Kind: "llm", AuthRef: "CRM_LLM_API_KEY", Enabled: true,
		Settings: provider.Settings{PriceInPerMTok: f64(2), PriceOutPerMTok: f64(10), MaxTokens: 50}}
	l, err := provider.NewRegistry(http.DefaultClient, env(map[string]string{
		"CRM_LLM_BASE_URL": base + "/v1", "CRM_LLM_MODEL": "test-model", "CRM_LLM_API_KEY": "sk-test",
	})).OpenLLM(cfg)
	require.NoError(t, err)
	return l
}

var yesNo = provider.Contract{
	Instructions: `Reply with JSON {"value": "yes" | "no"}`,
	Check: func(a map[string]any) (any, error) {
		if v, _ := a["value"].(string); v == "yes" || v == "no" {
			return map[string]any{"value": v}, nil
		}
		return nil, errors.New(`"value" must be yes or no`)
	},
}

func TestLLM_AnswerCostUsageAndKey(t *testing.T) {
	srv, reqs, _ := fakeLLM(t, "```json\n{\"value\": \"yes\"}\n```")
	a := openLLM(t, srv.URL).Bind(yesNo)
	ctx, usage := provider.WithUsage(context.Background())
	in := provider.Input{Value: "Is Acme a fit?"}

	out, cost, err := a.Enrich(ctx, in)
	require.NoError(t, err)
	assert.JSONEq(t, `{"value":"yes"}`, string(out), "a code fence around the object is tolerated")
	// 100 in × $2/M + 10 out × $10/M = $0.0003
	assert.Equal(t, provider.Cost(300), cost)
	tin, tout := usage.Tokens()
	assert.Equal(t, [2]int64{100, 10}, [2]int64{tin, tout})
	assert.GreaterOrEqual(t, a.Estimate(in), cost)

	require.Len(t, *reqs, 1)
	req := (*reqs)[0]
	assert.Equal(t, "test-model", req.Model)
	assert.Equal(t, 50, req.MaxTokens)
	assert.Equal(t, "json_object", req.ResponseFormat.Type)
	require.Len(t, req.Messages, 2)
	assert.Equal(t, "system", req.Messages[0].Role)
	assert.Equal(t, "Is Acme a fit?", req.Messages[1].Content)

	other := openLLM(t, srv.URL).Bind(provider.Contract{Instructions: "other", Check: yesNo.Check})
	assert.NotEqual(t, a.Key(in), other.Key(in), "the contract is part of the cache key")
	assert.NotEqual(t, a.Key(in), a.Key(provider.Input{Value: "Is Beta a fit?"}))
}

func TestLLM_InvalidJSONIsRepairedOnce(t *testing.T) {
	srv, reqs, _ := fakeLLM(t, "sure! yes", `{"value":"no"}`)
	a := openLLM(t, srv.URL).Bind(yesNo)
	out, cost, err := a.Enrich(context.Background(), provider.Input{Value: "q"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"value":"no"}`, string(out))
	assert.Equal(t, provider.Cost(600), cost, "both calls are charged")
	require.Len(t, *reqs, 2)
	repair := (*reqs)[1].Messages
	require.Len(t, repair, 4)
	assert.Equal(t, "assistant", repair[2].Role)
	assert.Equal(t, "sure! yes", repair[2].Content)
	assert.Contains(t, repair[3].Content, "not a JSON object")
}

func TestLLM_InvalidTwiceFailsWithoutRetry(t *testing.T) {
	srv, _, calls := fakeLLM(t, `{"value":"perhaps"}`)
	a := openLLM(t, srv.URL).Bind(yesNo)
	in := provider.Input{Value: "q"}
	_, cost, err := a.Enrich(context.Background(), in)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid answer after one repair")
	retry, _ := provider.Retryable(err)
	assert.False(t, retry)
	assert.EqualValues(t, 2, calls.Load())
	assert.Equal(t, provider.Cost(600), cost)
	assert.GreaterOrEqual(t, a.Estimate(in), cost, "the estimate covers the repair call")
}

func TestLLM_EstimateBoundsLongPrompts(t *testing.T) {
	// The provider reports one token per prompt byte and max tokens out: the
	// worst a byte-level tokenizer can do, with a repair.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		n := 0
		for _, m := range req.Messages {
			n += len(m.Content)
		}
		enc, _ := json.Marshal(strings.Repeat("x", 50))
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":` + string(enc) + `}}],"usage":{"prompt_tokens":` +
			itoa(n) + `,"completion_tokens":50}}`))
	}))
	defer srv.Close()
	a := openLLM(t, srv.URL).Bind(yesNo)
	in := provider.Input{Value: strings.Repeat("é", 2000)}
	_, cost, err := a.Enrich(context.Background(), in)
	require.Error(t, err)
	assert.Greater(t, cost, provider.Cost(0))
	assert.GreaterOrEqual(t, a.Estimate(in), cost)
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestLLM_ConfigurationErrors(t *testing.T) {
	reg := provider.NewRegistry(http.DefaultClient, env(nil))
	base := provider.Config{Name: "llm", Kind: "llm", Enabled: true}

	_, err := reg.OpenLLM(&base)
	assert.ErrorIs(t, err, model.ErrValidation, "no model")

	withModel := base
	withModel.Settings.Model = "m"
	_, err = reg.OpenLLM(&withModel)
	assert.ErrorIs(t, err, model.ErrValidation, "no price")
	assert.Contains(t, err.Error(), "--price-in")

	l, err := provider.NewRegistry(http.DefaultClient, env(map[string]string{
		"CRM_LLM_MODEL": "m", "CRM_LLM_PRICE_IN": "0", "CRM_LLM_PRICE_OUT": "0",
	})).OpenLLM(&base)
	require.NoError(t, err, "model and prices from the environment")
	assert.Equal(t, provider.Cost(0), l.Bind(yesNo).Estimate(provider.Input{Value: "q"}), "free model")

	_, err = reg.Open(&withModel)
	assert.ErrorIs(t, err, model.ErrValidation, "llm providers do not serve enrich waterfalls")
	_, err = reg.OpenLLM(&provider.Config{Name: "github", Kind: "github", Enabled: true})
	assert.ErrorIs(t, err, model.ErrValidation)

	perCall := withModel
	perCall.Price = 5000
	assert.ErrorIs(t, perCall.Validate(), model.ErrValidation, "llm prices are per token")
}

func TestLLM_HTTPErrors(t *testing.T) {
	status := http.StatusTooManyRequests
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
	defer srv.Close()
	a := openLLM(t, srv.URL).Bind(yesNo)
	_, _, err := a.Enrich(context.Background(), provider.Input{Value: "q"})
	retry, _ := provider.Retryable(err)
	assert.True(t, retry, "429 is retried by the run")

	status = http.StatusNotFound
	_, _, err = a.Enrich(context.Background(), provider.Input{Value: "q"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "check the base URL and model")
}
