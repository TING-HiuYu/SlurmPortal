//go:build !js

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/replication"
	"github.com/TING-HiuYu/SlurmPortal/internal/slave"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
	_ "modernc.org/sqlite"
)

// planReport is what `slave -plan` prints: every LDAP write a first sync would make, the guard
// verdict and whether the published SSH trust would differ from what sshd uses now.
type planReport struct {
	Snapshot struct {
		Users, Groups, Memberships, SSHCAs, Revocations int
	} `json:"snapshot"`
	Directory struct {
		Users, Groups int
	} `json:"directory"`
	Counts   map[string]int    `json:"counts"`
	LoginOps []slave.Op        `json:"loginOps"`
	OtherOps []slave.Op        `json:"otherOps"`
	Guard    string            `json:"guard"`
	Trust    map[string]string `json:"trust,omitempty"`
	Safe     bool              `json:"safe"`
}

// runPlan never writes LDAP or a trust directory: it reads a database copy, an LDIF export of the
// cluster directory (slapcat -n 1) and optionally a copy of the cluster's trust share.
func runPlan(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	dsn := fs.String("db", "", "SQLite DSN of the (migrated) master database")
	snapFile := fs.String("snapshot", "", "snapshot JSON instead of -db")
	sourceBase := fs.String("source-base-dn", "", "master directory base DN (default: the snapshot's)")
	localBase := fs.String("local-base-dn", "", "cluster LDAP base DN")
	localGroup := fs.String("local-group-dn", "", "cluster group container DN, when rewriting")
	adminDN := fs.String("admin-dn", "", "cluster LDAP bind DN (excluded from the diff)")
	ldifFile := fs.String("ldif", "", "LDIF export of the cluster directory")
	trustDir := fs.String("trust-dir", "", "copy of the cluster trust share (contains current/ssh/...)")
	maxDel := fs.Int("max-user-deletions", 5, "guard limit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *localBase == "" || *ldifFile == "" || (*dsn == "") == (*snapFile == "") {
		return errors.New("plan needs -local-base-dn, -ldif and exactly one of -db or -snapshot")
	}
	ctx := context.Background()
	next, err := loadSnapshot(ctx, *dsn, *snapFile, *sourceBase)
	if err != nil {
		return err
	}
	source := *sourceBase
	if source == "" {
		source = next.BaseDN
	}
	if !strings.EqualFold(source, *localBase) || *localGroup != "" {
		if next, err = slave.RewriteDNs(next, source, *localBase, *localGroup); err != nil {
			return err
		}
	} else {
		next.BaseDN = *localBase
	}
	f, err := os.Open(*ldifFile)
	if err != nil {
		return err
	}
	entries, err := slave.ParseLDIF(f)
	f.Close()
	if err != nil {
		return err
	}
	observed := slave.Baseline(slave.Observed(entries, *localBase, *adminDN), next)
	if *trustDir != "" {
		if observed.CAs, err = slave.TrustedCAs(*trustDir); err != nil {
			return err
		}
	}
	var rep planReport
	rep.Snapshot.Users, rep.Snapshot.Groups, rep.Snapshot.Memberships, rep.Snapshot.Revocations = len(next.Users), len(next.Groups), len(next.Memberships), len(next.SSHRevocations)
	for _, ca := range next.CAs {
		if ca.Kind == "ssh" && ca.Enabled {
			rep.Snapshot.SSHCAs++
		}
	}
	rep.Directory.Users, rep.Directory.Groups = len(observed.Users), len(observed.Groups)
	ops, err := slave.Plan(ctx, observed, next, *localBase, *adminDN)
	if err != nil {
		return err
	}
	rep.Counts = map[string]int{}
	for _, op := range ops {
		rep.Counts[op.Kind]++
		if op.Login {
			rep.LoginOps = append(rep.LoginOps, op)
		} else {
			rep.OtherOps = append(rep.OtherOps, op)
		}
	}
	rep.Guard = "pass"
	if err := slave.Guard(observed, next, slave.Limits{MaxUserDeletions: *maxDel}); err != nil {
		rep.Guard = err.Error()
	}
	trustSame := true
	if *trustDir != "" {
		if rep.Trust, trustSame, err = compareTrust(*trustDir, next); err != nil {
			return err
		}
	}
	rep.Safe = rep.Guard == "pass" && len(rep.LoginOps) == 0 && trustSame
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}

func loadSnapshot(ctx context.Context, dsn, file, baseDN string) (replication.Snapshot, error) {
	var s replication.Snapshot
	if file != "" {
		raw, err := os.ReadFile(file)
		if err != nil {
			return s, err
		}
		return s, json.Unmarshal(raw, &s)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return s, err
	}
	defer db.Close()
	s, _, err = replication.Load(ctx, store.SQL{DB: db}, baseDN)
	return s, err
}

// compareTrust publishes next into a scratch share and compares the SSH files sshd reads.
func compareTrust(liveDir string, next replication.Snapshot) (map[string]string, bool, error) {
	tmp, err := os.MkdirTemp("", "trust-plan-")
	if err != nil {
		return nil, false, err
	}
	defer os.RemoveAll(tmp)
	if err := slave.PublishTrust(tmp, next, time.Now()); err != nil {
		return nil, false, fmt.Errorf("render trust: %w", err)
	}
	out, same := map[string]string{}, true
	read := func(root, name string) ([]byte, error) {
		return os.ReadFile(filepath.Join(root, "current", "ssh", name))
	}
	a, errA := read(liveDir, "trusted-user-ca-keys.pub")
	b, errB := read(tmp, "trusted-user-ca-keys.pub")
	switch {
	case errA != nil || errB != nil:
		out["trusted-user-ca-keys.pub"], same = fmt.Sprintf("unreadable (live: %v, planned: %v)", errA, errB), false
	case bytes.Equal(normalizeKeys(a), normalizeKeys(b)):
		out["trusted-user-ca-keys.pub"] = "identical"
	default:
		out["trusted-user-ca-keys.pub"], same = "DIFFERENT", false
	}
	// A KRL embeds its generation time, so compare the revoked serials ssh-keygen lists.
	la, errA := krlSerials(filepath.Join(liveDir, "current", "ssh", "revoked.krl"))
	lb, errB := krlSerials(filepath.Join(tmp, "current", "ssh", "revoked.krl"))
	switch {
	case errA != nil || errB != nil:
		out["revoked.krl"], same = fmt.Sprintf("not compared (live: %v, planned: %v)", errA, errB), false
	case la == lb:
		out["revoked.krl"] = "same revocations"
	default:
		out["revoked.krl"], same = "DIFFERENT revocations", false
	}
	return out, same, nil
}

// normalizeKeys keeps "type base64" per line so comments and ordering do not count as changes.
func normalizeKeys(b []byte) []byte {
	var keys []string
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && !strings.HasPrefix(f[0], "#") {
			keys = append(keys, f[0]+" "+f[1])
		}
	}
	slices.Sort(keys)
	return []byte(strings.Join(keys, "\n"))
}

func krlSerials(path string) (string, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	raw, err := exec.Command("ssh-keygen", "-Q", "-l", "-f", path).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("ssh-keygen -Q -l: %v: %s", err, bytes.TrimSpace(raw))
	}
	var lines []string
	for _, l := range strings.Split(string(raw), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			lines = append(lines, l)
		}
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n"), nil
}
