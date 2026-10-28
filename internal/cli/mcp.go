package cli

import (
	"fmt"
	"net"
	"os"

	"github.com/jdanielnd/crm-cli/internal/httpguard"
	crmmcp "github.com/jdanielnd/crm-cli/internal/mcp"
	"github.com/jdanielnd/crm-cli/internal/model"
	"github.com/spf13/cobra"
)

// mcpTokenEnv is the environment variable read when --token is not given.
const mcpTokenEnv = "CRM_MCP_TOKEN"

func registerMCPCommands(rootCmd *cobra.Command) {
	mcpCmd := &cobra.Command{
		Use:   "mcp",
		Short: "MCP server for AI agent integration",
	}
	mcpCmd.AddCommand(mcpServeCmd())
	rootCmd.AddCommand(mcpCmd)
}

func mcpServeCmd() *cobra.Command {
	var addr, token string
	var tlsOpt tlsFlags
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the MCP server (stdio, or streamable HTTP with --http)",
		Long: `Start the MCP server.

Without --http it speaks JSON-RPC on stdin/stdout (what desktop clients
spawn). With --http ADDR it serves the streamable HTTP transport at
http(s)://ADDR` + crmmcp.HTTPPath + ` (https when --tls-cert and
--tls-key are given), and every request needs
"Authorization: Bearer <token>":

  - --token (or ` + mcpTokenEnv + `) sets the token. Without one, a loopback
    address gets a random token generated at startup and printed on stderr
    (copy it into your MCP client); any other address refuses to start.
  - On a loopback address only Host and Origin values naming 127.0.0.1,
    localhost or [::1] with the bound port are accepted, so a web page cannot
    reach the server through DNS rebinding. Requests carrying a foreign
    Origin get 403 on every address.
  - Request bodies are capped at 16 MiB.
  - --tls-cert and --tls-key serve HTTPS. Plain HTTP sends the token in clear
    text, so a bind beyond this machine refuses to start without TLS unless
    --behind-tls-proxy says a TLS reverse proxy in front terminates HTTPS.`,
		Example: `  crm mcp serve
  crm mcp serve --http 127.0.0.1:8765          # prints a generated token on stderr
  CRM_MCP_TOKEN=s3cret crm mcp serve --http 127.0.0.1:8765
  export CRM_MCP_TOKEN=$(openssl rand -hex 32)   # exported, so the client can send it too
  crm mcp serve --http :8765 --tls-cert crm.pem --tls-key crm.key
  curl -H "Authorization: Bearer s3cret" -H 'Content-Type: application/json' \
    -H 'Accept: application/json, text/event-stream' http://127.0.0.1:8765/mcp \
    -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"curl","version":"1"}}}'`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if addr == "" && cmd.Flags().Changed("token") {
				return model.NewExitError(model.ErrValidation, "--token needs --http (stdio has no auth)")
			}
			generated := false
			var err error
			if addr != "" {
				if _, _, err := net.SplitHostPort(addr); err != nil {
					return model.NewExitError(model.ErrValidation, "--http %q: want HOST:PORT or :PORT", addr)
				}
				if token, generated, err = resolveHTTPToken(addr, "--http", token, mcpTokenEnv); err != nil {
					return err
				}
			}
			var serveOpts []httpguard.ServeOption
			scheme := "http"
			if addr == "" {
				if tlsOpt.cert != "" || tlsOpt.key != "" || tlsOpt.behindProxy {
					return model.NewExitError(model.ErrValidation, "--tls-cert, --tls-key and --behind-tls-proxy need --http")
				}
			} else {
				var isTLS bool
				if serveOpts, isTLS, err = tlsOpt.serveOptions(addr, "--http"); err != nil {
					return err
				}
				if isTLS {
					scheme = "https"
				}
			}
			db, err := openDB()
			if err != nil {
				return err
			}
			defer db.Close()
			s := crmmcp.New(db, Version)
			defer s.Close()
			if addr == "" {
				return s.ServeStdio(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout())
			}
			return s.ServeHTTP(cmd.Context(), addr, token, func(a net.Addr) {
				fmt.Fprintf(os.Stderr, "crm mcp: serving streamable HTTP on %s://%s%s (bearer token required)\n", scheme, a, crmmcp.HTTPPath)
				if generated {
					fmt.Fprintf(os.Stderr, "crm mcp: no --token or %s given; generated token for this session:\n    Authorization: Bearer %s\n", mcpTokenEnv, token)
				}
			}, serveOpts...)
		},
	}
	cmd.Flags().StringVar(&addr, "http", "", "serve streamable HTTP on HOST:PORT instead of stdio")
	cmd.Flags().StringVar(&token, "token", "", "bearer token required on HTTP requests (default $"+mcpTokenEnv+", else generated on loopback)")
	tlsOpt.register(cmd.Flags())
	return cmd
}
