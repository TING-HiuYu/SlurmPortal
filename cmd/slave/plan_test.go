package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/replication"
	"github.com/TING-HiuYu/SlurmPortal/internal/slave"
	"golang.org/x/crypto/ssh"
)

const base = "dc=slurm,dc=lan"

func TestPlanCommand(t *testing.T) {
	dir := t.TempDir()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	key, _ := ssh.NewPublicKey(pub)
	snap := replication.Snapshot{Version: replication.Version, BaseDN: base,
		Users:          []replication.User{{ID: 1, Username: "alice", DN: "uid=alice," + base, UIDNumber: 10001, GIDNumber: 10001, HomeDirectory: "/HOME/alice"}},
		Groups:         []replication.Group{{ID: 2, Name: "lab", Type: "cn", Class: "posixGroup", GIDNumber: 10001, DN: "cn=lab," + base}},
		Memberships:    []replication.Membership{{GroupID: 2, UserID: 1}},
		CAs:            []replication.CA{{ID: 5, Kind: "ssh", Enabled: true, CertificatePEM: string(ssh.MarshalAuthorizedKey(key))}},
		SSHRevocations: []replication.Revocation{{CAID: 5, SerialHex: "0a"}},
	}
	raw, _ := json.Marshal(snap)
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	snapFile := write("snap.json", string(raw))
	ldif := write("dir.ldif", strings.Join([]string{
		"dn: " + base, "objectClass: dcObject", "",
		"dn: uid=alice," + base, "objectClass: inetOrgPerson", "objectClass: posixAccount", "cn: alice", "sn: alice", "uid: alice",
		"uidNumber: 10001", "gidNumber: 10001", "homeDirectory: /HOME/alice", "",
		"dn: cn=lab," + base, "objectClass: posixGroup", "cn: lab", "gidNumber: 10001", "description: lab", "memberUid: alice", "",
	}, "\n"))
	trust := filepath.Join(dir, "trust")
	if err := slave.PublishTrust(trust, snap, time.Now()); err != nil {
		t.Fatal(err)
	}
	plan := func() planReport {
		var out bytes.Buffer
		if err := runPlan([]string{"-snapshot", snapFile, "-local-base-dn", base, "-ldif", ldif, "-trust-dir", trust}, &out); err != nil {
			t.Fatal(err)
		}
		var rep planReport
		if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
			t.Fatal(err)
		}
		return rep
	}
	if rep := plan(); !rep.Safe || len(rep.LoginOps)+len(rep.OtherOps) != 0 || rep.Trust["trusted-user-ca-keys.pub"] != "identical" || rep.Trust["revoked.krl"] != "same revocations" {
		t.Fatalf("identical inputs not safe: %+v", rep)
	}
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	otherKey, _ := ssh.NewPublicKey(other)
	live := filepath.Join(trust, "current", "ssh", "trusted-user-ca-keys.pub")
	if err := os.WriteFile(live, ssh.MarshalAuthorizedKey(otherKey), 0o644); err != nil {
		t.Fatal(err)
	}
	if rep := plan(); rep.Safe || rep.Trust["trusted-user-ca-keys.pub"] != "DIFFERENT" || !strings.Contains(rep.Guard, "no longer be trusted") {
		t.Fatalf("changed CA not flagged: %+v", rep)
	}
}
