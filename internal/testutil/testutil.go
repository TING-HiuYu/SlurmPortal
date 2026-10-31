//go:build !js

// Package testutil provides migrated in-memory databases and fixtures for tests.
package testutil

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/TING-HiuYu/SlurmPortal/internal/store"
	"github.com/TING-HiuYu/SlurmPortal/migrations"
)

var seq atomic.Int64

// DB returns a fresh, fully migrated in-memory SQLite store.
func DB(t testing.TB) store.SQL {
	t.Helper()
	name := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	db, err := sql.Open("sqlite", "file:"+name+"_"+strconv.FormatInt(seq.Add(1), 10)+"?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	s := store.SQL{DB: db}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := store.Migrate(context.Background(), s, migrations.FS); err != nil {
		t.Fatal(err)
	}
	return s
}

// Role inserts a role and returns its id.
func Role(t testing.TB, db store.Store, keyword string, sort int) int64 {
	t.Helper()
	ctx := context.Background()
	if _, err := db.Exec(ctx, "INSERT INTO roles(name, keyword, status, sort) VALUES (?, ?, 1, ?)", keyword, keyword, sort); err != nil {
		t.Fatal(err)
	}
	return RoleID(t, db, keyword)
}

func RoleID(t testing.TB, db store.Store, keyword string) int64 {
	t.Helper()
	var id int64
	if err := store.QueryRow(context.Background(), db, "SELECT id FROM roles WHERE keyword = ?", []any{keyword}, &id); err != nil {
		t.Fatal(err)
	}
	return id
}

// User inserts a v1-shaped user row with the given roles and returns its id.
func User(t testing.TB, db store.Store, username string, status int, roles ...string) int64 {
	t.Helper()
	ctx := context.Background()
	if _, err := db.Exec(ctx, `INSERT INTO users(username, password, mobile, status, department_id, source_user_id, source_union_id, user_dn)
		VALUES (?, '', '', ?, '', '', '', ?)`, username, status, "uid="+username+",ou=people,dc=test"); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := store.QueryRow(ctx, db, "SELECT id FROM users WHERE username = ?", []any{username}, &id); err != nil {
		t.Fatal(err)
	}
	for _, r := range roles {
		if _, err := db.Exec(ctx, "INSERT INTO user_roles(user_id, role_id) VALUES (?, ?)", id, RoleID(t, db, r)); err != nil {
			t.Fatal(err)
		}
	}
	return id
}
