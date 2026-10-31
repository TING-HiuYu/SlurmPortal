package slave

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/replication"
)

const planBase = "dc=slurm,dc=lan"

func testCert(t *testing.T, serial int64) []byte {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "alice"}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// fold wraps an LDIF line at 76 columns the way slapcat does.
func fold(line string) string {
	var b strings.Builder
	for len(line) > 76 {
		b.WriteString(line[:76] + "\n ")
		line = line[76:]
	}
	return b.String() + line
}

func wanted(certPEM string) replication.Snapshot {
	return replication.Snapshot{Version: replication.Version, BaseDN: planBase,
		Users: []replication.User{
			{ID: 10, Username: "alice", DN: "uid=alice,ou=people," + planBase, Nickname: "Alice", Mail: "alice@example.org",
				UIDNumber: 10001, GIDNumber: 10001, HomeDirectory: "/HOME/alice", LoginShell: "/bin/bash", PasswordHash: "{CRYPT}$6$x"},
			{ID: 11, Username: "bob", DN: "uid=bob,ou=people," + planBase, UIDNumber: 10002, GIDNumber: 10001, HomeDirectory: "/HOME/bob"},
		},
		Groups: []replication.Group{
			{ID: 1, Name: "people", Type: "ou", DN: "ou=people," + planBase},
			{ID: 2, Name: "lab", Type: "cn", Class: "posixGroup", GIDNumber: 10001, DN: "cn=lab," + planBase, Remark: "Lab members"},
			{ID: 3, Name: "admins", Type: "cn", Class: "groupOfUniqueNames", DN: "cn=admins," + planBase},
		},
		Memberships:      []replication.Membership{{GroupID: 2, UserID: 10}, {GroupID: 2, UserID: 11}, {GroupID: 3, UserID: 10}},
		UserCertificates: []replication.UserCertificate{{UserID: 10, SerialHex: "07", CertificatePEM: certPEM}},
	}
}

func liveLDIF(der []byte) string {
	return strings.Join([]string{
		"version: 1", "",
		"dn: " + planBase, "objectClass: dcObject", "dc: slurm", "",
		"# people", "dn: ou=people," + planBase, "objectClass: organizationalUnit", "objectClass: top", "ou: people", "description: people", "",
		"dn: uid=alice,ou=people," + planBase, "objectClass: inetOrgPerson", "objectClass: posixAccount", "objectClass: pkiUser",
		"cn: alice", "sn: Alice", "uid: alice", "displayName: Alice", "mail: alice@example.org", "uidNumber: 10001", "gidNumber: 10001",
		"homeDirectory: /HOME/alice", "loginShell: /bin/bash", "userPassword:: " + base64.StdEncoding.EncodeToString([]byte("{CRYPT}$6$x")),
		fold("userCertificate;binary:: " + base64.StdEncoding.EncodeToString(der)), "",
		"dn: uid=bob,ou=people," + planBase, "objectClass: inetOrgPerson", "objectClass: posixAccount", "cn: bob", "sn: bob", "uid: bob",
		"uidNumber: 10002", "gidNumber: 10001", "homeDirectory: /HOME/bob", "",
		"dn: cn=lab," + planBase, "objectClass: posixGroup", "objectClass: top", "cn: lab", "gidNumber: 10001", "description: Lab members", "memberUid: alice", "memberUid: bob", "",
		"dn: cn=admins," + planBase, "objectClass: groupOfUniqueNames", "objectClass: top", "cn: admins", "description: admins",
		"uniqueMember: cn=admin," + planBase, "uniqueMember: uid=alice,ou=people," + planBase, "",
		"dn: cn=admin," + planBase, "objectClass: inetOrgPerson", "cn: admin", "sn: admin", "uid: admin", "",
	}, "\n")
}

func TestUnchangedDirectoryPlansNothing(t *testing.T) {
	der := testCert(t, 7)
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	entries, err := ParseLDIF(strings.NewReader(liveLDIF(der)))
	if err != nil {
		t.Fatal(err)
	}
	observed := Observed(entries, planBase, "cn=admin,"+planBase)
	if len(observed.Users) != 2 || len(observed.UserCertificates) != 1 || observed.UserCertificates[0].SerialHex != "07" {
		t.Fatalf("observed %+v", observed)
	}
	ops, err := Plan(context.Background(), Baseline(observed, wanted(certPEM)), wanted(certPEM), planBase, "cn=admin,"+planBase)
	if err != nil || len(ops) != 0 {
		t.Fatalf("unchanged directory would be modified: %v %+v", err, ops)
	}
}

