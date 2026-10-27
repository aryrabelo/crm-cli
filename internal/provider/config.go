package provider

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jdanielnd/crm-cli/internal/db/repo"
	"github.com/jdanielnd/crm-cli/internal/model"
)

// Kinds lists the adapter implementations a provider row can use. llm
// providers serve ai columns (see Registry.OpenLLM); the others serve enrich
// waterfalls (Registry.Open).
func Kinds() []string { return []string{"exa", "jina", "github", "http", "llm"} }

// Settings is the kind-specific part of a provider's configuration.
type Settings struct {
	// BaseURL overrides the API root (exa, jina, github, llm). For every kind
	// the environment variable CRM_PROVIDER_<NAME>_URL takes precedence.
	BaseURL string `json:"base_url,omitempty"`
	// Endpoint selects the exa API: "search" (default) or "contents".
	Endpoint string `json:"endpoint,omitempty"`
	// NumResults is the exa search result count (default 5).
	NumResults int `json:"num_results,omitempty"`
	// Resource selects the github API: "users" (default; answers orgs too) or "orgs".
	Resource string `json:"resource,omitempty"`
	// URL is the http request URL template; {{input}} is replaced by the
	// query-escaped input.
	URL string `json:"url,omitempty"`
	// Method is the http method (default GET, or POST when Body is set).
	Method string `json:"method,omitempty"`
	// Body is the http request body template; {{input}} is replaced by the
	// JSON-string-escaped input (without quotes).
	Body string `json:"body,omitempty"`
	// Path selects the result inside the http JSON response ("data.email").
	Path string `json:"path,omitempty"`
	// AuthHeader is the http header carrying the credential named by
	// auth_ref. Empty means "Authorization: Bearer <token>".
	AuthHeader string `json:"auth_header,omitempty"`
	// ErrorPath (http) marks a 2xx response as an error when this dotted
	// path holds a value that is not empty, false or 0 ("error",
	// "errors.0.message"): the answer is not cached and the item fails.
	ErrorPath string `json:"error_path,omitempty"`
	// OKPath (http) marks a 2xx response as an error unless this dotted path
	// holds a value that is not empty, false or 0 ("success", "status").
	OKPath string `json:"ok_path,omitempty"`
	// Model is the llm model (default: $CRM_LLM_MODEL).
	Model string `json:"model,omitempty"`
	// PriceInPerMTok and PriceOutPerMTok are the llm prices in USD per 1M
	// prompt / completion tokens (default: $CRM_LLM_PRICE_IN / _OUT). nil is
	// unset; 0 is free (local model, subscription gateway).
	PriceInPerMTok  *float64 `json:"price_in_per_mtok,omitempty"`
	PriceOutPerMTok *float64 `json:"price_out_per_mtok,omitempty"`
	// MaxTokens caps the completion tokens of one llm call (default 512).
	MaxTokens int `json:"max_tokens,omitempty"`
	// CacheTTL is how long a cached answer of this provider is reused: a
	// duration ("720h", "30d"); "0" never expires; empty is DefaultCacheTTL.
	CacheTTL string `json:"cache_ttl,omitempty"`
}

// DefaultCacheTTL is how long cached answers are reused when a provider sets
// no cache_ttl. Older entries are treated as missing, so the provider is
// called (and paid) again and the entry refreshed.
const DefaultCacheTTL = 30 * 24 * time.Hour

// TTL returns the cache lifetime of the provider's answers; 0 means they
// never expire.
func (s Settings) TTL() (time.Duration, error) {
	if s.CacheTTL == "" {
		return DefaultCacheTTL, nil
	}
	return ParseAge(s.CacheTTL)
}

// ParseAge parses a non-negative duration: Go syntax ("36h", "90m") or whole
// days ("30d").
func ParseAge(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("invalid duration %q (use e.g. 30d, 12h or 0)", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("invalid duration %q (use e.g. 30d, 12h or 0)", s)
	}
	return d, nil
}

