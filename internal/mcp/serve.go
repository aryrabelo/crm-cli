package mcp

import (
	"context"
	"database/sql"
	"io"
	"net"
	"net/http"

	"github.com/jdanielnd/crm-cli/internal/httpguard"
	"github.com/jdanielnd/crm-cli/internal/provider"
	"github.com/jdanielnd/crm-cli/internal/run"
	"github.com/jdanielnd/crm-cli/internal/service"
	"github.com/mark3labs/mcp-go/server"
)

// HTTPPath is the endpoint of the streamable HTTP transport.
const HTTPPath = "/mcp"

// Server is the crm MCP server: the upstream per-entity tools plus the
// batch-shaped data-plane tools, which call the same service as the CLI.
type Server struct {
	mcp    *server.MCPServer
	svc    *service.Service
	runs   *run.Tracker
	entity map[string]bool
}

type options struct {
	registry *provider.Registry
}

// Option configures New.
type Option func(*options)

// WithRegistry sets the provider registry used by enrich and ai columns
// (default provider.NewEnvRegistry).
func WithRegistry(r *provider.Registry) Option {
	return func(o *options) { o.registry = r }
}

// New creates the MCP server on db. Close it to stop background runs.
func New(db *sql.DB, version string, opts ...Option) *Server {
	o := options{}
	for _, opt := range opts {
		opt(&o)
	}
	if o.registry == nil {
		o.registry = provider.NewEnvRegistry()
	}
	s := &Server{
		mcp:    server.NewMCPServer("crm", version, server.WithToolCapabilities(false)),
		svc:    service.New(db, o.registry),
		runs:   run.NewTracker(),
		entity: map[string]bool{},
	}
	registerEntityTools(s.mcp, db)
	for name := range s.mcp.ListTools() {
		s.entity[name] = true
	}
	s.registerBatchTools()
	return s
}

// MCP returns the underlying mcp-go server (e.g. for an in-process client).
func (s *Server) MCP() *server.MCPServer { return s.mcp }

// IsEntityTool reports whether name is one of the upstream per-entity tools.
func (s *Server) IsEntityTool(name string) bool { return s.entity[name] }

// Close interrupts the runs started with wait=false (they stay resumable)
// and waits for them to record their state.
func (s *Server) Close() { s.runs.Close() }

// ServeStdio serves JSON-RPC on in/out until ctx is cancelled or in closes.
func (s *Server) ServeStdio(ctx context.Context, in io.Reader, out io.Writer) error {
	return server.NewStdioServer(s.mcp).Listen(ctx, in, out)
}

// Handler serves the streamable HTTP transport at HTTPPath behind
// httpguard: Host/Origin allowlists, a mandatory bearer token and a body cap.
func (s *Server) Handler(guard httpguard.Config) (http.Handler, error) {
	h, err := httpguard.Wrap(guard, server.NewStreamableHTTPServer(s.mcp, server.WithEndpointPath(HTTPPath)))
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle(HTTPPath, h)
	return mux, nil
}

// ServeHTTP listens on addr and serves Handler until ctx is cancelled, then
// shuts down gracefully. token is required. On a loopback address only
// loopback Host and Origin values are accepted (DNS rebinding). ready
// (optional) receives the bound address once listening.
func (s *Server) ServeHTTP(ctx context.Context, addr, token string, ready func(net.Addr), opts ...httpguard.ServeOption) error {
	return httpguard.Serve(ctx, addr, token, s.Handler, ready, opts...)
}
