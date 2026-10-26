package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// github looks up a login through the GitHub REST API (/users/{login}, which
// also answers organizations, or /orgs/{login}). The answer is the profile
// object; an unknown login is "no data". The token is optional.
type github struct {
	client   *http.Client
	base     string
	resource string
	price    Cost
	token    string
}

func newGitHub(client *http.Client, cfg *Config, base, token string) *github {
	g := &github{client: client, base: base, resource: cfg.Settings.Resource, price: cfg.Price, token: token}
	if g.resource == "" {
		g.resource = "users"
	}
	return g
}

// login normalizes "@octocat", "github.com/octocat" or a profile URL to "octocat".
func (g *github) login(in Input) string {
	v := strings.TrimSpace(in.Value)
	for _, p := range []string{"https://", "http://", "www.", "github.com/"} {
		v = strings.TrimPrefix(v, p)
	}
	v = strings.TrimPrefix(v, "@")
	if i := strings.IndexByte(v, '/'); i >= 0 {
		v = v[:i]
	}
	return strings.ToLower(v)
}

func (g *github) Estimate(Input) Cost { return g.price }

func (g *github) Key(in Input) Hash { return hashKey("github", g.base, g.resource, g.login(in)) }

func (g *github) Enrich(ctx context.Context, in Input) (Output, Cost, error) {
	login := g.login(in)
	if login == "" {
		return nullOutput, 0, nil
	}
	req, err := http.NewRequest(http.MethodGet, g.base+"/"+g.resource+"/"+url.PathEscape(login), http.NoBody)
	if err != nil {
		return nil, 0, fmt.Errorf("build github request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "crm-cli")
	if g.token != "" {
		req.Header.Set("Authorization", "Bearer "+g.token)
	}
	resp, found, err := call(ctx, g.client, req)
	if err != nil {
		return nil, 0, err
	}
	if !found {
		return nullOutput, g.price, nil
	}
	return Output(resp), g.price, nil
}
