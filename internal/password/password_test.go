package password

import (
	"strings"
	"testing"
)

// Vectors from Drepper's SHA-crypt specification.
func TestCryptSHA512Vectors(t *testing.T) {
	cases := []struct {
		salt   string
		rounds int
		pw     string
		want   string
	}{
		{"saltstring", 0, "Hello world!", "$6$saltstring$svn8UoSVapNtMuq1ukKS4tPQd8iKwSMHWjl/O817G3uBnIFNjnQJuesI68u4OTLiBFdcbYEdFCoEOfaS35inz1"},
		{"saltstringsaltstring", 10000, "Hello world!", "$6$rounds=10000$saltstringsaltst$OW1/O6BYHV6BcXZu8QVeXbDWra3Oeqh0sbHbbMCVNSnCM/UrjmM0Dp8vOuZeHBy/YTBmSK6H9qs/y3RnOaw5v."},
		{"toolongsaltstring", 5000, "This is just a test", "$6$rounds=5000$toolongsaltstrin$lQ8jolhgVRVhY4b5pZKaysCLi0QBxGoNeKQzQ3glMhwllF7oGDZxUhx1yxdYcz/e1JSbq3y6JMxxl8audkUEm0"},
		{"anotherlongsaltstring", 1400, "a very much longer text to encrypt.  This one even stretches over morethan one line.", "$6$rounds=1400$anotherlongsalts$POfYwTEok97VWcjxIiSOjiykti.o/pQs.wPvMxQ6Fm7I6IoYN3CmLs66x9t0oSwbtEW7o7UmJEiDwGqd8p4ur1"},
		{"roundstoolow", 10, "the minimum number is still observed", "$6$rounds=1000$roundstoolow$kUMsbe306n21p9R.FRkW3IGn.S9NPN0x50YhH1xhLsPuWGsUSklZt58jaTfF4ZEQpyUNGc0dqbpBYYBaHHrsX."},
	}
	for _, tc := range cases {
		got, err := CryptSHA512([]byte(tc.pw), tc.salt, tc.rounds)
		if err != nil || got != tc.want {
			t.Errorf("salt %q:\n got %s\nwant %s (%v)", tc.salt, got, tc.want, err)
		}
		if !VerifyLDAPCrypt("{CRYPT}"+tc.want, tc.pw) || VerifyLDAPCrypt("{CRYPT}"+tc.want, tc.pw+"x") {
			t.Errorf("verify mismatch for salt %q", tc.salt)
		}
	}
}

func TestHashAndVerify(t *testing.T) {
	c, err := Hash("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(c.App, "$argon2id$v=19$m=19456,t=2,p=1$") || !strings.HasPrefix(c.LDAP, "{CRYPT}$6$rounds=100000$") {
		t.Fatalf("formats: %s %s", c.App, c.LDAP)
	}
	if ok, err := Verify(c.App, "correct horse battery"); !ok || err != nil {
		t.Fatal("valid password rejected")
	}
	if ok, _ := Verify(c.App, "wrong horse battery"); ok {
		t.Fatal("wrong password accepted")
	}
	if !VerifyLDAPCrypt(c.LDAP, "correct horse battery") {
		t.Fatal("ldap hash mismatch")
	}
	c2, _ := Hash("correct horse battery")
	if c2.App == c.App || c2.LDAP == c.LDAP {
		t.Fatal("salts reused")
	}
}

func TestPolicyAndMalformed(t *testing.T) {
	for _, pw := range []string{"", "short", strings.Repeat("x", MaxLength+1), "\xff\xfe\xfd\xfc\xfb\xfa\xf9\xf8\xf7\xf6"} {
		if _, err := Hash(pw); err == nil {
			t.Errorf("policy accepted %q", pw)
		}
	}
	if _, err := Derive("legacy"); err != nil {
		t.Fatal("legacy short password not derivable")
	}
	for _, h := range []string{
		"", "plain", "$argon2i$v=19$m=19456,t=2,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=999999999,t=2,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA",
		"$argon2id$v=19$m=19456,t=2,p=1$!!$aGFzaGhhc2hoYXNoaGFzaA",
	} {
		if ok, err := Verify(h, "x"); ok || err == nil {
			t.Errorf("malformed hash accepted: %q", h)
		}
	}
	if VerifyLDAPCrypt("$6$salt$abc", "x") || VerifyLDAPCrypt("{CRYPT}$1$salt$abc", "x") {
		t.Fatal("bad ldap value accepted")
	}
}
