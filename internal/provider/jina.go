package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// jina reads a web page through Jina Reader (https://r.jina.ai/<url>). The
// answer is the "data" object ({title, url, content, ...}). The API key is
// optional (higher rate limits).
type jina struct {
	client *http.Client
	base   string
	price  Cost
	token  string
}

func newJina(client *http.Client, cfg *Config, base, token string) *jina {
	return &jina{client: client, base: base, price: cfg.Price, token: token}
}

func (j *jina) Estimate(Input) Cost { return j.price }

func (j *jina) Key(in Input) Hash {
	return hashKey("jina", j.base, withScheme(strings.TrimSpace(in.Value)))
}

func (j *jina) Enrich(ctx context.Context, in Input) (Output, Cost, error) {
	req, err := http.NewRequest(http.MethodGet, j.base+"/"+withScheme(strings.TrimSpace(in.Value)), http.NoBody)
	if err != nil {
		return nil, 0, fmt.Errorf("build jina request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if j.token != "" {
		req.Header.Set("Authorization", "Bearer "+j.token)
	}
	resp, found, err := call(ctx, j.client, req)
	if err != nil {
		return nil, 0, err
	}
	if !found {
		return nullOutput, j.price, nil
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(resp, &envelope); err != nil {
		return nil, 0, &Error{Msg: "jina: decode response: " + err.Error()}
	}
	if len(envelope.Data) == 0 {
		return nullOutput, j.price, nil
	}
	return Output(envelope.Data), j.price, nil
}
