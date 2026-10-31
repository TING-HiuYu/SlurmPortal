package pki_test

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/TING-HiuYu/SlurmPortal/internal/app"
	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
	"github.com/TING-HiuYu/SlurmPortal/internal/config"
	"github.com/TING-HiuYu/SlurmPortal/internal/domain/pki"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
	"github.com/TING-HiuYu/SlurmPortal/internal/testutil"
)

var masterKey = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))

func newService(t *testing.T) (*pki.Service, store.Store) {
	t.Helper()
	db := testutil.DB(t)
	s, err := pki.New(context.Background(), db, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	return s, db
}

func parseCRL(t *testing.T, ca pki.CA) *x509.RevocationList {
	t.Helper()
	cb, _ := pem.Decode([]byte(ca.CertificatePEM))
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := pem.Decode([]byte(ca.CRLPEM))
	crl, err := x509.ParseRevocationList(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := crl.CheckSignatureFrom(cert); err != nil {
		t.Fatalf("CRL signature: %v", err)
	}
	return crl
}

func crlHas(crl *x509.RevocationList, serialHex string) bool {
	want, _ := new(big.Int).SetString(serialHex, 16)
	for _, e := range crl.RevokedCertificateEntries {
		if e.SerialNumber.Cmp(want) == 0 {
			return true
		}
	}
	return false
}

func TestX509Lifecycle(t *testing.T) {
	ctx := context.Background()
	s, db := newService(t)
	ca, key, err := s.GenerateX509(ctx, "Users CA")
	if err != nil || len(key) == 0 || ca.CRLNumber != 1 {
		t.Fatalf("generate: %+v %v", ca, err)
	}
	if _, _, err := s.GenerateX509(ctx, "Users CA"); err == nil {
		t.Fatal("duplicate CA name accepted")
	}
	uid := testutil.User(t, db, "alice", 1)

	issued, err := s.Assign(ctx, uid, ca.ID)
	if err != nil {
		t.Fatal(err)
	}
	cb, _ := pem.Decode([]byte(issued.CertificatePEM))
	leaf, _ := x509.ParseCertificate(cb.Bytes)
	caBlock, _ := pem.Decode([]byte(ca.CertificatePEM))
	caCert, _ := x509.ParseCertificate(caBlock.Bytes)
	if err := leaf.CheckSignatureFrom(caCert); err != nil || leaf.Subject.CommonName != "alice" || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Fatalf("leaf: %v %+v", err, leaf.Subject)
	}
	if _, err := s.Assign(ctx, uid, ca.ID); err == nil {
		t.Fatal("second assignment accepted")
	}

	rotated, err := s.Rotate(ctx, uid)
	if err != nil || rotated.SerialHex == issued.SerialHex {
		t.Fatalf("rotate: %v", err)
	}
	ca, _ = s.CA(ctx, ca.ID)
	if crl := parseCRL(t, ca); !crlHas(crl, issued.SerialHex) || crlHas(crl, rotated.SerialHex) || ca.CRLNumber != 2 {
		t.Fatalf("CRL after rotate wrong (number %d)", ca.CRLNumber)
	}
	mine, err := s.Mine(ctx, uid)
	if err != nil || mine.SerialHex != rotated.SerialHex {
		t.Fatalf("mine: %+v %v", mine, err)
	}

	if err := s.RevokeUser(ctx, uid); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeUser(ctx, uid); err != nil {
		t.Fatal("repeat revoke failed:", err)
	}
	ca, _ = s.CA(ctx, ca.ID)
	if crl := parseCRL(t, ca); !crlHas(crl, rotated.SerialHex) {
		t.Fatal("revoked serial missing from CRL")
	}
	if _, err := s.Mine(ctx, uid); err == nil {
		t.Fatal("assignment survived revocation")
	}

	if err := s.Revoke(ctx, ca.ID, "0xABCDEF", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(ctx, ca.ID, "abcdef", 1); err != nil {
		t.Fatal("repeat admin revoke failed:", err)
	}
	if err := s.Revoke(ctx, ca.ID, "zz", 1); err == nil {
		t.Fatal("bad serial accepted")
	}
	ca, _ = s.CA(ctx, ca.ID)
	if !crlHas(parseCRL(t, ca), "abcdef") {
		t.Fatal("admin revocation missing")
	}
}

func TestSSHLifecycle(t *testing.T) {
	ctx := context.Background()
	s, db := newService(t)
	ca, priv, err := s.GenerateSSH(ctx, "ssh users")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ssh.ParsePrivateKey(priv); err != nil {
		t.Fatal("generated CA key unparsable:", err)
	}
	caPub, _, _, _, _ := ssh.ParseAuthorizedKey([]byte(ca.CertificatePEM))
	uid := testutil.User(t, db, "bob", 1)
	issued, err := s.Assign(ctx, uid, ca.ID)
	if err != nil {
		t.Fatal(err)
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(issued.CertificatePEM))
	if err != nil {
		t.Fatal(err)
	}
	cert := pub.(*ssh.Certificate)
	checker := ssh.CertChecker{IsUserAuthority: func(k ssh.PublicKey) bool { return bytes.Equal(k.Marshal(), caPub.Marshal()) }}
	if err := checker.CheckCert("bob", cert); err != nil {
		t.Fatal("cert does not verify:", err)
	}
	if err := checker.CheckCert("root", cert); err == nil {
		t.Fatal("cert valid for another principal")
	}
	userKey, err := ssh.ParsePrivateKey([]byte(issued.PrivateKeyPEM))
	if err != nil || !bytes.Equal(userKey.PublicKey().Marshal(), cert.Key.Marshal()) {
		t.Fatal("private key does not match certificate")
	}

	again, err := s.IssueMine(ctx, uid)
	if err != nil || again.SerialHex == issued.SerialHex {
		t.Fatalf("self rotate: %v", err)
	}
	var n int
	_ = store.QueryRow(ctx, db, "SELECT COUNT(*) FROM revoked_certificates WHERE serial_hex = ?", []any{issued.SerialHex}, &n)
	if n != 1 {
		t.Fatal("old SSH serial not revoked")
	}
	if err := s.Revoke(ctx, ca.ID, "ffffffffffffffffff", 0); err == nil {
		t.Fatal("SSH serial over 64 bits accepted")
	}
}

func TestConcurrentRevocationsSerialiseOnCRL(t *testing.T) {
	ctx := context.Background()
	s, db := newService(t)
	ca, _, _ := s.GenerateX509(ctx, "CA")
	var ids []int64
	for i := range 4 {
		id := testutil.User(t, db, "u"+strconv.Itoa(i), 1)
		if _, err := s.Assign(ctx, id, ca.ID); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(ids))
	for _, id := range ids {
		wg.Go(func() { errs <- s.RevokeUser(ctx, id) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	ca, _ = s.CA(ctx, ca.ID)
	if got := len(parseCRL(t, ca).RevokedCertificateEntries); got != 4 {
		t.Fatalf("CRL has %d entries, want 4", got)
	}
}

func TestImportValidation(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	k1, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	k2, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	mk := func(isCA bool, notAfter time.Time) []byte {
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "x"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter,
			IsCA: isCA, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
		der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k1.PublicKey, k1)
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	}
	keyPEM := func(k *ecdsa.PrivateKey) []byte {
		der, _ := x509.MarshalPKCS8PrivateKey(k)
		return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	}
	future := time.Now().Add(24 * time.Hour)
	for name, c := range map[string][2][]byte{
		"mismatched key": {mk(true, future), keyPEM(k2)},
		"not a CA":       {mk(false, future), keyPEM(k1)},
		"expired":        {mk(true, time.Now().Add(-time.Minute)), keyPEM(k1)},
		"garbage":        {[]byte("nope"), keyPEM(k1)},
	} {
		if _, err := s.ImportX509(ctx, name, c[0], c[1]); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := s.ImportX509(ctx, "good", mk(true, future), keyPEM(k1)); err != nil {
		t.Fatal(err)
	}
}

// legacySeal reproduces v1's encryption so existing production CA rows keep working.
func legacySeal(t *testing.T, key, aad, plain []byte) string {
	block, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(block)
	nonce := make([]byte, aead.NonceSize())
	_, _ = rand.Read(nonce)
	fp := sha256.Sum256(aad)
	return base64.StdEncoding.EncodeToString(aead.Seal(nonce, nonce, plain, fp[:]))
}

func TestMasterKeyChecksAndLegacyFormat(t *testing.T) {
	ctx := context.Background()
	db := testutil.DB(t)
	if _, err := pki.New(ctx, db, ""); err != nil {
		t.Fatal("no CAs should not require a key:", err)
	}
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(9), Subject: pkix.Name{CommonName: "legacy"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().AddDate(1, 0, 0), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	priv, _ := x509.MarshalPKCS8PrivateKey(k)
	raw, _ := base64.StdEncoding.DecodeString(masterKey)
	if _, err := db.Exec(ctx, `INSERT INTO certificate_authorities (created_at, name, kind, certificate_pem, private_key_ciphertext, crl_pem, crl_number, enabled)
		VALUES ('2026-01-02 03:04:05.123+00:00', 'legacy', 'x509', ?, ?, '', 3, 1)`,
		string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), legacySeal(t, raw, der, priv)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO revoked_certificates (created_at, certificate_authority_id, serial_hex, revoked_at, reason)
		VALUES ('2026-01-02 03:04:05+00:00', 1, '0a', '2026-01-02 03:04:05.5+00:00', 4)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pki.New(ctx, db, ""); err == nil {
		t.Fatal("missing key accepted with CAs present")
	}
	if _, err := pki.New(ctx, db, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32))); err == nil {
		t.Fatal("wrong key accepted")
	}
	s, err := pki.New(ctx, db, masterKey)
	if err != nil {
		t.Fatal("legacy ciphertext rejected:", err)
	}
	if err := s.RenewExpiring(ctx); err != nil {
		t.Fatal(err)
	}
	ca, _ := s.CA(ctx, 1)
	if ca.CRLNumber != 4 || !crlHas(parseCRL(t, ca), "0a") {
		t.Fatalf("renewed CRL wrong: number %d", ca.CRLNumber)
	}
	if err := s.RenewExpiring(ctx); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.CA(ctx, 1); again.CRLNumber != 4 {
		t.Fatal("fresh CRL re-signed unnecessarily")
	}
}

func TestHTTPAndDeactivationHook(t *testing.T) {
	db := testutil.DB(t)
	root := testutil.User(t, db, "root", 1, "admin")
	alice := testutil.User(t, db, "alice", 1, "user")
	cfg := &config.Config{
		Role: "master", HTTP: config.HTTP{RatePerSecond: 1e6, RateBurst: 1e6},
		Auth:         config.Auth{Issuer: "i", Audience: "a", TokenTTL: time.Hour},
		JWTKeys:      authn.Keyring{ActiveKID: "k", Keys: map[string][]byte{"k": []byte("0123456789abcdef0123456789abcdef")}},
		PKIMasterKey: masterKey,
	}
	a, err := app.New(context.Background(), cfg, db)
	if err != nil {
		t.Fatal(err)
	}
	tok := func(id int64) string { s, _, _, _ := a.JWT.Issue(id, authn.SourcePassword); return s }
	call := func(id int64, method, path, body string, secure bool) (int, json.RawMessage) {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req.RemoteAddr = "198.51.100.7:1"
		if secure {
			req.TLS = &tls.ConnectionState{}
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tok(id))
		rec := httptest.NewRecorder()
		a.Handler.ServeHTTP(rec, req)
		var env struct {
			Data json.RawMessage `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &env)
		return rec.Code, env.Data
	}
	if c, _ := call(root, "POST", "/api/v2/pki/cas", `{"kind":"ssh","name":"ssh"}`, false); c != 403 {
		t.Fatalf("plain HTTP CA creation: %d", c)
	}
	if c, _ := call(alice, "POST", "/api/v2/pki/cas", `{"kind":"ssh","name":"ssh"}`, true); c != 403 {
		t.Fatalf("non-admin CA creation: %d", c)
	}
	c, data := call(root, "POST", "/api/v2/pki/cas", `{"kind":"ssh","name":"ssh"}`, true)
	var created struct {
		CA            pki.CA `json:"ca"`
		PrivateKeyPEM string `json:"privateKeyPem"`
	}
	_ = json.Unmarshal(data, &created)
	if c != 201 || created.PrivateKeyPEM == "" {
		t.Fatalf("create CA: %d", c)
	}
	if c, data := call(root, "GET", "/api/v2/pki/cas", "", false); c != 200 || bytes.Contains(data, []byte("PRIVATE")) || bytes.Contains(data, []byte("ciphertext")) {
		t.Fatalf("CA list leaked secrets or failed: %d %s", c, data)
	}
	path := "/api/v2/users/" + strconv.FormatInt(alice, 10) + "/certificate"
	if c, _ := call(root, "POST", path, `{"caId":`+strconv.FormatInt(created.CA.ID, 10)+`}`, true); c != 201 {
		t.Fatalf("assign: %d", c)
	}
	if c, _ := call(alice, "POST", "/api/v2/users/"+strconv.FormatInt(root, 10)+"/certificate", `{"caId":1}`, true); c != 403 {
		t.Fatalf("user assigning to admin: %d", c)
	}
	if c, data := call(alice, "GET", "/api/v2/me/certificate", "", false); c != 200 || bytes.Contains(data, []byte("PRIVATE")) {
		t.Fatalf("me/certificate: %d", c)
	}
	if c, data := call(alice, "POST", "/api/v2/me/certificate", "", true); c != 200 || !bytes.Contains(data, []byte("OPENSSH PRIVATE KEY")) {
		t.Fatalf("self rotate: %d", c)
	}

	if c, _ := call(root, "PUT", "/api/v2/users/"+strconv.FormatInt(alice, 10)+"/status", `{"status":2}`, true); c != 204 {
		t.Fatalf("disable: %d", c)
	}
	var n int
	_ = store.QueryRow(context.Background(), db, "SELECT COUNT(*) FROM user_certificates WHERE user_id = ?", []any{alice}, &n)
	if n != 0 {
		t.Fatal("certificate not revoked when user disabled")
	}
}
