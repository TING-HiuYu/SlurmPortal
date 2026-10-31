package store

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
)

const migrationTable = `CREATE TABLE IF NOT EXISTS v2_schema_migrations (
	name TEXT PRIMARY KEY,
	applied_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
)`

// StatementSeparator splits a migration file; needed because trigger bodies contain semicolons.
const StatementSeparator = "\n--;\n"

// Migrate applies *.sql files from fsys in lexical order, each file atomically, never rewriting applied ones.
func Migrate(ctx context.Context, s Store, fsys fs.FS) ([]string, error) {
	if _, err := s.Exec(ctx, migrationTable); err != nil {
		return nil, err
	}
	applied := map[string]bool{}
	rows, err := s.Query(ctx, "SELECT name FROM v2_schema_migrations")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		applied[n] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return nil, err
	}
	slices.Sort(names)
	var done []string
	for _, name := range names {
		if applied[name] {
			continue
		}
		raw, err := fs.ReadFile(fsys, name)
		if err != nil {
			return done, err
		}
		var stmts []Statement
		for _, q := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), StatementSeparator) {
			if q = strings.TrimSpace(q); q != "" {
				stmts = append(stmts, Statement{Query: q})
			}
		}
		stmts = append(stmts, Statement{Query: "INSERT INTO v2_schema_migrations(name) VALUES (?)", Args: []any{name}})
		if _, err := s.Batch(ctx, stmts); err != nil {
			// Concurrent cold starts (several Worker isolates) race to apply the same migration; the
			// batch is atomic, so the loser rolled back and only has to see the winner's record.
			var n int
			if qerr := QueryRow(ctx, s, "SELECT COUNT(*) FROM v2_schema_migrations WHERE name = ?", []any{name}, &n); qerr == nil && n > 0 {
				continue
			}
			return done, fmt.Errorf("migration %s: %w", path.Base(name), err)
		}
		done = append(done, name)
	}
	return done, nil
}
