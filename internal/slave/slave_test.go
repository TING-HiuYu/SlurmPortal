package slave

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/TING-HiuYu/SlurmPortal/internal/replication"
)

const base = "dc=test"

func snap() replication.Snapshot {
	return replication.Snapshot{
		Version: replication.Version, BaseDN: base,
		Groups: []replication.Group{
			{ID: 1, Name: "sshusers", DN: "cn=sshusers,dc=test", Type: "cn", Class: "posixGroup", GIDNumber: 10000},
			{ID: 2, Name: "lab", DN: "ou=lab,dc=test", Type: "ou", GIDNumber: 10001},
			{ID: 3, Name: "team", DN: "cn=team,ou=lab,dc=test", Type: "cn", Class: "groupOfUniqueNames", GIDNumber: 10002, ParentID: 2},
		},
		Users: []replication.User{
			{ID: 10, Username: "alice", DN: "uid=alice,ou=people,dc=test", UIDNumber: 20000, GIDNumber: 10000, HomeDirectory: "/HOME/sshusers/alice", LoginShell: "/bin/bash", PasswordHash: "{CRYPT}$6$a$1"},
			{ID: 11, Username: "bob", DN: "uid=bob,ou=people,dc=test", UIDNumber: 20001, GIDNumber: 10000, HomeDirectory: "/HOME/sshusers/bob", LoginShell: "/bin/bash", PasswordHash: "{CRYPT}$6$b$1"},
		},
		Memberships: []replication.Membership{{GroupID: 1, UserID: 10}, {GroupID: 1, UserID: 11}, {GroupID: 3, UserID: 10}},
	}
}

func empty() replication.Snapshot { return replication.Snapshot{Version: replication.Version} }

func TestReconcileFromEmpty(t *testing.T) {
	d := newFakeDir()
	if err := Reconcile(context.Background(), d, empty(), snap(), base, "cn=admin,dc=test"); err != nil {
		t.Fatal(err)
	}
	if got := d.attr("cn=sshusers,dc=test", "memberUid"); !slices.Equal(got, []string{"alice", "bob"}) {
		t.Fatalf("posix members = %v", got)
	}
	if got := d.attr("cn=team,ou=lab,dc=test", "uniqueMember"); !slices.Contains(got, "uid=alice,ou=people,dc=test") {
		t.Fatalf("unique members = %v", got)
	}
	// subgroup membership propagates to the GID-bearing OU's companion posix group
	if got := d.attr("cn=managed-2,ou=go-ldap-admin-posix-groups,dc=test", "memberUid"); !slices.Equal(got, []string{"alice"}) {
		t.Fatalf("projected OU group members = %v", got)
	}
	if got := d.attr("cn=managed-3,ou=go-ldap-admin-posix-groups,dc=test", "memberUid"); !slices.Equal(got, []string{"alice"}) {
		t.Fatalf("projected team group members = %v", got)
	}
	if got := d.attr("uid=alice,ou=people,dc=test", "userPassword"); !slices.Equal(got, []string{"{CRYPT}$6$a$1"}) {
		t.Fatalf("password = %v", got)
	}
	// parents created before children
	ou, team := slices.Index(d.ops, "addgroup ou=lab,dc=test"), slices.Index(d.ops, "addgroup cn=team,ou=lab,dc=test")
	if ou < 0 || team < ou {
		t.Fatalf("group creation order wrong: %v", d.ops)
	}
}

func TestReconcileJoinExistingKeepsPasswords(t *testing.T) {
	d := newFakeDir()
	_ = d.AddUser(context.Background(), replication.User{DN: "uid=alice,ou=people,dc=test", Username: "alice", PasswordHash: "local"}, true)
	if err := Reconcile(context.Background(), d, empty(), snap(), base, ""); err != nil {
		t.Fatal(err)
	}
	if got := d.attr("uid=alice,ou=people,dc=test", "userPassword"); !slices.Equal(got, []string{"local"}) {
		t.Fatalf("existing password overwritten: %v", got)
	}
	if got := d.attr("uid=alice,ou=people,dc=test", "uidNumber"); !slices.Equal(got, []string{"20000"}) {
		t.Fatal("existing entry not updated")
	}
}

func TestReconcileChangesAndRemovals(t *testing.T) {
	ctx := context.Background()
	d := newFakeDir()
	old := snap()
	if err := Reconcile(ctx, d, empty(), old, base, ""); err != nil {
		t.Fatal(err)
	}
	next := snap()
	next.Users = next.Users[:1]
	next.Users[0].LoginShell = "/bin/zsh"
	next.Users[0].PasswordHash = "{CRYPT}$6$a$2"
	next.Memberships = []replication.Membership{{GroupID: 1, UserID: 10}}
	next.Groups = next.Groups[:1]
	d.ops = nil
	if err := Reconcile(ctx, d, old, next, base, ""); err != nil {
		t.Fatal(err)
	}
	if d.has("uid=bob,ou=people,dc=test") || d.has("cn=team,ou=lab,dc=test") || d.has("ou=lab,dc=test") {
		t.Fatal("removed entries still present")
	}
	if d.has("ou=go-ldap-admin-posix-groups,dc=test") {
		t.Fatal("empty projection container not removed")
	}
	if got := d.attr("uid=alice,ou=people,dc=test", "shell"); !slices.Equal(got, []string{"/bin/zsh"}) {
		t.Fatal("attribute change not applied")
	}
	if got := d.attr("uid=alice,ou=people,dc=test", "userPassword"); !slices.Equal(got, []string{"{CRYPT}$6$a$2"}) {
		t.Fatal("password change not applied")
	}
	if got := d.attr("cn=sshusers,dc=test", "memberUid"); !slices.Equal(got, []string{"alice"}) {
		t.Fatalf("bob still a member: %v", got)
	}
	d.ops = nil
	if err := Reconcile(ctx, d, next, next, base, ""); err != nil || len(d.ops) != 0 {
		t.Fatalf("no-op reconcile issued %v (%v)", d.ops, err)
	}
}

