// Package store abstracts SQL access so the same code runs on database/sql (native) and D1 (Worker).
// Multi-statement writes go through Batch: D1 cannot hold interactive transactions.
package store

import (
	"context"
	"errors"
)

var ErrNoRows = errors.New("store: no rows")

// Rows is the subset of *sql.Rows the code uses.
type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close() error
}

// Result reports what a statement changed.
type Result struct {
	RowsAffected int64
	LastInsertID int64
}

// Statement is one query with its arguments, for Batch.
type Statement struct {
	Query string
	Args  []any
}

// Querier runs single statements.
type Querier interface {
	Query(ctx context.Context, query string, args ...any) (Rows, error)
	Exec(ctx context.Context, query string, args ...any) (Result, error)
}

// Store is the database used everywhere: database/sql natively, D1 on the Worker.
type Store interface {
	Querier
	// Batch executes all statements atomically: all commit or none do.
	Batch(ctx context.Context, stmts []Statement) ([]Result, error)
	Close() error
}

// QueryRow scans exactly the first row, returning ErrNoRows when absent.
func QueryRow(ctx context.Context, q Querier, query string, args []any, dest ...any) error {
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return ErrNoRows
	}
	if err := rows.Scan(dest...); err != nil {
		return err
	}
	return rows.Close()
}
