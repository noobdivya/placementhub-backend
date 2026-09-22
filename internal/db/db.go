// Package db owns the Postgres pool, migrations and small query helpers.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	dbfiles "placementhub/db"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// DBTX is satisfied by *pgxpool.Pool and pgx.Tx, so services can run the
// same query inside or outside a transaction.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Postgres error codes we react to.
const (
	CodeUniqueViolation     = "23505"
	CodeForeignKeyViolation = "23503"
	CodeCheckViolation      = "23514"
	CodeStudentPlaced       = "PH001" // raised by forbid_placed_student()
)

func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	// Through a transaction-mode pooler (PgBouncer — Neon's pooled endpoint is
	// one), a connection can be handed to a different session between
	// statements, so pgx's default named, cached prepared statements can
	// collide with another session's ("prepared statement ... already in
	// use", SQLSTATE 08P01). QueryExecModeExec still uses the fast extended
	// protocol but with an unnamed statement each time, which is safe with
	// or without a pooler in front.
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

// Migrate applies all pending migrations.
func Migrate(ctx context.Context, url string) error {
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return err
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeExec // see Connect: same pooler-safety reason
	sqlDB := stdlib.OpenDB(*cfg)
	defer sqlDB.Close()
	return migrateDB(ctx, sqlDB)
}

func migrateDB(ctx context.Context, sqlDB *sql.DB) error {
	goose.SetBaseFS(dbfiles.Migrations)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	return goose.UpContext(ctx, sqlDB, "migrations")
}

// InTx runs fn in a transaction, committing on nil and rolling back otherwise.
func InTx(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// PgCode returns the SQLSTATE of a Postgres error, or "".
func PgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// PgConstraint returns the violated constraint / index name, or "".
func PgConstraint(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.ConstraintName
	}
	return ""
}

func IsNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
