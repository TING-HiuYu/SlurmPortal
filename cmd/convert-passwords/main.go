//go:build !js

// convert-passwords replaces v1 reversible RSA passwords with one-way credentials.
// Dry-run by default; the private key file must be 0600 or stricter.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"sort"

	_ "modernc.org/sqlite"

	"github.com/TING-HiuYu/SlurmPortal/internal/domain/auth"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
	"github.com/TING-HiuYu/SlurmPortal/migrations"
)

func main() {
	dsn := flag.String("db", "", "SQLite DSN of the database to convert")
	keyFile := flag.String("legacy-private-key", "", "v1 RSA private key (PKCS#1 PEM)")
	apply := flag.Bool("apply", false, "write credentials (default: dry-run)")
	flag.Parse()
	if err := run(*dsn, *keyFile, *apply); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(dsn, keyFile string, apply bool) error {
	if dsn == "" || keyFile == "" {
		return fmt.Errorf("-db and -legacy-private-key are required")
	}
	info, err := os.Stat(keyFile)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s must be a regular file with mode 0600 or stricter", keyFile)
	}
	pemBytes, err := os.ReadFile(keyFile)
	if err != nil {
		return err
	}
	dec, err := auth.ParseLegacyKey(pemBytes)
	if err != nil {
		return err
	}
	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	sqldb.SetMaxOpenConns(1)
	db := store.SQL{DB: sqldb}
	defer db.Close()

	ctx := context.Background()
	if apply {
		if _, err := store.Migrate(ctx, db, migrations.FS); err != nil {
			return err
		}
	}
	rep, err := auth.ConvertLegacy(ctx, db, dec, apply)
	if err != nil {
		return err
	}
	mode := "dry-run"
	if apply {
		mode = "applied"
	}
	fmt.Printf("%s: converted=%d already=%d failed=%d\n", mode, len(rep.Converted), rep.Existing, len(rep.Failed))
	ids := make([]int64, 0, len(rep.Failed))
	for id := range rep.Failed {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		fmt.Printf("  user %d: %s\n", id, rep.Failed[id])
	}
	if len(rep.Failed) > 0 {
		os.Exit(2)
	}
	return nil
}
