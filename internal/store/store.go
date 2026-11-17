// Package store is the PostgreSQL persistence layer. Every write path is
// append-only; the schema triggers reject UPDATE/DELETE/TRUNCATE so no scan or
// review can overwrite history even by accident.
package store

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"cargo.example/secstate/internal/domain"
)

//go:embed schema.sql
var schemaSQL string

// ErrDuplicate is returned when an append would violate a uniqueness rule.
var ErrDuplicate = errors.New("duplicate")

// Store wraps the pool. All queries go through it so we can keep the
// append-only + transaction discipline in one place.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects and applies the schema.
func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	cfg.MaxConns = 12
	cfg.MaxConnLifetime = time.Hour
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	s := &Store{pool: pool}
	if err := s.Migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Pool exposes the pool for read-only demo helpers.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Migrate applies the idempotent schema.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, schemaSQL); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// Reset drops all data for the demo/tests. It recreates the schema so the
// append-only triggers never see the truncation; this runs only in ephemeral
// dev/test environments.
func (s *Store) Reset(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, `DROP SCHEMA IF EXISTS public CASCADE; CREATE SCHEMA public;`); err != nil {
		return fmt.Errorf("reset: %w", err)
	}
	return s.Migrate(ctx)
}

func isUnique(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}

// ---- Contracts --------------------------------------------------------------

// Contract is a registered message contract.
type Contract struct {
	ID           int64                     `json:"id"`
	Source       string                    `json:"source"`
	MessageType  string                    `json:"message_type"`
	Version      int                       `json:"version"`
	Compat       string                    `json:"compat"`
	Definition   domain.ContractDefinition `json:"definition"`
	Status       string                    `json:"status"`
	RegisteredAt time.Time                 `json:"registered_at"`
}

// RegisterContract appends a new contract version. The latest version of a
// (source, message_type) pair is the one intake validates against.
func (s *Store) RegisterContract(ctx context.Context, c Contract) (Contract, error) {
	if err := domain.ValidateContractDefinition(c.Definition); err != nil {
		return Contract{}, err
	}
	if err := domain.ValidateContractDefinition(domain.ContractDefinition{
		Fields: c.Definition.Fields, Policy: c.Compat,
	}); err != nil {
		return Contract{}, fmt.Errorf("compat: %w", err)
	}
	if c.Version == 0 {
		err := s.pool.QueryRow(ctx, `SELECT COALESCE(MAX(version),0)+1 FROM contracts WHERE source=$1 AND message_type=$2`,
			c.Source, c.MessageType).Scan(&c.Version)
		if err != nil {
			return Contract{}, err
		}
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO contracts (source, message_type, version, compat, definition)
		VALUES ($1,$2,$3,$4,$5)
		RETURNING id, registered_at`,
		c.Source, c.MessageType, c.Version, c.Compat, c.Definition)
	if err := row.Scan(&c.ID, &c.RegisteredAt); err != nil {
		if isUnique(err) {
			return Contract{}, fmt.Errorf("%w: contract %s/%s v%d already registered",
				ErrDuplicate, c.Source, c.MessageType, c.Version)
		}
		return Contract{}, err
	}
	c.Status = "active"
	return c, nil
}

// LatestContract fetches the newest active contract for a source+type.
func (s *Store) LatestContract(ctx context.Context, source, messageType string) (Contract, error) {
	var c Contract
	err := s.pool.QueryRow(ctx, `
		SELECT id, source, message_type, version, compat, definition, status, registered_at
		FROM contracts WHERE source=$1 AND message_type=$2 AND status='active'
		ORDER BY version DESC LIMIT 1`, source, messageType).
		Scan(&c.ID, &c.Source, &c.MessageType, &c.Version, &c.Compat, &c.Definition, &c.Status, &c.RegisteredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Contract{}, ErrNotFound
	}
	return c, err
}

// ListContracts returns all registered contracts newest first.
func (s *Store) ListContracts(ctx context.Context) ([]Contract, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, source, message_type, version, compat, definition, status, registered_at
		FROM contracts ORDER BY source, message_type, version DESC`)
	if err != nil {
		return nil, err
	}
	return collectRows(rows, func(r pgx.Rows) (Contract, error) {
		var c Contract
		err := r.Scan(&c.ID, &c.Source, &c.MessageType, &c.Version, &c.Compat,
			&c.Definition, &c.Status, &c.RegisteredAt)
		return c, err
	})
}

// ErrNotFound signals a missing row.
var ErrNotFound = errors.New("not found")

func collectRows[T any](rows pgx.Rows, scan func(pgx.Rows) (T, error)) ([]T, error) {
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
