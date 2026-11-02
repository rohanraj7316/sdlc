// Package storetest gives each test its own empty PostgreSQL schema, so tests (and test
// packages running in parallel) never see each other's tickets.
package storetest

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// EnvVar names the server tests run against, e.g.
// postgres://postgres:aisdlc@127.0.0.1:55432/aisdlc?sslmode=disable (see README).
const EnvVar = "AISDLC_TEST_DATABASE_URL"

// DSN creates a fresh schema and returns a connection string whose search_path points at it.
// The schema is dropped when the test ends. Open as many stores on it as the test needs.
func DSN(t testing.TB) string {
	t.Helper()
	base := os.Getenv(EnvVar)
	if base == "" {
		t.Fatalf("%s is not set: these tests need PostgreSQL (see README, \"Build and test\")", EnvVar)
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("%s: %v", EnvVar, err)
	}
	b := make([]byte, 8)
	rand.Read(b)
	schema := "t_" + hex.EncodeToString(b)

	db, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE SCHEMA " + schema); err != nil {
		db.Close()
		t.Fatalf("create test schema: %v", err)
	}
	t.Cleanup(func() {
		db.Exec("DROP SCHEMA " + schema + " CASCADE")
		db.Close()
	})
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}
