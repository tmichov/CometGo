package main

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tmichov/cometgo"
)

// TestSchemaUpToDate fails when web/src/schema.ts no longer matches the
// server; regenerate it with `make gen`.
func TestSchemaUpToDate(t *testing.T) {
	dsn := os.Getenv("COMETGO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("COMETGO_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	app, err := newApp(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	s, err := cometgo.Describe(ctx, app, cometgo.DescribeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cur, err := os.ReadFile(schemaFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cur, s.TypeScript()) {
		t.Fatalf("%s is out of date; run make gen", schemaFile)
	}
}
