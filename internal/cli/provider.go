package cli

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"

	"github.com/jdanielnd/crm-cli/internal/format"
	"github.com/jdanielnd/crm-cli/internal/model"
	"github.com/jdanielnd/crm-cli/internal/provider"
	"github.com/jdanielnd/crm-cli/internal/service"
	"github.com/spf13/cobra"
)

// newService builds the data-plane service used by commands, with the
// production provider registry (credentials and URL overrides from the
// environment).
func newService(db *sql.DB) *service.Service {
	return service.New(db, provider.NewEnvRegistry())
}

func registerProviderCommands(rootCmd *cobra.Command) {
	cmd := &cobra.Command{
		Use:   "provider",
		Short: "Providers: prices, credentials, endpoints, llm models",
		Long: `Providers used by enrich columns (waterfalls) and ai columns (llm).

Built in: exa (search), exa-contents, jina (reader), github (users/orgs) and
llm, an OpenAI-compatible chat completions endpoint for ai columns. Add any
JSON API as a generic http provider with a URL template and a JSON path, or
more llm providers (other models/gateways) with --kind llm.
Credentials are read from the environment variable named by --auth-env and
never stored. CRM_PROVIDER_<NAME>_URL overrides a provider's base URL (or the
URL template of an http provider).

llm providers: base URL from --base-url, else CRM_LLM_BASE_URL, else
https://api.openai.com/v1; key from CRM_LLM_API_KEY (the built-in llm's
--auth-env); model from --model or CRM_LLM_MODEL; prices in USD per 1M
tokens from --price-in/--price-out or CRM_LLM_PRICE_IN/CRM_LLM_PRICE_OUT
(required; 0 for a local model or a subscription gateway).`,
	}
	cmd.AddCommand(providerListCmd(), providerSetCmd())
	rootCmd.AddCommand(cmd)
}

var providerColumns = []format.ColumnDef{
	{Header: "Provider", Field: "name"},
	{Header: "Kind", Field: "kind"},
	{Header: "Price/Call", Field: "price"},
	{Header: "Rate/s", Field: "rate"},
	{Header: "Auth Env", Field: "auth_ref"},
	{Header: "Auth Set", Field: "auth_set"},
	{Header: "Enabled", Field: "enabled"},
	{Header: "Config", Field: "config_text"},
}

// priceText is the price column: per call, or per 1M tokens for llm providers.
func priceText(c *provider.Config) string {
	if c.Kind != "llm" {
		return c.Price.String()
	}
	usd := func(p *float64, env string) string {
		switch {
		case p != nil:
			return provider.Cost(math.Round(*p * 1e6)).String()
		case os.Getenv(env) != "":
			return "$" + os.Getenv(env) + " (env)"
		default:
			return "unset"
		}
	}
	return "in " + usd(c.Settings.PriceInPerMTok, provider.EnvLLMPriceIn) +
		" / out " + usd(c.Settings.PriceOutPerMTok, provider.EnvLLMPriceOut) + " per 1M tok"
}

func providerData(c *provider.Config) map[string]any {
	cfg, _ := json.Marshal(c.Settings)
	return map[string]any{
		"id":          c.ID,
		"name":        c.Name,
		"kind":        c.Kind,
		"price":       priceText(c),
		"price_usd":   c.Price.USD(),
		"rate":        c.Rate,
		"auth_ref":    c.AuthRef,
		"auth_set":    c.AuthRef != "" && os.Getenv(c.AuthRef) != "",
		"enabled":     c.Enabled,
		"config":      c.Settings,
		"config_text": string(cfg),
	}
}

func providerListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List providers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			db, err := openDB()
			if err != nil {
				return err
			}
			defer db.Close()
			list, err := provider.NewStore(db).List(cmd.Context())
			if err != nil {
				return err
			}
			data := make([]map[string]any, len(list))
			for i, c := range list {
				data[i] = providerData(c)
			}
			return format.Output(os.Stdout, resolveFormat(), data, providerColumns, flagQuiet)
		},
	}
}