func TestPlanReportsLoginRelevantChanges(t *testing.T) {
	der := testCert(t, 7)
	entries, _ := ParseLDIF(strings.NewReader(liveLDIF(der)))
	observed := Observed(entries, planBase, "cn=admin,"+planBase)
	next := wanted(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
	next.Users[0].UIDNumber = 20001
	next.Users[0].Mail = "a@example.org"
	next.Users = next.Users[:1] // bob disappears
	next.Memberships = []replication.Membership{{GroupID: 2, UserID: 10}, {GroupID: 3, UserID: 10}}
	ops, err := Plan(context.Background(), observed, next, planBase, "cn=admin,"+planBase)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Op{}
	for _, op := range ops {
		got[op.Kind+" "+op.DN] = op
	}
	if op, ok := got["delete uid=bob,ou=people,"+planBase]; !ok || !op.Login {
		t.Errorf("bob deletion missing: %+v", ops)
	}
	if op := got["update-user uid=alice,ou=people,"+planBase]; !op.Login || strings.Join(op.Fields, ",") != "uidNumber,mail" {
		t.Errorf("alice update %+v", op)
	}
	if _, ok := got["remove-member cn=lab,"+planBase+" <- uid=bob,ou=people,"+planBase]; !ok {
		t.Errorf("membership removal missing: %+v", ops)
	}
}

func TestGuard(t *testing.T) {
	users := func(n int) []replication.User {
		var out []replication.User
		for i := range n {
			out = append(out, replication.User{Username: string(rune('a' + i)), DN: "uid=" + string(rune('a'+i))})
		}
		return out
	}
	ca := func(key string) replication.CA {
		return replication.CA{Kind: "ssh", Enabled: true, CertificatePEM: "ssh-ed25519 " + key + " comment"}
	}
	old := replication.Snapshot{Users: users(10), CAs: []replication.CA{ca("AAAA"), ca("BBBB")}}
	if err := Guard(old, replication.Snapshot{Users: users(5), CAs: old.CAs}, Limits{}); err != nil {
		t.Errorf("5 deletions refused: %v", err)
	}
	if err := Guard(old, replication.Snapshot{Users: users(4), CAs: old.CAs}, Limits{}); err == nil || !strings.Contains(err.Error(), "delete 6 users") {
		t.Errorf("6 deletions allowed: %v", err)
	}
	if err := Guard(old, replication.Snapshot{Users: users(0), CAs: old.CAs}, Limits{MaxUserDeletions: 10}); err != nil {
		t.Errorf("raised limit ignored: %v", err)
	}
	dropped := replication.Snapshot{Users: old.Users, CAs: []replication.CA{ca("AAAA"), ca("CCCC")}}
	if err := Guard(old, dropped, Limits{}); err == nil || !strings.Contains(err.Error(), "BBBB") {
		t.Errorf("CA removal allowed: %v", err)
	}
	if err := Guard(old, dropped, Limits{AllowCARemoval: true}); err != nil {
		t.Errorf("explicit CA removal refused: %v", err)
	}
}

func TestTrustedCAs(t *testing.T) {
	dir := t.TempDir()
	if cas, err := TrustedCAs(dir); err != nil || cas != nil {
		t.Fatalf("missing file: %v %v", cas, err)
	}
	p := filepath.Join(dir, "current", "ssh")
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "trusted-user-ca-keys.pub"), []byte("# bundle\nssh-ed25519 AAAA ca-1\n\nssh-ed25519 BBBB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cas, err := TrustedCAs(dir)
	if err != nil || len(cas) != 2 || cas[0].CertificatePEM != "ssh-ed25519 AAAA" {
		t.Fatalf("%v %+v", err, cas)
	}
}

func TestFirstSyncBaseline(t *testing.T) {
	der := testCert(t, 7)
	entries, _ := ParseLDIF(strings.NewReader(liveLDIF(der)))
	next := wanted(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
	next.Users[0].PasswordHash = "{CRYPT}$6$different"
	observed := Observed(entries, planBase, "cn=admin,"+planBase)
	raw := Baseline(observed, next)
	raw.Users[0].PasswordHash = "{CRYPT}$6$x" // what a baseline that kept LDAP's hash would hold
	if ops, _ := Plan(context.Background(), raw, next, planBase, "cn=admin,"+planBase); len(ops) != 1 || ops[0].Kind != "set-password" {
		t.Fatalf("password change not planned: %+v", ops)
	}
	if ops, _ := Plan(context.Background(), Baseline(observed, next), next, planBase, "cn=admin,"+planBase); len(ops) != 0 {
		t.Fatalf("first sync would rewrite passwords: %+v", ops)
	}
}

func TestFirstSyncLeavesLocalEntries(t *testing.T) {
	der := testCert(t, 7)
	ldif := liveLDIF(der) + "\ndn: cn=cluster-users," + planBase + "\nobjectClass: posixGroup\ncn: cluster-users\ngidNumber: 3000\nmemberUid: alice\n\n" +
		"dn: uid=local,ou=people," + planBase + "\nobjectClass: inetOrgPerson\nuid: local\ncn: local\nsn: local\n"
	entries, err := ParseLDIF(strings.NewReader(ldif))
	if err != nil {
		t.Fatal(err)
	}
	next := wanted(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
	observed := Observed(entries, planBase, "cn=admin,"+planBase)
	if len(observed.Users) != 3 {
		t.Fatalf("observed users %d", len(observed.Users))
	}
	if ops, err := Plan(context.Background(), Baseline(observed, next), next, planBase, "cn=admin,"+planBase); err != nil || len(ops) != 0 {
		t.Fatalf("first sync touches local entries: %v %+v", err, ops)
	}
}

// The first sync diffs against the live directory. Its entries carry IDs made up while reading
// LDAP, so Baseline must adopt master's IDs: an unchanged directory yields no trigger events, never
// a delete and re-create of every user (which would deprovision Slurm accounts).
func TestFirstSyncTriggersOnlyRealChanges(t *testing.T) {
	der := testCert(t, 7)
	entries, err := ParseLDIF(strings.NewReader(liveLDIF(der)))
	if err != nil {
		t.Fatal(err)
	}
	next := wanted(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
	base := Baseline(Observed(entries, planBase, "cn=admin,"+planBase), next)
	if evs := diffEvents(base, next); len(evs) != 0 {
		t.Fatalf("unchanged directory produced trigger events: %+v", evs)
	}
	next.Users[1].LoginShell = "/bin/zsh"
	evs := diffEvents(base, next)
	if len(evs) != 1 || evs[0].Name != "user.updated" {
		t.Fatalf("one real change: %+v", evs)
	}
}