func TestReconcileRetryAfterFailure(t *testing.T) {
	ctx := context.Background()
	d := newFakeDir()
	d.failOn = "adduser uid=bob"
	if err := Reconcile(ctx, d, empty(), snap(), base, ""); err == nil {
		t.Fatal("injected failure not reported")
	}
	if err := Reconcile(ctx, d, empty(), snap(), base, ""); err != nil {
		t.Fatal("retry from the same baseline failed:", err)
	}
	if !d.has("uid=bob,ou=people,dc=test") || len(d.attr("cn=sshusers,dc=test", "memberUid")) != 2 {
		t.Fatal("retry did not converge")
	}
}

func TestReconcileRejectsInvalid(t *testing.T) {
	bad := snap()
	bad.Users[0].HomeDirectory = ""
	if err := Reconcile(context.Background(), newFakeDir(), empty(), bad, base, ""); err == nil {
		t.Fatal("incomplete POSIX user accepted")
	}
	old := snap()
	next := snap()
	next.Groups[2].Class = "posixGroup"
	if err := Reconcile(context.Background(), newFakeDir(), old, next, base, ""); err == nil {
		t.Fatal("objectClass change accepted")
	}
}

func TestReconcileCertificates(t *testing.T) {
	ctx := context.Background()
	der := []byte{0x30, 0x01}
	pemCert := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	d := newFakeDir()
	s1 := snap()
	s1.UserCertificates = []replication.UserCertificate{{UserID: 10, CAID: 1, SerialHex: "01", CertificatePEM: pemCert}}
	if err := Reconcile(ctx, d, empty(), s1, base, ""); err != nil {
		t.Fatal(err)
	}
	if got := d.attr("uid=alice,ou=people,dc=test", "cert"); len(got) != 1 || got[0] != string(der) {
		t.Fatal("certificate not published")
	}
	s2 := snap()
	if err := Reconcile(ctx, d, s1, s2, base, ""); err != nil {
		t.Fatal(err)
	}
	if got := d.attr("uid=alice,ou=people,dc=test", "cert"); len(got) != 0 {
		t.Fatal("certificate not cleared")
	}
}

func TestRewriteDNs(t *testing.T) {
	s := snap()
	out, err := RewriteDNs(s, "dc=test", "dc=cluster1,dc=lab", "ou=groups,dc=cluster1,dc=lab")
	if err != nil {
		t.Fatal(err)
	}
	if out.Users[0].DN != "uid=alice,ou=people,dc=cluster1,dc=lab" || out.Groups[0].DN != "cn=sshusers,ou=groups,dc=cluster1,dc=lab" || out.Groups[1].DN != "ou=lab,dc=cluster1,dc=lab" {
		t.Fatalf("rewrite: %s %s %s", out.Users[0].DN, out.Groups[0].DN, out.Groups[1].DN)
	}
	if out.Users[0].UIDNumber != 20000 || s.Users[0].DN != "uid=alice,ou=people,dc=test" {
		t.Fatal("rewrite changed identity or mutated input")
	}
	s.Users[0].DN = "uid=x,dc=other"
	if _, err := RewriteDNs(s, "dc=test", "dc=cluster1", ""); err == nil {
		t.Fatal("foreign DN accepted")
	}
	if _, err := RewriteDNs(snap(), "dc=test", "dc=cluster1", "ou=groups,dc=elsewhere"); err == nil {
		t.Fatal("group DN outside base accepted")
	}
}

func testCA(t *testing.T) replication.CA {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ca"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour * 24), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	cert, _ := x509.ParseCertificate(der)
	crl, _ := x509.CreateRevocationList(rand.Reader, &x509.RevocationList{Number: big.NewInt(3), ThisUpdate: time.Now(), NextUpdate: time.Now().Add(time.Hour)}, cert, k)
	return replication.CA{ID: 1, Name: "ca", Kind: "x509", Enabled: true, CRLNumber: 3,
		CertificatePEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		CRLPEM:         string(pem.EncodeToMemory(&pem.Block{Type: "X509 CRL", Bytes: crl}))}
}

