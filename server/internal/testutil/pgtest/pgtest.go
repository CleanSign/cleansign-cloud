// Package pgtest creates an empty temporary database for each test.
// The admin connection URL is TEST_DATABASE_URL (default: Postgres from the local docker compose); if unreachable, the test is skipped.
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/CleanSign/cleansign/server/internal/database"
)

const defaultAdminURL = "postgres://postgres:mysecretpassword@localhost:5433/postgres?sslmode=disable"

// New returns a pool for a temporary DB with migrations applied, and drops it when the test ends.
func New(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := NewUnmigrated(t)
	if err := database.Migrate(context.Background(), pool); err != nil {
		t.Fatalf("마이그레이션: %v", err)
	}
	return pool
}

// AdminURL is the connection URL of the Postgres the tests create their temporary databases on.
func AdminURL() string {
	if u := os.Getenv("TEST_DATABASE_URL"); u != "" {
		return u
	}
	return defaultAdminURL
}

// NewUnmigrated returns a pool for an empty temporary DB (no tables), and drops it when the test ends.
func NewUnmigrated(t *testing.T) *pgxpool.Pool {
	t.Helper()
	adminURL := os.Getenv("TEST_DATABASE_URL")
	if adminURL == "" {
		adminURL = defaultAdminURL
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Skipf("테스트용 Postgres에 연결할 수 없어 건너뜁니다 (TEST_DATABASE_URL 또는 `make db`): %v", err)
	}
	defer func() { _ = admin.Close(ctx) }()

	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	name := "cleansign_test_" + hex.EncodeToString(suffix[:])
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s`, name)); err != nil {
		t.Fatalf("임시 DB 생성: %v", err)
	}

	u, err := url.Parse(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name

	pool, err := database.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		a, err := pgx.Connect(c, adminURL)
		if err != nil {
			return
		}
		defer func() { _ = a.Close(c) }()
		_, _ = a.Exec(c, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, name))
	})

	return pool
}
