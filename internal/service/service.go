// Package service holds the data-plane operations shared by the CLI and the
// MCP server (import, upsert, query, export, columns, runs, qualify, push).
// A command and its MCP tool call the same method, so they cannot drift.
package service

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jdanielnd/crm-cli/internal/provider"
	"github.com/jdanielnd/crm-cli/internal/run"
)

// Service runs data-plane operations on one database.
type Service struct {
	db       *sql.DB
	registry *provider.Registry
}

// New creates a Service. registry opens the providers of enrich and ai
// columns (provider.NewEnvRegistry in production).
func New(db *sql.DB, registry *provider.Registry) *Service {
	return &Service{db: db, registry: registry}
}

// Engine returns the batch run engine.
func (s *Service) Engine() *run.Engine { return run.NewEngine(s.db, s.registry) }

func (s *Service) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
