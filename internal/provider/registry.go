package provider

import (
	"net/http"
	"strings"

	"github.com/jdanielnd/crm-cli/internal/model"
)

// Registry builds adapters from provider configurations.
type Registry struct {
	client *http.Client
	getenv func(string) string
}

// NewRegistry creates a Registry. getenv resolves credentials (auth_ref) and
// base URL overrides; pass os.Getenv in production.
func NewRegistry(client *http.Client, getenv func(string) string) *Registry {
	return &Registry{client: client, getenv: getenv}
}

// URLOverrideEnv is the environment variable that overrides the base URL (or,
// for http providers, the URL template) of the provider named name.
func URLOverrideEnv(name string) string {
	return "CRM_PROVIDER_" + strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(name)) + "_URL"
}

// usable checks that cfg is valid and enabled.
func usable(cfg *Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if !cfg.Enabled {
		return model.NewExitError(model.ErrValidation, "provider %s is disabled (crm provider set %s --enabled)", cfg.Name, cfg.Name)
	}
	return nil
}

// Open returns the enrich adapter for cfg. Disabled and llm providers cannot
// be opened (llm providers serve ai columns: OpenLLM).
func (r *Registry) Open(cfg *Config) (Adapter, error) {
	if err := usable(cfg); err != nil {
		return nil, err
	}
	if cfg.Kind == "llm" {
		return nil, model.NewExitError(model.ErrValidation, "provider %s is an llm: use it in an ai column (crm column add --type ai)", cfg.Name)
	}
	override := r.getenv(URLOverrideEnv(cfg.Name))
	token := ""
	if cfg.AuthRef != "" {
		token = r.getenv(cfg.AuthRef)
	}
	base := func(def string) string {
		switch {
		case override != "":
			return strings.TrimRight(override, "/")
		case cfg.Settings.BaseURL != "":
			return strings.TrimRight(cfg.Settings.BaseURL, "/")
		default:
			return def
		}
	}
	switch cfg.Kind {
	case "exa":
		return newExa(r.client, cfg, base("https://api.exa.ai"), token), nil
	case "jina":
		return newJina(r.client, cfg, base("https://r.jina.ai"), token), nil
	case "github":
		return newGitHub(r.client, cfg, base("https://api.github.com"), token), nil
	default: // "http", guaranteed by Validate
		tmpl := cfg.Settings.URL
		if override != "" {
			tmpl = override
		}
		return newHTTP(r.client, cfg, tmpl, token), nil
	}
}

// OpenLLM returns the chat completions client of an llm provider. Unset
// settings fall back to the environment: base URL to CRM_LLM_BASE_URL (then
// the OpenAI API), model to CRM_LLM_MODEL, prices to CRM_LLM_PRICE_IN and
// CRM_LLM_PRICE_OUT (USD per 1M tokens). The model and both prices are
// required so a run can never spend unpriced tokens.
func (r *Registry) OpenLLM(cfg *Config) (*LLM, error) {
	if err := usable(cfg); err != nil {
		return nil, err
	}
	if cfg.Kind != "llm" {
		return nil, model.NewExitError(model.ErrValidation, "provider %s is %s, not an llm (ai columns need llm providers)", cfg.Name, cfg.Kind)
	}
	s := cfg.Settings
	l := &LLM{client: r.client, name: cfg.Name, model: s.Model, authRef: cfg.AuthRef, maxTokens: s.MaxTokens}
	switch override, env := r.getenv(URLOverrideEnv(cfg.Name)), r.getenv(EnvLLMBaseURL); {
	case override != "":
		l.base = override
	case s.BaseURL != "":
		l.base = s.BaseURL
	case env != "":
		l.base = env
	default:
		l.base = DefaultLLMBaseURL
	}
	l.base = strings.TrimRight(l.base, "/")
	if l.model == "" {
		l.model = r.getenv(EnvLLMModel)
	}
	if l.model == "" {
		return nil, model.NewExitError(model.ErrValidation, "provider %s has no model: crm provider set %s --model <name> (or %s)", cfg.Name, cfg.Name, EnvLLMModel)
	}
	if l.maxTokens == 0 {
		l.maxTokens = DefaultLLMMaxTokens
	}
	for _, p := range []struct {
		set  *float64
		env  string
		flag string
		dst  *Cost
	}{
		{s.PriceInPerMTok, EnvLLMPriceIn, "--price-in", &l.priceIn},
		{s.PriceOutPerMTok, EnvLLMPriceOut, "--price-out", &l.priceOut},
	} {
		switch v := r.getenv(p.env); {
		case p.set != nil:
			*p.dst = usdPerMTok(*p.set)
		case v != "":
			c, err := ParseUSD(v)
			if err != nil {
				return nil, model.NewExitError(model.ErrValidation, "%s: %s", p.env, err)
			}
			*p.dst = c
		default:
			return nil, model.NewExitError(model.ErrValidation,
				"provider %s has no token price: crm provider set %s %s USD (per 1M tokens; 0 for a local model or subscription) or set %s",
				cfg.Name, cfg.Name, p.flag, p.env)
		}
	}
	if cfg.AuthRef != "" {
		l.token = r.getenv(cfg.AuthRef)
	}
	return l, nil
}
