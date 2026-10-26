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

// Open returns the adapter for cfg. Disabled providers cannot be opened.
func (r *Registry) Open(cfg *Config) (Adapter, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, model.NewExitError(model.ErrValidation, "provider %s is disabled (crm provider set %s --enabled)", cfg.Name, cfg.Name)
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
