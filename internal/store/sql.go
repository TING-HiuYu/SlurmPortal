package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// SQL adapts *sql.DB; the driver is registered by the native entry point only.
type SQL struct{ DB *sql.DB }

// normalize stores time.Time as RFC3339Nano UTC text, the D1 encoding, so both backends hold identical bytes.
func normalize(args []any) []any {
	copied := false
	for i, a := range args {
		if t, ok := a.(time.Time); ok {
			if !copied {
				args, copied = append([]any(nil), args...), true
			}
			args[i] = t.UTC().Format(time.RFC3339Nano)
		}
	}
	return args
}

// Query runs a SELECT.
func (s SQL) Query(ctx context.Context, query string, args ...any) (Rows, error) {
	return s.DB.QueryContext(ctx, query, normalize(args)...)
}

// Exec runs a statement that returns no rows.
func (s SQL) Exec(ctx context.Context, query string, args ...any) (Result, error) {
	res, err := s.DB.ExecContext(ctx, query, normalize(args)...)
	if err != nil {
		return Result{}, err
	}
	return toResult(res), nil
}

// Batch runs the statements in one transaction.
func (s SQL) Batch(ctx context.Context, stmts []Statement) ([]Result, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	out := make([]Result, 0, len(stmts))
	for _, st := range stmts {
		res, err := tx.ExecContext(ctx, st.Query, normalize(st.Args)...)
		if err != nil {
			return nil, errors.Join(err, tx.Rollback())
		}
		out = append(out, toResult(res))
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// Close closes the database.
func (s SQL) Close() error { return s.DB.Close() }

func toResult(res sql.Result) Result {
	n, _ := res.RowsAffected()
	id, _ := res.LastInsertId()
	return Result{RowsAffected: n, LastInsertID: id}
}
