package db

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/lib/pq"
)

// Open opens a PostgreSQL connection and verifies it.
func Open(dsn string) (*sql.DB, error) {
	conn, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	if err := conn.Ping(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return conn, nil
}

// ApplySchema idempotently applies all story-API DDL.
func ApplySchema(ctx context.Context, conn *sql.DB) error {
	for _, stmt := range ddl {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("apply schema: %w", err)
		}
	}
	return nil
}
