package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/jdanielnd/crm-cli/internal/format"
	"github.com/jdanielnd/crm-cli/internal/model"
	"github.com/jdanielnd/crm-cli/internal/provider"
	"github.com/spf13/cobra"
)

// newRegistry builds the provider registry used by commands: real HTTP with
// a per-request timeout, credentials and URL overrides from the environment.
func newRegistry() *provider.Registry {
	return provider.NewRegistry(&http.Client{Timeout: 30 * time.Second}, os.Getenv)
}

func registerProviderCommands(rootCmd *cobra.Command) {
	cmd := &cobra.Command{
		Use:   "provider",
		Short: "Enrichment providers: prices, credentials, endpoints",
		Long: `Enrichment providers used by enrich columns.

Built in: exa (search), exa-contents, jina (reader), github (users/orgs). Add
any JSON API as a generic http provider with a URL template and a JSON path.
Credentials are read from the environment variable named by --auth-env and
never stored. CRM_PROVIDER_<NAME>_URL overrides a provider's base URL (or the
URL template of an http provider).`,
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

func providerData(c *provider.Config) map[string]any {
	cfg, _ := json.Marshal(c.Settings)
	return map[string]any{
		"id":          c.ID,
		"name":        c.Name,
		"kind":        c.Kind,
		"price":       c.Price.String(),
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
	var kind, price, authEnv, baseURL, endpoint, resource, url, method, body, path, authHeader string
	var rate float64
	var numResults int
	var enabled bool
	cmd := &cobra.Command{
		Use:   "set <name>",
		Short: "Create or update a provider",
		Long: `Create a provider (--kind is required) or update the flags given.

Generic http providers: --url is a template where {{input}} becomes the
query-escaped input; --body (JSON, sent with POST) gets the JSON-escaped input;
--path is the dotted path of the answer inside the response ("data.0.email").`,
		Example: `  crm provider set exa --price 0.005
  crm provider set github --auth-env GITHUB_TOKEN --rate 1
  crm provider set clearbit --kind http --url "https://api.example.com/v1/company?domain={{input}}" \
    --path data --auth-env EXAMPLE_KEY --price 0.01`,
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
	f.StringVar(&kind, "kind", "", "adapter: exa, jina, github or http")
	f.StringVar(&price, "price", "", "USD per call (e.g. 0.005)")
	f.Float64Var(&rate, "rate", 0, "max requests per second (0 = unlimited)")
	f.StringVar(&authEnv, "auth-env", "", "environment variable holding the credential")
	f.BoolVar(&enabled, "enabled", true, "enable (--enabled=false disables) the provider")
	f.StringVar(&baseURL, "base-url", "", "API root override (exa, jina, github)")
	f.StringVar(&endpoint, "endpoint", "", "exa: search or contents")
	f.IntVar(&numResults, "num-results", 0, "exa search: number of results")
	f.StringVar(&resource, "resource", "", "github: users or orgs")
	f.StringVar(&url, "url", "", "http: URL template with {{input}}")
	f.StringVar(&method, "method", "", "http: method (default GET, POST with --body)")
	f.StringVar(&body, "body", "", "http: JSON body template with {{input}}")
	f.StringVar(&path, "path", "", "http: dotted path of the answer in the response")
	f.StringVar(&authHeader, "auth-header", "", "http: header for the credential (default Authorization: Bearer)")
	return cmd
}
