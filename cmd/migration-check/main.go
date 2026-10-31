//go:build !js

// migration-check rehearses the v1→v2 cut-over on a SQLite copy of the production database: it
// applies v2 migrations, proves the master key opens every CA, lists when user SSH certificates
// expire and writes the snapshot a v2 slave would receive. It never contacts any server.
//
//	PKI_MASTER_KEY=… migration-check -db file:copy.sqlite -base-dn dc=slurm,dc=lan -snapshot-out snap.json
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/domain/pki"
	"github.com/TING-HiuYu/SlurmPortal/internal/legacyimport"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugins/webaccess"
	"github.com/TING-HiuYu/SlurmPortal/internal/replication"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
	"github.com/TING-HiuYu/SlurmPortal/migrations"
	"golang.org/x/crypto/ssh"
	_ "modernc.org/sqlite"
)

func main() {
	dsn := flag.String("db", "", "SQLite DSN of the database copy (it is migrated in place)")
	baseDN := flag.String("base-dn", "", "directory base DN of the snapshot")
	out := flag.String("snapshot-out", "", "write the snapshot JSON here (mode 0600)")
	flag.Parse()
	if err := run(*dsn, *baseDN, *out); err != nil {
		slog.Error("migration check failed", "err", err)
		os.Exit(1)
	}
}

func run(dsn, baseDN, out string) error {
	if dsn == "" || baseDN == "" {
		return fmt.Errorf("-db and -base-dn are required")
	}
	ctx := context.Background()
	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	defer sqldb.Close()
	sqldb.SetMaxOpenConns(1)
	db := store.SQL{DB: sqldb}
	applied, err := store.Migrate(ctx, db, migrations.FS)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	fmt.Printf("migrations applied: %v\n", applied)
	rep, err := legacyimport.Run(ctx, db, time.Now())
	if err != nil {
		return fmt.Errorf("import v1 data: %w", err)
	}
	fmt.Printf("v1 data imported: webAccess=%v notification=%v resourceSummaries=%d\n", rep.WebAccess, rep.Notification, rep.Summaries)
	if err := checkWebAccess(ctx, db); err != nil {
		return err
	}
	// pki.New refuses to start unless PKI_MASTER_KEY opens every stored CA private key.
	svc, err := pki.New(ctx, db, os.Getenv("PKI_MASTER_KEY"))
	if err != nil {
		return fmt.Errorf("open CAs: %w", err)
	}
	cas, err := svc.ListCAs(ctx)
	if err != nil {
		return err
	}
	for _, ca := range cas {
		fp := "-"
		if k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(ca.CertificatePEM)); err == nil {
			fp = ssh.FingerprintSHA256(k)
		}
		fmt.Printf("CA %d %s %q enabled=%v %s: private key opens\n", ca.ID, ca.Kind, ca.Name, ca.Enabled, fp)
	}
	snap, hash, err := replication.Load(ctx, db, baseDN)
	if err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	fmt.Printf("snapshot %s: %d users, %d groups, %d memberships, %d CAs, %d SSH revocations, %d user certificates\n",
		hash[:12], len(snap.Users), len(snap.Groups), len(snap.Memberships), len(snap.CAs), len(snap.SSHRevocations), len(snap.UserCertificates))
	printExpiries(ctx, db)
	if out != "" {
		raw, _ := json.Marshal(snap)
		if err := os.WriteFile(out, raw, 0o600); err != nil {
			return err
		}
		fmt.Println("snapshot written to", out)
	}
	return nil
}

// printExpiries lists active SSH user certificates by expiry, so a freeze can end before any lapses.
func printExpiries(ctx context.Context, db store.Store) {
	rows, err := db.Query(ctx, `SELECT u.username, c.certificate_pem FROM user_certificates c JOIN users u ON u.id = c.user_id
		WHERE c.deleted_at IS NULL AND u.deleted_at IS NULL`)
	if err != nil {
		fmt.Println("certificate expiries unavailable:", err)
		return
	}
	defer rows.Close()
	type exp struct {
		user string
		at   time.Time
	}
	var list []exp
	for rows.Next() {
		var user, text string
		if rows.Scan(&user, &text) != nil {
			continue
		}
		if k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(text)); err == nil {
			if c, ok := k.(*ssh.Certificate); ok {
				list = append(list, exp{user, time.Unix(int64(c.ValidBefore), 0).UTC()})
			}
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].at.Before(list[j].at) })
	for _, e := range list {
		fmt.Printf("ssh certificate %-20s expires %s (in %s)\n", e.user, e.at.Format(time.RFC3339), time.Until(e.at).Round(time.Hour))
	}
}

// checkWebAccess validates imported web access settings exactly as the v2 plugin will on load.
func checkWebAccess(ctx context.Context, db store.Store) error {
	var raw string
	if err := store.QueryRow(ctx, db, "SELECT config_json FROM v2_web_access_config WHERE id = 1", nil, &raw); err != nil {
		fmt.Println("web access settings: none")
		return nil
	}
	var s webaccess.Settings
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return fmt.Errorf("web access settings: %w", err)
	}
	if err := s.Validate(); err != nil {
		return fmt.Errorf("web access settings invalid after import: %w", err)
	}
	for _, c := range s.Clusters {
		fmt.Printf("web access cluster %s gateway %s %s roles %v\n", c.ID, c.GatewayID, c.GatewayURL, c.AllowedRoles)
	}
	return nil
}
