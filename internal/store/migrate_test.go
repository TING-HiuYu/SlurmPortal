package store_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"testing/fstest"

	_ "modernc.org/sqlite"

	"github.com/TING-HiuYu/SlurmPortal/internal/store"
)

// racer applies the migration as "another instance" right before our batch, so our batch loses.
type racer struct {
	store.SQL
	raced bool
}

func (r *racer) Batch(ctx context.Context, stmts []store.Statement) ([]store.Result, error) {
	if !r.raced {
		r.raced = true
		if _, err := r.SQL.Batch(ctx, stmts); err != nil {
			return nil, err
		}
		return nil, errors.New("UNIQUE constraint failed: v2_schema_migrations.name")
	}
	return r.SQL.Batch(ctx, stmts)
}

func TestMigrateToleratesConcurrentApply(t *testing.T) {
	db, err := sql.Open("sqlite", "file:race?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	fsys := fstest.MapFS{
		"0001_a.sql": {Data: []byte("CREATE TABLE IF NOT EXISTS a (id INTEGER)")},
		"0002_b.sql": {Data: []byte("CREATE TABLE IF NOT EXISTS b (id INTEGER)")},
	}
	s := &racer{SQL: store.SQL{DB: db}}
	done, err := store.Migrate(context.Background(), s, fsys)
	if err != nil || len(done) != 1 || done[0] != "0002_b.sql" {
		t.Fatalf("lost race not tolerated: %v %v", done, err)
	}
	// A failure that is not a lost race still surfaces.
	bad := fstest.MapFS{"0003_c.sql": {Data: []byte("NOT SQL")}}
	if _, err := store.Migrate(context.Background(), store.SQL{DB: db}, bad); err == nil {
		t.Fatal("broken migration accepted")
	}
}
