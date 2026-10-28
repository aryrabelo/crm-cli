package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// exa calls the Exa API (https://docs.exa.ai): /search with the input as the
// query, or /contents with the input as a URL. The answer is the results array.
type exa struct {
	client     *http.Client
	name       string
	base       string
	endpoint   string
	numResults int
	price      Cost
	authRef    string
	token      string
}

func newExa(client *http.Client, cfg *Config, base, token string) *exa {
	e := &exa{
		client:     client,
		name:       cfg.Name,
		base:       base,
		endpoint:   cfg.Settings.Endpoint,
		numResults: cfg.Settings.NumResults,
		price:      cfg.Price,
		authRef:    cfg.AuthRef,
		token:      token,
	}
	if e.endpoint == "" {
		e.endpoint = "search"
	}
	if e.numResults <= 0 {
		e.numResults = 5
	}
	return e
}

func (e *exa) input(in Input) string {
	v := strings.TrimSpace(in.Value)
	if e.endpoint == "contents" {
		return withScheme(v)
	}
	return v
}

func (e *exa) Estimate(Input) Cost { return e.price }

func (e *exa) Key(in Input) Hash {
	return hashKey("exa", e.base, e.endpoint, strconv.Itoa(e.numResults), e.input(in))
}

func (e *exa) Enrich(ctx context.Context, in Input) (Output, Cost, error) {
	if e.token == "" {
		return nil, 0, errMissingAuth(e.name, e.authRef)
	}
	var payload any
	if e.endpoint == "contents" {
		payload = map[string]any{"urls": []string{e.input(in)}, "text": true}
	} else {
		payload = map[string]any{"query": e.input(in), "numResults": e.numResults}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, fmt.Errorf("encode exa request: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, e.base+"/"+e.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("build exa request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-api-key", e.token)

	resp, found, err := call(ctx, e.client, req)
	if err != nil {
		return nil, 0, err
	}
	if !found {
		return nullOutput, e.price, nil
	}
	var envelope struct {
		Results json.RawMessage `json:"results"`
	}
	if err := json.Unmarshal(resp, &envelope); err != nil {
		return nil, 0, &Error{Msg: "exa: decode response: " + err.Error()}
	}
	if len(envelope.Results) == 0 {
		return nullOutput, e.price, nil
	}
	return Output(envelope.Results), e.price, nil
}

// withScheme prefixes bare domains ("acme.com") with https://.
func withScheme(v string) string {
	if v == "" || strings.Contains(v, "://") {
		return v
	}
	return "https://" + v
}