func providerSetCmd() *cobra.Command {
	var kind, price, authEnv, baseURL, endpoint, resource, url, method, body, path, authHeader, modelName, priceIn, priceOut string
	var rate float64
	var numResults, maxTokens int
	var enabled bool
	cmd := &cobra.Command{
		Use:   "set <name>",
		Short: "Create or update a provider",
		Long: `Create a provider (--kind is required) or update the flags given.

Generic http providers: --url is a template where {{input}} becomes the
query-escaped input; --body (JSON, sent with POST) gets the JSON-escaped input;
--path is the dotted path of the answer inside the response ("data.0.email").

llm providers (OpenAI-compatible chat completions): --model, --price-in and
--price-out (USD per 1M prompt/completion tokens), --max-tokens (per call,
default 512), --base-url (default $CRM_LLM_BASE_URL or the OpenAI API).`,
		Example: `  crm provider set exa --price 0.005
  crm provider set github --auth-env GITHUB_TOKEN --rate 1
  crm provider set clearbit --kind http --url "https://api.example.com/v1/company?domain={{input}}" \
    --path data --auth-env EXAMPLE_KEY --price 0.01
  crm provider set llm --model gpt-4o-mini --price-in 0.15 --price-out 0.60
  crm provider set local --kind llm --base-url http://127.0.0.1:4000/v1 --model qwen3 \
    --price-in 0 --price-out 0 --auth-env LOCAL_LLM_KEY`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := openDB()
			if err != nil {
				return err
			}
			defer db.Close()
			store := provider.NewStore(db)
			cfg, err := store.Get(cmd.Context(), args[0])
			switch {
			case errors.Is(err, model.ErrNotFound):
				if kind == "" {
					return model.NewExitError(model.ErrValidation, "provider %q does not exist; --kind is required to create it", args[0])
				}
				cfg = &provider.Config{Name: args[0], Enabled: true}
			case err != nil:
				return err
			}
			f := cmd.Flags()
			if f.Changed("kind") {
				cfg.Kind = kind
			}
			if f.Changed("price") {
				if cfg.Price, err = provider.ParseUSD(price); err != nil {
					return model.NewExitError(model.ErrValidation, "--price: %s", err)
				}
			}
			if f.Changed("rate") {
				cfg.Rate = rate
			}
			if f.Changed("auth-env") {
				cfg.AuthRef = authEnv
			}
			if f.Changed("enabled") {
				cfg.Enabled = enabled
			}
			s := &cfg.Settings
			for flag, pair := range map[string][2]*string{
				"base-url": {&s.BaseURL, &baseURL}, "endpoint": {&s.Endpoint, &endpoint},
				"resource": {&s.Resource, &resource}, "url": {&s.URL, &url},
				"method": {&s.Method, &method}, "body": {&s.Body, &body},
				"path": {&s.Path, &path}, "auth-header": {&s.AuthHeader, &authHeader},
			} {
				if f.Changed(flag) {
					*pair[0] = *pair[1]
				}
			}
			if f.Changed("num-results") {
				s.NumResults = numResults
			}
			if f.Changed("max-tokens") {
				s.MaxTokens = maxTokens
			}
			if f.Changed("model") {
				s.Model = modelName
			}
			for flag, pair := range map[string]struct {
				dst **float64
				src string
			}{"price-in": {&s.PriceInPerMTok, priceIn}, "price-out": {&s.PriceOutPerMTok, priceOut}} {
				if !f.Changed(flag) {
					continue
				}
				c, err := provider.ParseUSD(pair.src)
				if err != nil {
					return model.NewExitError(model.ErrValidation, "--%s: %s", flag, err)
				}
				usd := c.USD()
				*pair.dst = &usd
			}
			if err := store.Save(cmd.Context(), cfg); err != nil {
				return err
			}
			saved, err := store.Get(cmd.Context(), cfg.Name)
			if err != nil {
				return err
			}
			if !flagQuiet {
				fmt.Fprintf(os.Stderr, "Saved provider %s\n", saved.Name)
			}
			return format.Output(os.Stdout, resolveFormat(), []map[string]any{providerData(saved)}, providerColumns, flagQuiet)
		},
	}
	f := cmd.Flags()
	f.StringVar(&kind, "kind", "", "adapter: exa, jina, github, http or llm")
	f.StringVar(&price, "price", "", "USD per call (e.g. 0.005)")
	f.Float64Var(&rate, "rate", 0, "max requests per second (0 = unlimited)")
	f.StringVar(&authEnv, "auth-env", "", "environment variable holding the credential")
	f.BoolVar(&enabled, "enabled", true, "enable (--enabled=false disables) the provider")
	f.StringVar(&baseURL, "base-url", "", "API root override (exa, jina, github, llm)")
	f.StringVar(&endpoint, "endpoint", "", "exa: search or contents")
	f.IntVar(&numResults, "num-results", 0, "exa search: number of results")
	f.StringVar(&resource, "resource", "", "github: users or orgs")
	f.StringVar(&url, "url", "", "http: URL template with {{input}}")
	f.StringVar(&method, "method", "", "http: method (default GET, POST with --body)")
	f.StringVar(&body, "body", "", "http: JSON body template with {{input}}")
	f.StringVar(&path, "path", "", "http: dotted path of the answer in the response")
	f.StringVar(&authHeader, "auth-header", "", "http: header for the credential (default Authorization: Bearer)")
	f.StringVar(&modelName, "model", "", "llm: model name (default $CRM_LLM_MODEL)")
	f.StringVar(&priceIn, "price-in", "", "llm: USD per 1M prompt tokens (0 for local/subscription)")
	f.StringVar(&priceOut, "price-out", "", "llm: USD per 1M completion tokens (0 for local/subscription)")
	f.IntVar(&maxTokens, "max-tokens", 0, "llm: max completion tokens per call (default 512)")
	return cmd
}
