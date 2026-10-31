package replication_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/TING-HiuYu/SlurmPortal/internal/replication"
	"github.com/TING-HiuYu/SlurmPortal/internal/testutil"
)

func TestLoadContents(t *testing.T) {
	ctx := context.Background()
	db := testutil.DB(t)
	alice := testutil.User(t, db, "alice", 1)
	testutil.User(t, db, "gone", 2)
	for _, q := range []string{
		"INSERT INTO groups(id, group_name, group_type, group_class, gid_number, group_dn) VALUES (5, 'zeta', 'cn', 'groupOfUniqueNames', 0, 'cn=zeta,dc=test')",
		"INSERT INTO groups(id, group_name, group_type, group_class, gid_number, group_dn) VALUES (6, 'cluster1', 'cn', 'posixGroup', 10000, 'cn=cluster1,dc=test')",
		"INSERT INTO group_users(group_id, user_id) VALUES (5, 1), (6, 1), (6, 2)",
		"INSERT INTO v2_user_credentials(user_id, app_hash, ldap_hash, updated_at) VALUES (1, '$argon2id$x', '{CRYPT}$6$x', 'now')",
		"INSERT INTO certificate_authorities(id, name, kind, certificate_pem, crl_number, enabled) VALUES (1, 'x', 'x509', 'X', 1, 1), (2, 's', 'ssh', 'S', 0, 'true')",
		"INSERT INTO revoked_certificates(certificate_authority_id, serial_hex, revoked_at, reason) VALUES (1, 'aa', 'now', 0), (2, 'bb', 'now', 0)",
		"INSERT INTO user_certificates(user_id, certificate_authority_id, serial_hex, certificate_pem) VALUES (1, 1, 'cc', 'P'), (2, 2, 'dd', 'Q')",
	} {
		if _, err := db.Exec(ctx, q); err != nil {
			t.Fatal(q, err)
		}
	}
	s, h1, err := replication.Load(ctx, db, "dc=test")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Users) != 1 || s.Users[0].ID != alice || s.Users[0].PasswordHash != "{CRYPT}$6$x" || s.Users[0].Departments != "cluster1,zeta" {
		t.Fatalf("users = %+v", s.Users)
	}
	if len(s.Memberships) != 2 {
		t.Fatalf("disabled user's membership leaked: %+v", s.Memberships)
	}
	if len(s.CAs) != 2 || !s.CAs[1].Enabled || len(s.SSHRevocations) != 1 || s.SSHRevocations[0].SerialHex != "bb" {
		t.Fatalf("CA material = %+v %+v", s.CAs, s.SSHRevocations)
	}
	if len(s.UserCertificates) != 1 || s.UserCertificates[0].SerialHex != "cc" {
		t.Fatalf("user certificates = %+v", s.UserCertificates)
	}
	raw, _ := json.Marshal(s)
	if strings.Contains(string(raw), "argon2") || strings.Contains(string(raw), "private") {
		t.Fatal("snapshot leaks app hashes or private material")
	}
	_, h2, _ := replication.Load(ctx, db, "dc=test")
	if h1 != h2 {
		t.Fatal("hash not stable")
	}
	_, _ = db.Exec(ctx, "UPDATE users SET login_shell = '/bin/zsh' WHERE id = 1")
	if _, h3, _ := replication.Load(ctx, db, "dc=test"); h3 == h1 {
		t.Fatal("hash did not change with data")
	}
}

type fakeCF struct {
	mu      sync.Mutex
	records map[string]string
	calls   []string
	next    int
}

func (f *fakeCF) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method)
	if r.Header.Get("Authorization") != "Bearer tok" {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"success":false}`))
		return
	}
	var body struct {
		Content string `json:"content"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	switch {
	case r.Method == http.MethodGet:
		var out []map[string]string
		for id := range f.records {
			out = append(out, map[string]string{"id": id})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": out})
	case r.Method == http.MethodPost:
		f.next++
		id := "rec" + string(rune('0'+f.next))
		f.records[id] = body.Content
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]string{"id": id}})
	case r.Method == http.MethodPatch:
		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		f.records[id] = body.Content
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]string{"id": id}})
	}
}

func TestDNSPublisher(t *testing.T) {
	ctx := context.Background()
	db := testutil.DB(t)
	testutil.User(t, db, "alice", 1)
	cf := &fakeCF{records: map[string]string{}}
	srv := httptest.NewServer(cf)
	defer srv.Close()
	p := &replication.DNSPublisher{DB: db, BaseDN: "dc=test", Name: "_s.test", ZoneID: "z", Token: "tok", APIBase: srv.URL}
	h, err := p.Publish(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(cf.records) != 1 || cf.records["rec1"] != `"v2:`+h+`"` {
		t.Fatalf("records = %v", cf.records)
	}
	n := len(cf.calls)
	if _, err := p.Publish(ctx); err != nil || len(cf.calls) != n {
		t.Fatalf("unchanged hash re-published (calls %v)", cf.calls[n:])
	}
	other := &replication.DNSPublisher{DB: db, BaseDN: "dc=test", Name: "_s.test", ZoneID: "z", Token: "tok", APIBase: srv.URL}
	if _, err := other.Publish(ctx); err != nil || len(cf.calls) != n {
		t.Fatal("second instance did not reuse the shared published hash")
	}
	_, _ = db.Exec(ctx, "UPDATE users SET mail = 'a@b.c'")
	h2, err := p.Publish(ctx)
	if err != nil || h2 == h || cf.records["rec1"] != `"v2:`+h2+`"` || len(cf.records) != 1 {
		t.Fatalf("update failed: %v %v", err, cf.records)
	}
	bad := &replication.DNSPublisher{DB: db, BaseDN: "dc=test", Name: "_s.test", ZoneID: "z", Token: "wrong", APIBase: srv.URL}
	_, _ = db.Exec(ctx, "UPDATE users SET mail = 'x@b.c'")
	if _, err := bad.Publish(ctx); err == nil {
		t.Fatal("rejected token reported success")
	}
}
