package bench

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/jdanielnd/crm-cli/internal/provider"
)

// Fake provider settings: tasks refer to the enrich provider by FirmoName
// and to the llm provider "llm".
const (
	FirmoName  = "firmo"
	firmoPrice = "0.002"
	llmModel   = "bench-model"
	llmKey     = "sk-bench"
	// Token prices (USD per 1M) of the fake llm, gpt-4o-mini-like.
	llmPriceIn  = "0.15"
	llmPriceOut = "0.60"
	// llmCompletionTokens is the completion size the fake llm reports.
	llmCompletionTokens = 30
)

var firmoIndustries = []string{"Software", "Finance", "Retail", "Media", "Logistics", "Health"}

// Fakes are local HTTP providers: firmo answers company data for a domain
// (404 for .org and .fr domains), the llm is an OpenAI-compatible chat
// endpoint that judges a row by its description ("B2B" → yes / 90,
// "consumer" → no / 10, otherwise maybe / 50).
type Fakes struct {
	srv *httptest.Server
}

// NewFakes starts the fake providers. Close stops them.
func NewFakes() *Fakes {
	mux := http.NewServeMux()
	mux.HandleFunc("/company", firmo)
	mux.HandleFunc("/v1/chat/completions", fakeLLM)
	return &Fakes{srv: httptest.NewServer(mux)}
}

// Close stops the fake providers.
func (f *Fakes) Close() { f.srv.Close() }

// Registry returns a provider registry whose llm settings point at the fake.
func (f *Fakes) Registry() *provider.Registry {
	env := map[string]string{
		provider.EnvLLMBaseURL:  f.srv.URL + "/v1",
		provider.EnvLLMModel:    llmModel,
		provider.EnvLLMPriceIn:  llmPriceIn,
		provider.EnvLLMPriceOut: llmPriceOut,
		"CRM_LLM_API_KEY":       llmKey,
	}
	return provider.NewRegistry(&http.Client{Timeout: 10 * time.Second}, func(k string) string { return env[k] })
}

// Provision saves the firmo provider in db.
func (f *Fakes) Provision(ctx context.Context, db *sql.DB) error {
	price, err := provider.ParseUSD(firmoPrice)
	if err != nil {
		return err
	}
	return provider.NewStore(db).Save(ctx, &provider.Config{Name: FirmoName, Kind: "http", Price: price, Enabled: true,
		Settings: provider.Settings{URL: f.srv.URL + "/company?domain={{input}}"}})
}

func hashOf(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}

func firmo(w http.ResponseWriter, r *http.Request) {
	domain := strings.ToLower(r.URL.Query().Get("domain"))
	if domain == "" || strings.HasSuffix(domain, ".org") || strings.HasSuffix(domain, ".fr") {
		http.NotFound(w, r)
		return
	}
	h := hashOf(domain)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"domain":    domain,
		"industry":  firmoIndustries[h%uint32(len(firmoIndustries))],
		"employees": 10 + h%990,
	})
}

func fakeLLM(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Messages) < 2 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+llmKey {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	system, user := req.Messages[0].Content, req.Messages[1].Content
	// A qualify prompt lists the row as "- field: value" lines after the
	// criteria; judge the description line only.
	row := user
	for _, line := range strings.Split(user, "\n") {
		if d, ok := strings.CutPrefix(line, "- description: "); ok {
			row = d
		}
	}
	decision, score := "maybe", 50
	switch {
	case strings.Contains(row, "B2B"):
		decision, score = "yes", 90
	case strings.Contains(row, "consumer"):
		decision, score = "no", 10
	}
	answer := map[string]any{"value": score, "reason": fmt.Sprintf("The description reads %q.", row)}
	if strings.Contains(system, "maybe") {
		answer["value"] = decision
	}
	content, _ := json.Marshal(answer)
	size := 0
	for _, m := range req.Messages {
		size += len(m.Content)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": string(content)}}},
		"usage":   map[string]any{"prompt_tokens": size / 4, "completion_tokens": llmCompletionTokens},
	})
}
