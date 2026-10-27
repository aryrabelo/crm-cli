package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/jdanielnd/crm-cli/internal/value"
)

// httpAdapter is the generic provider: a URL template (and optional body
// template) with {{input}}, plus a dotted JSON path selecting the answer.
type httpAdapter struct {
	client     *http.Client
	name       string
	urlTmpl    string
	method     string
	bodyTmpl   string
	path       string
	errorPath  string
	okPath     string
	authRef    string
	authHeader string
	price      Cost
	token      string
}

func newHTTP(client *http.Client, cfg *Config, urlTmpl, token string) *httpAdapter {
	h := &httpAdapter{
		client:     client,
		name:       cfg.Name,
		urlTmpl:    urlTmpl,
		method:     strings.ToUpper(cfg.Settings.Method),
		bodyTmpl:   cfg.Settings.Body,
		path:       cfg.Settings.Path,
		errorPath:  cfg.Settings.ErrorPath,
		okPath:     cfg.Settings.OKPath,
		authRef:    cfg.AuthRef,
		authHeader: cfg.Settings.AuthHeader,
		price:      cfg.Price,
		token:      token,
	}
	if h.method == "" {
		h.method = http.MethodGet
		if h.bodyTmpl != "" {
			h.method = http.MethodPost
		}
	}
	return h
}

func (h *httpAdapter) Estimate(Input) Cost { return h.price }

func (h *httpAdapter) Key(in Input) Hash {
	return hashKey("http", h.method, h.urlTmpl, h.bodyTmpl, h.path, strings.TrimSpace(in.Value))
}

// responseError reports a 2xx response that the provider marks as an error
// (error_path set, or ok_path not set), or nil. The call was served, so it is
// still charged; the answer is never cached.
func (h *httpAdapter) responseError(doc any) error {
	if h.errorPath != "" {
		if v, ok := value.Lookup(doc, h.errorPath); ok && truthy(v) {
			return &Error{Msg: fmt.Sprintf("%s: error in response (%s: %s)", h.name, h.errorPath, value.Text(v))}
		}
	}
	if h.okPath != "" {
		if v, ok := value.Lookup(doc, h.okPath); !ok || !truthy(v) {
			return &Error{Msg: fmt.Sprintf("%s: response not ok (%s: %s)", h.name, h.okPath, value.Text(v))}
		}
	}
	return nil
}

// truthy reports whether v is set: not empty, false or 0.
func truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case float64:
		return x != 0
	default:
		return !value.Empty(v)
	}
}

func (h *httpAdapter) Enrich(ctx context.Context, in Input) (Output, Cost, error) {
	if h.authRef != "" && h.token == "" {
		return nil, 0, errMissingAuth(h.name, h.authRef)
	}
	v := strings.TrimSpace(in.Value)
	target := strings.ReplaceAll(h.urlTmpl, "{{input}}", url.QueryEscape(v))
	var body io.Reader = http.NoBody
	if h.bodyTmpl != "" {
		escaped, err := json.Marshal(v)
		if err != nil {
			return nil, 0, fmt.Errorf("encode %s input: %w", h.name, err)
		}
		inner := string(escaped[1 : len(escaped)-1])
		body = strings.NewReader(strings.ReplaceAll(h.bodyTmpl, "{{input}}", inner))
	}
	req, err := http.NewRequest(h.method, target, body)
	if err != nil {
		return nil, 0, &Error{Msg: fmt.Sprintf("%s: build request: %v", h.name, err)}
	}
	req.Header.Set("Accept", "application/json")
	if h.bodyTmpl != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if h.token != "" {
		if h.authHeader == "" {
			req.Header.Set("Authorization", "Bearer "+h.token)
		} else {
			req.Header.Set(h.authHeader, h.token)
		}
	}

	resp, found, err := call(ctx, h.client, req)
	if err != nil {
		return nil, 0, err
	}
	if !found {
		return nullOutput, h.price, nil
	}
	doc, err := value.Decode(resp)
	if err != nil {
		return nil, 0, &Error{Msg: fmt.Sprintf("%s: decode response: %v", h.name, err)}
	}
	if err := h.responseError(doc); err != nil {
		return nil, h.price, err
	}
	selected, ok := value.Lookup(doc, h.path)
	if !ok {
		return nullOutput, h.price, nil
	}
	out, err := json.Marshal(selected)
	if err != nil {
		return nil, 0, fmt.Errorf("encode %s answer: %w", h.name, err)
	}
	return Output(out), h.price, nil
}
