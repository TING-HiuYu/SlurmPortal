package slave

import (
	"context"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	ldap "github.com/go-ldap/ldap/v3"

	"github.com/TING-HiuYu/SlurmPortal/internal/password"
	"github.com/TING-HiuYu/SlurmPortal/internal/replication"
)

// startSlapd runs a throwaway OpenLDAP when SLAPD (path to the slapd binary) and SLAPD_SCHEMA are set.
func startSlapd(t *testing.T) LDAPConfig {
	t.Helper()
	bin, schema := os.Getenv("SLAPD"), os.Getenv("SLAPD_SCHEMA")
	if bin == "" || schema == "" {
		t.Skip("set SLAPD and SLAPD_SCHEMA to run the OpenLDAP integration test")
	}
	dir := t.TempDir()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	conf := fmt.Sprintf(`include %[1]s/core.schema
include %[1]s/cosine.schema
include %[1]s/inetorgperson.schema
include %[1]s/nis.schema
pidfile %[2]s/slapd.pid
database mdb
maxsize 104857600
suffix "dc=test"
rootdn "cn=admin,dc=test"
rootpw secret
directory %[2]s/data
`, schema, dir)
	if err := os.Mkdir(filepath.Join(dir, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "slapd.conf"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	url := fmt.Sprintf("ldap://127.0.0.1:%d", port)
	cmd := exec.Command(bin, "-f", filepath.Join(dir, "slapd.conf"), "-h", url, "-d", "0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	cfg := LDAPConfig{URL: url, BindDN: "cn=admin,dc=test", Password: "secret"}
	for i := 0; ; i++ {
		c, err := DialLDAP(cfg)
		if err == nil {
			seed := []*ldap.AddRequest{ldap.NewAddRequest("dc=test", nil), ldap.NewAddRequest("ou=people,dc=test", nil)}
			seed[0].Attribute("objectClass", []string{"dcObject", "organization"})
			seed[0].Attribute("dc", []string{"test"})
			seed[0].Attribute("o", []string{"test"})
			seed[1].Attribute("objectClass", []string{"organizationalUnit"})
			seed[1].Attribute("ou", []string{"people"})
			for _, r := range seed {
				if err := c.conn.Add(r); err != nil {
					t.Fatal(err)
				}
			}
			c.Close()
			return cfg
		}
		if i > 50 {
			t.Fatalf("slapd did not start: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func search(t *testing.T, cfg LDAPConfig, dn string, attrs ...string) *ldap.Entry {
	t.Helper()
	c, err := DialLDAP(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	res, err := c.conn.Search(ldap.NewSearchRequest(dn, ldap.ScopeBaseObject, ldap.NeverDerefAliases, 0, 0, false, "(objectClass=*)", attrs, nil))
	if ldap.IsErrorWithCode(err, ldap.LDAPResultNoSuchObject) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return res.Entries[0]
}

func TestOpenLDAPIntegration(t *testing.T) {
	cfg := startSlapd(t)
	ctx := context.Background()
	cred, _ := password.Hash("alice-password-1")
	s1 := snap()
	s1.Users[0].PasswordHash = cred.LDAP
	s1.Users[0].Mail = ""
	ca := testCA(t)
	block, _ := pem.Decode([]byte(ca.CertificatePEM))
	der := block.Bytes
	s1.UserCertificates = []replication.UserCertificate{{UserID: 10, CAID: 1, SerialHex: "01", CertificatePEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}}
	run := func(old, next replication.Snapshot) {
		t.Helper()
		d, err := DialLDAP(cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		if err := Reconcile(ctx, d, old, next, "dc=test", cfg.BindDN); err != nil {
			t.Fatal(err)
		}
	}
	run(empty(), s1)

	alice := search(t, cfg, "uid=alice,ou=people,dc=test", "uidNumber", "homeDirectory", "objectClass", "userPassword", "mail", "sn")
	if alice == nil || alice.GetAttributeValue("uidNumber") != "20000" || alice.GetAttributeValue("sn") != "alice" || alice.GetAttributeValue("mail") != "" {
		t.Fatalf("alice entry wrong: %+v", alice)
	}
	if got := alice.GetAttributeValue("userPassword"); got != cred.LDAP {
		t.Fatalf("userPassword = %q", got)
	}
	if !slices.Contains(alice.GetAttributeValues("objectClass"), "pkiUser") {
		t.Fatal("certificate class missing")
	}
	if g := search(t, cfg, "cn=sshusers,dc=test", "memberUid"); !slices.Equal(g.GetAttributeValues("memberUid"), []string{"alice", "bob"}) {
		t.Fatalf("memberUid = %v", g.GetAttributeValues("memberUid"))
	}
	if g := search(t, cfg, "cn=managed-2,ou=go-ldap-admin-posix-groups,dc=test", "gidNumber"); g == nil || g.GetAttributeValue("gidNumber") != "10001" {
		t.Fatal("projected POSIX group missing")
	}

	if runtime.GOOS == "linux" {
		c, err := ldap.DialURL(cfg.URL)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Bind("uid=alice,ou=people,dc=test", "alice-password-1"); err != nil {
			t.Fatalf("bind with {CRYPT}$6$ hash failed: %v", err)
		}
		if err := c.Bind("uid=alice,ou=people,dc=test", "wrong"); err == nil {
			t.Fatal("wrong password accepted")
		}
		c.Close()
	}

	s2 := snap()
	s2.Users = s2.Users[:1]
	s2.Users[0].Mail = "alice@example.org"
	s2.Users[0].LoginShell = "/bin/zsh"
	s2.Users[0].PasswordHash = cred.LDAP
	s2.Groups = s2.Groups[:1]
	s2.Memberships = []replication.Membership{{GroupID: 1, UserID: 10}}
	run(s1, s2)
	alice = search(t, cfg, "uid=alice,ou=people,dc=test", "mail", "loginShell", "userCertificate;binary")
	if alice.GetAttributeValue("mail") != "alice@example.org" || alice.GetAttributeValue("loginShell") != "/bin/zsh" || len(alice.GetRawAttributeValues("userCertificate;binary")) != 0 {
		t.Fatalf("update not applied: %+v", alice)
	}
	for _, dn := range []string{"uid=bob,ou=people,dc=test", "ou=lab,dc=test", "cn=team,ou=lab,dc=test", "ou=go-ldap-admin-posix-groups,dc=test"} {
		if search(t, cfg, dn) != nil {
			t.Fatalf("%s should be gone", dn)
		}
	}
	run(s2, s2)
}
