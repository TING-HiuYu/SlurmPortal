package d1

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/TING-HiuYu/SlurmPortal/internal/audit"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
	"github.com/TING-HiuYu/SlurmPortal/internal/store/storetest"
	"github.com/TING-HiuYu/SlurmPortal/migrations"
)

// sqliteBridge emulates the JS bridge contract against SQLite so D1 semantics are testable natively.
type sqliteBridge struct {
	db    *sql.DB
	calls []string
}

func decodeArg(v any) any {
	if m, ok := v.(map[string]any); ok {
		b, _ := base64.StdEncoding.DecodeString(m["blob"].(string))
		return b
	}
	if f, ok := v.(float64); ok && f == float64(int64(f)) {
		return int64(f)
	}
	return v
}

func encodeCell(v any) any {
	if b, ok := v.([]byte); ok {
		return map[string]string{"blob": base64.StdEncoding.EncodeToString(b)}
	}
	return v
}

type execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func (b *sqliteBridge) run(ctx context.Context, x execer, op string, st wireStatement) (map[string]any, error) {
	args := make([]any, len(st.Args))
	for i, a := range st.Args {
		args[i] = decodeArg(a)
	}
	if op == "query" {
		rows, err := x.QueryContext(ctx, st.SQL, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		out := [][]any{}
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				return nil, err
			}
			for i := range vals {
				vals[i] = encodeCell(vals[i])
			}
			out = append(out, vals)
		}
		return map[string]any{"columns": cols, "rows": out}, rows.Err()
	}
	res, err := x.ExecContext(ctx, st.SQL, args...)
	if err != nil {
		return nil, err
	}
	n, _ := res.RowsAffected()
	id, _ := res.LastInsertId()
	return map[string]any{"rowsAffected": n, "lastInsertId": id}, nil
}

func (b *sqliteBridge) Call(ctx context.Context, op string, payload []byte) ([]byte, error) {
	b.calls = append(b.calls, op)
	var req struct {
		Statements []wireStatement `json:"statements"`
	}
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, err
	}
	fail := func(err error) ([]byte, error) { return json.Marshal(map[string]string{"error": err.Error()}) }
	var results []map[string]any
	if op == "batch" {
		tx, err := b.db.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		for _, st := range req.Statements {
			r, err := b.run(ctx, tx, "exec", st)
			if err != nil {
				_ = tx.Rollback()
				return fail(err)
			}
			results = append(results, r)
		}
		if err := tx.Commit(); err != nil {
			return fail(err)
		}
	} else {
		r, err := b.run(ctx, b.db, op, req.Statements[0])
		if err != nil {
			return fail(err)
		}
		results = append(results, r)
	}
	return json.Marshal(map[string]any{"results": results})
}

func newStore(t *testing.T) (*Store, *sqliteBridge) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	b := &sqliteBridge{db: db}
	return New(b), b
}

func TestConformance(t *testing.T) {
	s, b := newStore(t)
	storetest.Run(t, s)
	for _, op := range b.calls {
		if op != "query" && op != "exec" && op != "batch" {
			t.Fatalf("unexpected op %q", op)
		}
	}
}

func TestSQLConformance(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	storetest.Run(t, store.SQL{DB: db})
}

func TestNoInteractiveTransactions(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.DB.Begin(); err == nil {
		t.Fatal("transaction allowed on d1")
	}
}

func TestLimits(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	stmts := make([]store.Statement, MaxBatchStatements+1)
	for i := range stmts {
		stmts[i] = store.Statement{Query: "SELECT 1"}
	}
	if _, err := s.Batch(ctx, stmts); err == nil {
		t.Fatal("oversized batch accepted")
	}
	if _, err := s.Exec(ctx, "SELECT ?", strings.Repeat("x", MaxPayloadBytes)); err == nil {
		t.Fatal("oversized payload accepted")
	}
	if _, err := s.Exec(ctx, "SELECT ?", int64(1)<<60); err == nil {
		t.Fatal("unsafe integer accepted")
	}
}

func TestMigrationsAndAuditOnD1(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t)
	if _, err := store.Migrate(ctx, s, migrations.FS); err != nil {
		t.Fatal(err)
	}
	rec, _ := audit.NewRecorder(true, audit.SQLStore{DB: s})
	for range 3 {
		if err := rec.Record(ctx, audit.Event{Action: "x", Outcome: audit.OutcomeSuccess}); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := audit.Verify(ctx, audit.SQLStore{DB: s}); err != nil || n != 3 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}
