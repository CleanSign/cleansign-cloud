// Package database holds the Postgres connection, migrations, and repositories (port implementations).
package database

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/CleanSign/cleansign/server/internal/database/sqlc"
	"github.com/CleanSign/cleansign/server/internal/shared/errs"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Open creates the connection pool and verifies the connection.
func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

// retryEvery is how long OpenWithRetry waits between two attempts.
const retryEvery = 2 * time.Second

// OpenWithRetry is Open for a database that may still be starting (on Kubernetes the API and the database come up at the
// same time): if it cannot be reached it keeps trying for up to wait, calling onRetry with each failure. A URL that cannot
// be parsed is a mistake in the settings and fails at once.
func OpenWithRetry(ctx context.Context, url string, wait time.Duration, onRetry func(error)) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	deadline := time.Now().Add(wait)
	for {
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err == nil {
			if err = pool.Ping(ctx); err == nil {
				return pool, nil
			}
			pool.Close()
		}
		if ctx.Err() != nil || !time.Now().Add(retryEvery).Before(deadline) {
			return nil, fmt.Errorf("ping database: %w", err)
		}
		if onRetry != nil {
			onRetry(err)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("ping database: %w", ctx.Err())
		case <-time.After(retryEvery):
		}
	}
}

// Migrate applies all migrations that have not been applied yet.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return err
	}
	db := stdlib.OpenDBFromPool(pool)
	defer func() { _ = db.Close() }()

	// Several processes may start at once (API replicas, workers). A Postgres advisory lock makes them take turns,
	// so each migration is applied exactly once and the others wait for it to finish.
	// Check for the lock every second (the default is every 5 seconds) for up to two minutes.
	locker, err := lock.NewPostgresSessionLocker(lock.WithLockTimeout(1, 120))
	if err != nil {
		return fmt.Errorf("create migration locker: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, sub, goose.WithSessionLocker(locker))
	if err != nil {
		return fmt.Errorf("create migration provider: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

// notFound turns "no row" into the shared cross-layer error.
func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return errs.ErrNotFound
	}
	return err
}

// queries runs queries on the pool. Use WithTx when a transaction is needed.
func queries(pool *pgxpool.Pool) *sqlc.Queries { return sqlc.New(pool) }
