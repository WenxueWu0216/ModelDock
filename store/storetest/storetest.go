// Package storetest provisions a throwaway, fully-migrated Postgres database
// per test. Tests that need it are skipped unless MODELDOCK_TEST_DATABASE_URL
// points at a Postgres server where the user may CREATE DATABASE.
package storetest

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/wenxuewu0216/modeldock/store"
)

// EnvVar names the admin connection URL used to create test databases.
const EnvVar = "MODELDOCK_TEST_DATABASE_URL"

// URL creates a fresh database, migrates it and returns its URL. The database
// is dropped when the test finishes.
func URL(t testing.TB) string {
	t.Helper()
	admin := os.Getenv(EnvVar)
	if admin == "" {
		if os.Getenv("CI") != "" {
			t.Fatalf("%s must be set in CI", EnvVar)
		}
		t.Skipf("%s not set; skipping Postgres-backed test", EnvVar)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("storetest: connect admin: %v", err)
	}
	defer conn.Close(ctx)
	name := "md_test_" + strings.ReplaceAll(uuid.NewString()[:13], "-", "")
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("storetest: create database: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c, err := pgx.Connect(ctx, admin)
		if err != nil {
			return
		}
		defer c.Close(ctx)
		_, _ = c.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("storetest: parse %s: %v", EnvVar, err)
	}
	u.Path = "/" + name
	dbURL := u.String()
	if err := store.Migrate(dbURL); err != nil {
		t.Fatalf("storetest: migrate: %v", err)
	}
	return dbURL
}

// New returns an open Store on a fresh database.
func New(t testing.TB) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), URL(t))
	if err != nil {
		t.Fatalf("storetest: open: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// MustExec runs sql against the store's database (test fixtures only).
func MustExec(t testing.TB, dbURL, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatalf("storetest: connect: %v", err)
	}
	defer c.Close(ctx)
	if _, err := c.Exec(ctx, sql, args...); err != nil {
		t.Fatal(fmt.Errorf("storetest: exec: %w", err))
	}
}