// Config is one row of the providers table.
type Config struct {
	ID        int64    `json:"id"`
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	AuthRef   string   `json:"auth_ref"`
	Rate      float64  `json:"rate"`
	Price     Cost     `json:"-"`
	Settings  Settings `json:"config"`
	Enabled   bool     `json:"enabled"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
}

// Store reads and writes provider configuration.
type Store struct {
	db repo.DBTX
}

// NewStore creates a provider Store.
func NewStore(db repo.DBTX) *Store { return &Store{db: db} }

const configColumns = "id, name, kind, COALESCE(auth_ref, ''), rate, price_micros, config, enabled, created_at, updated_at"

func scanConfig(scan func(...any) error) (*Config, error) {
	var c Config
	var settings string
	if err := scan(&c.ID, &c.Name, &c.Kind, &c.AuthRef, &c.Rate, &c.Price, &settings, &c.Enabled, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(settings), &c.Settings); err != nil {
		return nil, fmt.Errorf("provider %s: decode config: %w", c.Name, err)
	}
	return &c, nil
}

// List returns every provider ordered by name.
func (s *Store) List(ctx context.Context) ([]*Config, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+configColumns+" FROM providers ORDER BY name")
	if err != nil {
		return nil, fmt.Errorf("list providers: %w", err)
	}
	defer rows.Close()
	var out []*Config
	for rows.Next() {
		c, err := scanConfig(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan provider: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate providers: %w", err)
	}
	return out, nil
}

// Get returns the provider named name.
func (s *Store) Get(ctx context.Context, name string) (*Config, error) {
	c, err := scanConfig(s.db.QueryRowContext(ctx, "SELECT "+configColumns+" FROM providers WHERE name = ?", name).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, model.NewExitError(model.ErrNotFound, "provider %q not found (see crm provider list)", name)
	}
	if err != nil {
		return nil, fmt.Errorf("get provider %s: %w", name, err)
	}
	return c, nil
}

// Validate checks the fields every kind needs.
func (c *Config) Validate() error {
	if c.Name == "" {
		return model.NewExitError(model.ErrValidation, "provider name is required")
	}
	if !slices.Contains(Kinds(), c.Kind) {
		return model.NewExitError(model.ErrValidation, "provider %s: unknown kind %q (use one of %v)", c.Name, c.Kind, Kinds())
	}
	if c.Rate < 0 {
		return model.NewExitError(model.ErrValidation, "provider %s: rate must be >= 0", c.Name)
	}
	if c.Price < 0 {
		return model.NewExitError(model.ErrValidation, "provider %s: price must be >= 0", c.Name)
	}
	if _, err := c.Settings.TTL(); err != nil {
		return model.NewExitError(model.ErrValidation, "provider %s: cache ttl: %s", c.Name, err)
	}
	if c.Kind != "http" && (c.Settings.ErrorPath != "" || c.Settings.OKPath != "") {
		return model.NewExitError(model.ErrValidation, "provider %s: error_path and ok_path apply to http providers only", c.Name)
	}
	switch c.Kind {
	case "exa":
		if e := c.Settings.Endpoint; e != "" && e != "search" && e != "contents" {
			return model.NewExitError(model.ErrValidation, "provider %s: exa endpoint must be search or contents", c.Name)
		}
	case "github":
		if r := c.Settings.Resource; r != "" && r != "users" && r != "orgs" {
			return model.NewExitError(model.ErrValidation, "provider %s: github resource must be users or orgs", c.Name)
		}
	case "http":
		if c.Settings.URL == "" {
			return model.NewExitError(model.ErrValidation, "provider %s: http providers need a url template", c.Name)
		}
	case "llm":
		if c.Price != 0 {
			return model.NewExitError(model.ErrValidation, "provider %s: llm providers are priced per token (--price-in, --price-out), not per call", c.Name)
		}
		for _, p := range []*float64{c.Settings.PriceInPerMTok, c.Settings.PriceOutPerMTok} {
			if p != nil && (*p < 0 || math.IsNaN(*p) || math.IsInf(*p, 0)) {
				return model.NewExitError(model.ErrValidation, "provider %s: token prices must be >= 0", c.Name)
			}
		}
		if c.Settings.MaxTokens < 0 {
			return model.NewExitError(model.ErrValidation, "provider %s: max tokens must be >= 0", c.Name)
		}
	}
	return nil
}

// Save inserts or updates (by name) a provider configuration.
func (s *Store) Save(ctx context.Context, c *Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	settings, err := json.Marshal(c.Settings)
	if err != nil {
		return fmt.Errorf("encode provider config: %w", err)
	}
	var authRef *string
	if c.AuthRef != "" {
		authRef = &c.AuthRef
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO providers (name, kind, auth_ref, rate, price_micros, config, enabled)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET kind = excluded.kind, auth_ref = excluded.auth_ref,
		   rate = excluded.rate, price_micros = excluded.price_micros, config = excluded.config,
		   enabled = excluded.enabled, updated_at = datetime('now')`,
		c.Name, c.Kind, authRef, c.Rate, c.Price, string(settings), c.Enabled)
	if err != nil {
		return fmt.Errorf("save provider %s: %w", c.Name, err)
	}
	return nil
}
