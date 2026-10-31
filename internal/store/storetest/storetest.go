// Package storetest holds conformance checks every store.Store implementation must pass.
package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/store"
)

// Run checks that s behaves like the reference implementation.
func Run(t *testing.T, s store.Store) {
	t.Helper()
	ctx := context.Background()
	mustExec := func(q string, args ...any) store.Result {
		t.Helper()
		r, err := s.Exec(ctx, q, args...)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return r
	}
	mustExec(`CREATE TABLE conf (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, n INTEGER, f REAL, b BLOB, at TEXT)`)

	r := mustExec("INSERT INTO conf(name, n, f, b, at) VALUES (?, ?, ?, ?, ?)", "a", int64(7), 1.5, []byte{0, 1, 255}, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	if r.RowsAffected != 1 || r.LastInsertID != 1 {
		t.Fatalf("insert result = %+v", r)
	}

	var (
		name string
		n    int64
		f    float64
		b    []byte
		at   string
		null *string
	)
	if err := store.QueryRow(ctx, s, "SELECT name, n, f, b, at, NULL FROM conf WHERE id = ?", []any{1}, &name, &n, &f, &b, &at, &null); err != nil {
		t.Fatal(err)
	}
	if name != "a" || n != 7 || f != 1.5 || string(b) != "\x00\x01\xff" || at != "2026-01-02T03:04:05Z" || null != nil {
		t.Fatalf("round trip: %q %d %v %v %q %v", name, n, f, b, at, null)
	}
	if err := store.QueryRow(ctx, s, "SELECT name FROM conf WHERE id = ?", []any{99}, &name); !errors.Is(err, store.ErrNoRows) {
		t.Fatalf("missing row err = %v", err)
	}

	if _, err := s.Batch(ctx, []store.Statement{
		{Query: "INSERT INTO conf(name) VALUES (?)", Args: []any{"b"}},
		{Query: "INSERT INTO conf(name) VALUES (?)", Args: []any{"a"}},
	}); err == nil {
		t.Fatal("batch with unique violation succeeded")
	}
	var count int
	_ = store.QueryRow(ctx, s, "SELECT COUNT(*) FROM conf", nil, &count)
	if count != 1 {
		t.Fatalf("failed batch was not rolled back: %d rows", count)
	}

	res, err := s.Batch(ctx, []store.Statement{
		{Query: "INSERT INTO conf(name) VALUES (?)", Args: []any{"b"}},
		{Query: "UPDATE conf SET n = n + 1 WHERE name = ?", Args: []any{"a"}},
	})
	if err != nil || len(res) != 2 || res[1].RowsAffected != 1 {
		t.Fatalf("batch res=%+v err=%v", res, err)
	}

	rows, err := s.Query(ctx, "SELECT name FROM conf ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		names = append(names, v)
	}
	rows.Close()
	if len(names) != 2 || names[0] != "a" || names[1] != "b" {
		t.Fatalf("names = %v", names)
	}
	if _, err := s.Exec(ctx, "SELECT * FROM missing_table"); err == nil {
		t.Fatal("error not propagated")
	}
}
