// Package store is ModelDock's Postgres persistence layer: the model
// registry, deployment history, approvals, the append-only audit log and the
// transactional outbox.
//
// Every state change and the audit event describing it are written in one
// transaction (see Tx.AppendEvent), so the database can never hold a
// deployment record without its event or vice versa.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // migrate driver
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wenxuewu0216/modeldock/domain"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrate applies all pending up-migrations to the database at url.
func Migrate(url string) error {
	src, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("store: open embedded migrations: %w", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, toMigrateURL(url))
	if err != nil {
		return fmt.Errorf("store: init migrate: %w", err)
	}
	defer m.Close()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("store: migrate up: %w", err)
	}
	return nil
}

// toMigrateURL rewrites postgres:// URLs to the pgx5:// scheme the
// golang-migrate pgx/v5 driver registers.
func toMigrateURL(url string) string {
	for _, p := range []string{"postgres://", "postgresql://"} {
		if len(url) > len(p) && url[:len(p)] == p {
			return "pgx5://" + url[len(p):]
		}
	}
	return url
}

// querier is satisfied by both *pgxpool.Pool and pgx.Tx so read queries can
// run inside or outside a transaction.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// queries holds every query that is valid both inside and outside a
// transaction.
type queries struct{ db querier }

// Store is the entry point; it owns the connection pool.
type Store struct {
	queries
	pool       *pgxpool.Pool
	connConfig *pgx.ConnConfig
}

// Tx is an open transaction. It exposes the write operations.
type Tx struct {
	queries
	tx pgx.Tx
}

// Open connects to Postgres and verifies the connection.
func Open(ctx context.Context, url string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("store: parse database URL: %w", err)
	}
	// pgx defaults to max(4, NumCPU) connections, which GraphQL's parallel
	// field resolvers exhaust quickly. Respect an explicit pool_max_conns.
	if !strings.Contains(url, "pool_max_conns") {
		cfg.MaxConns = 20
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return &Store{queries: queries{db: pool}, pool: pool, connConfig: cfg.ConnConfig}, nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// PoolStats exposes connection pool statistics.
func (s *Store) PoolStats() *pgxpool.Stat { return s.pool.Stat() }

// Ping checks database connectivity (used by /readyz).
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// InTx runs fn in a transaction, committing if fn returns nil and rolling
// back otherwise.
func (s *Store) InTx(ctx context.Context, fn func(*Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	if err := fn(&Tx{queries: queries{db: tx}, tx: tx}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit: %w", mapErr(err))
	}
	return nil
}

// TryLeader attempts to take a session-level advisory lock, giving simple
// leader election among API replicas for singleton loops (outbox relay,
// reconciler). If ok, the caller must call release when it stops leading.
//
// The lock lives on a dedicated connection outside the pool: a leader holds
// it for the life of the process, and borrowing a pooled connection for that
// would silently shrink the pool available to requests.
func (s *Store) TryLeader(ctx context.Context, key int64) (release func(), ok bool, err error) {
	conn, err := pgx.ConnectConfig(ctx, s.connConfig.Copy())
	if err != nil {
		return nil, false, fmt.Errorf("store: connect for leader lock: %w", err)
	}
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&ok); err != nil {
		conn.Close(context.Background())
		return nil, false, fmt.Errorf("store: try advisory lock %d: %w", key, err)
	}
	if !ok {
		conn.Close(context.Background())
		return nil, false, nil
	}
	// Closing the session releases the lock; use a fresh context so release
	// works during shutdown.
	return func() { conn.Close(context.Background()) }, true, nil
}

// mapErr translates Postgres errors into domain sentinels.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505": // unique_violation
			return fmt.Errorf("%w: %s", domain.ErrConflict, pgErr.Detail)
		case "23514": // check_violation
			if pgErr.ConstraintName == "approvals_no_self_approval" {
				return fmt.Errorf("%w: self-approval rejected by database constraint", domain.ErrForbidden)
			}
			return fmt.Errorf("%w: %s", domain.ErrInvalid, pgErr.Message)
		case "23503": // foreign_key_violation
			return fmt.Errorf("%w: %s", domain.ErrInvalid, pgErr.Detail)
		}
	}
	return err
}

func wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("store: %s: %w", op, mapErr(err))
}
