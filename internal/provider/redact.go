package provider

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// MaxErrorBytes caps an item error as stored in run_items.error and shown by
// crm run show --items and crm_run_status.
const MaxErrorBytes = 300

const redacted = "<redacted>"

var (
	urlPattern = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s"'<>]+`)
	// secretPattern catches credentials echoed in error bodies: "Bearer x",
	// "api_key=x", `"token": "x"`, "password: x".
	secretPattern = regexp.MustCompile(`(?i)(bearer\s+|[a-z0-9_-]*(?:api[_-]?key|apikey|access[_-]?token|token|secret|password|passwd|key)"?\s*[:=]\s*"?)([^\s"',;&}]+)`)
)

// RedactError makes provider error text safe to persist and return: URLs
// lose their userinfo, query string and fragment, credential-looking values
// ("Bearer x", "api_key=x", `"token":"x"`) are masked, and the text is
// capped at MaxErrorBytes.
func RedactError(s string) string {
	return truncate(redactText(s), MaxErrorBytes)
}

func redactText(s string) string {
	s = urlPattern.ReplaceAllStringFunc(s, redactURL)
	return secretPattern.ReplaceAllString(s, "${1}"+redacted)
}

func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		scheme, _, _ := strings.Cut(raw, "://")
		return scheme + "://" + redacted
	}
	u.User = nil
	// An empty query ("x?" — or "x?<redacted>", where the match stops at
	// '<') is left alone, so redacting twice changes nothing.
	if u.RawQuery != "" {
		u.RawQuery = redacted
	}
	u.Fragment, u.RawFragment = "", ""
	return u.String()
}

// requestSecrets lists the values of req that may carry a credential: query
// parameter values, URL userinfo, string values of a JSON body and header
// values other than content negotiation.
func requestSecrets(req *http.Request) []string {
	var out []string
	for _, vs := range req.URL.Query() {
		out = append(out, vs...)
	}
	if req.URL.User != nil {
		out = append(out, req.URL.User.Username())
		if p, ok := req.URL.User.Password(); ok {
			out = append(out, p)
		}
	}
	if req.GetBody != nil {
		if b, err := req.GetBody(); err == nil {
			raw, _ := io.ReadAll(io.LimitReader(b, 1<<20))
			b.Close()
			var doc any
			if json.Unmarshal(raw, &doc) == nil {
				out = appendStrings(out, doc)
			}
		}
	}
	for name, vs := range req.Header {
		if name == "Accept" || name == "Content-Type" {
			continue
		}
		for _, v := range vs {
			out = append(out, v)
			if _, tok, ok := strings.Cut(v, " "); ok { // "Bearer <token>"
				out = append(out, tok)
			}
		}
	}
	// Longest first, so a token is masked before any shorter value it contains.
	slices.SortFunc(out, func(a, b string) int { return len(b) - len(a) })
	return out
}

// appendStrings adds every string leaf of a decoded JSON document to out.
func appendStrings(out []string, v any) []string {
	switch x := v.(type) {
	case string:
		out = append(out, x)
	case []any:
		for _, e := range x {
			out = appendStrings(out, e)
		}
	case map[string]any:
		for _, e := range x {
			out = appendStrings(out, e)
		}
	}
	return out
}

// redactRequest applies RedactError to s and also masks every credential
// value of req that still appears in it (a key echoed in an error body).
func redactRequest(s string, req *http.Request) string {
	s = redactText(s)
	for _, secret := range requestSecrets(req) {
		if len(secret) >= 4 {
			s = strings.ReplaceAll(s, secret, redacted)
			if esc := url.QueryEscape(secret); esc != secret {
				s = strings.ReplaceAll(s, esc, redacted)
			}
		}
	}
	return truncate(s, MaxErrorBytes)
}
