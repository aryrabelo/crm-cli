package cli

import (
	"fmt"
	"net"
	"os"

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
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the MCP server (stdio, or streamable HTTP with --http)",
		Long: `Start the MCP server.

Without --http it speaks JSON-RPC on stdin/stdout (what desktop clients
spawn). With --http ADDR it serves the streamable HTTP transport at
http://ADDR` + crmmcp.HTTPPath + `; --token (or ` + mcpTokenEnv + `) then requires
"Authorization: Bearer <token>". A token is mandatory when ADDR is not a
loopback address.`,
		Example: `  crm mcp serve
  crm mcp serve --http 127.0.0.1:8765
  CRM_MCP_TOKEN=s3cret crm mcp serve --http :8765`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if addr == "" && cmd.Flags().Changed("token") {
				return model.NewExitError(model.ErrValidation, "--token needs --http (stdio has no auth)")
			}
			if token == "" {
				token = os.Getenv(mcpTokenEnv)
			}
			if addr != "" {
				if _, _, err := net.SplitHostPort(addr); err != nil {
					return model.NewExitError(model.ErrValidation, "--http %q: want HOST:PORT or :PORT", addr)
				}
				if token == "" && crmmcp.RequiresToken(addr) {
					return model.NewExitError(model.ErrValidation,
						"--http %s listens beyond this machine: set --token or %s (or bind 127.0.0.1)", addr, mcpTokenEnv)
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
				auth := "no auth"
				if token != "" {
					auth = "bearer token required"
				}
				fmt.Fprintf(os.Stderr, "crm mcp: serving streamable HTTP on http://%s%s (%s)\n", a, crmmcp.HTTPPath, auth)
			})
		},
	}
	cmd.Flags().StringVar(&addr, "http", "", "serve streamable HTTP on HOST:PORT instead of stdio")
	cmd.Flags().StringVar(&token, "token", "", "bearer token required on HTTP requests (default $"+mcpTokenEnv+")")
	return cmd
}