func TestPublishTrust(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not installed")
	}
	dir := t.TempDir()
	pub, _, _ := sshKey(t)
	s := replication.Snapshot{Version: replication.Version, CAs: []replication.CA{testCA(t), {ID: 2, Name: "ssh", Kind: "ssh", Enabled: true, CertificatePEM: pub}},
		SSHRevocations: []replication.Revocation{{CAID: 2, SerialHex: "ff"}}}
	if err := PublishTrust(dir, s, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"current/ca-bundle.pem", "current/crl/ca-1.pem", "current/ssh/trusted-user-ca-keys.pub", "current/ssh/revoked.krl"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("missing %s: %v", f, err)
		}
	}
	first, _ := os.Readlink(filepath.Join(dir, "current"))
	if err := PublishTrust(dir, s, time.Now()); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.Readlink(filepath.Join(dir, "current")); again != first {
		t.Fatal("unchanged material produced a new generation")
	}
	s.SSHRevocations = append(s.SSHRevocations, replication.Revocation{CAID: 2, SerialHex: "100"})
	if err := PublishTrust(dir, s, time.Now()); err != nil {
		t.Fatal(err)
	}
	if third, _ := os.Readlink(filepath.Join(dir, "current")); third == first {
		t.Fatal("new revocation did not switch generation")
	}
	if err := PublishTrust(dir, s, time.Now().Add(48*time.Hour)); err == nil {
		t.Fatal("expired CRL published")
	}
	bad := s
	bad.CAs = []replication.CA{testCA(t)}
	bad.CAs[0].CRLNumber = 9
	if err := PublishTrust(t.TempDir(), bad, time.Now()); err == nil {
		t.Fatal("CRL number mismatch accepted")
	}
}

func sshKey(t *testing.T) (string, ssh.Signer, error) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := ssh.NewSignerFromKey(priv)
	return string(ssh.MarshalAuthorizedKey(signer.PublicKey())), signer, nil
}

func writeRules(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "triggers.yaml")
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	// WriteFile is subject to umask; the test needs the exact mode.
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadTriggers(t *testing.T) {
	good := "version: 1\ntriggers:\n  - id: home\n    enabled: true\n    events: [user.created]\n    command: [/usr/bin/true, \"%DATA%\"]\n    timeoutSeconds: 5\n"
	if _, err := LoadTriggers(writeRules(t, good, 0o600)); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		body string
		mode os.FileMode
	}{
		"world readable": {good, 0o644},
		"shell":          {strings.Replace(good, "/usr/bin/true", "/bin/sh", 1), 0o600},
		"relative":       {strings.Replace(good, "/usr/bin/true", "true", 1), 0o600},
		"no data":        {strings.Replace(good, `"%DATA%"`, "x", 1), 0o600},
		"embedded data":  {strings.Replace(good, `"%DATA%"`, `"--x=%DATA%"`, 1), 0o600},
		"bad event":      {strings.Replace(good, "user.created", "user.exploded", 1), 0o600},
		"unknown field":  {good + "    extra: 1\n", 0o600},
		"bad version":    {strings.Replace(good, "version: 1", "version: 2", 1), 0o600},
	} {
		if _, err := LoadTriggers(writeRules(t, c.body, c.mode)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestQueue(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "ran")
	hook := filepath.Join(t.TempDir(), "hook")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nprintf '%s\\n' \"$1\" >> "+marker+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	q := &Queue{Dir: dir, Now: time.Now, Rules: []TriggerRule{
		{ID: "record", Enabled: true, Events: []string{"user.created"}, Command: []string{hook, "%DATA%"}, TimeoutSeconds: 5},
		{ID: "guarded", Enabled: true, Events: []string{"user.updated"}, Command: []string{"/usr/bin/false", "%DATA%"}, TimeoutSeconds: 5, CurrentUserOnly: true},
	}}
	old, next := empty(), snap()
	if err := q.Enqueue(old, next); err != nil {
		t.Fatal(err)
	}
	if n, _ := q.Pending(); n != 2 {
		t.Fatalf("pending = %d, want 2 user.created deliveries", n)
	}
	_ = q.Dispatch(ctx, next)
	ran, err := os.ReadFile(marker)
	if err != nil || strings.Count(string(ran), "\n") != 2 || !strings.Contains(string(ran), `"event":"user.created"`) || !strings.Contains(string(ran), `"username":"alice"`) {
		t.Fatalf("trigger output = %q (%v)", ran, err)
	}
	if n, _ := q.Pending(); n != 0 {
		t.Fatalf("deliveries left: %d", n)
	}

	changed := snap()
	changed.Users[0].LoginShell = "/bin/zsh"
	if err := q.Enqueue(next, changed); err != nil {
		t.Fatal(err)
	}
	newer := snap()
	newer.Users[0].LoginShell = "/bin/fish"
	if err := q.Dispatch(ctx, newer); err != nil {
		t.Fatal("obsolete guarded delivery should be dropped, not run:", err)
	}
	if err := q.Enqueue(next, changed); err != nil {
		t.Fatal(err)
	}
	if err := q.Dispatch(ctx, changed); err == nil {
		t.Fatal("failing current delivery reported success")
	}
	if n, _ := q.Pending(); n != 1 {
		t.Fatal("failed delivery not kept for retry")
	}
}
