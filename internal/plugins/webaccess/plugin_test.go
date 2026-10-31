package webaccess

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/app"
	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
	"github.com/TING-HiuYu/SlurmPortal/internal/config"
	"github.com/TING-HiuYu/SlurmPortal/internal/mail"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugin"
	"github.com/TING-HiuYu/SlurmPortal/internal/testutil"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v3"
)

func init() { plugin.Register(Plugin{}) }

var (
	fpSlave   = strings.Repeat("a", 64)
	fpGateway = strings.Repeat("c", 64)
	fpOther   = strings.Repeat("d", 64)
)

type harness struct {
	t            *testing.T
	app          *app.App
	user, admin  string
	ticketPublic ed25519.PublicKey
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	db := testutil.DB(t)
	uid := testutil.User(t, db, "alice", 1, "user")
	aid := testutil.User(t, db, "boss", 1, "admin")
	if _, err := db.Exec(context.Background(), "UPDATE users SET mail = 'alice@example.com' WHERE id = ?", uid); err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	var section yaml.Node
	if err := yaml.Unmarshal([]byte("gateways:\n- {fingerprint: "+fpGateway+", cluster-id: cluster1, gateway-id: cluster1-gw, instance-id: cluster1-gw-1}\n- {fingerprint: "+fpOther+", cluster-id: cluster2, gateway-id: cluster2-gw, instance-id: cluster2-gw-1}\n"), &section); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"PLUGIN_WEB_ACCESS_TICKET_KEY": base64.StdEncoding.EncodeToString(priv.Seed())}
	master := make([]byte, 32)
	_, _ = rand.Read(master)
	a, err := app.New(context.Background(), &config.Config{
		Role: "master", HTTP: config.HTTP{RatePerSecond: 1000, RateBurst: 1000},
		Auth:         config.Auth{Issuer: "i", Audience: "a", TokenTTL: time.Hour},
		Plugins:      config.Plugins{Enabled: []string{ID}, Config: map[string]yaml.Node{ID: *section.Content[0]}},
		Sync:         config.Sync{PeerFingerprints: []string{fpSlave, fpGateway, fpOther}},
		Clusters:     plugin.Clusters{{ID: "cluster1", Fingerprint: fpSlave}},
		PKIMasterKey: base64.StdEncoding.EncodeToString(master),
		JWTKeys:      authn.Keyring{ActiveKID: "k", Keys: map[string][]byte{"k": []byte("0123456789abcdef0123456789abcdef")}},
		Getenv:       func(k string) string { return env[k] },
	}, db)
	if err != nil {
		t.Fatal(err)
	}
	user, _, _, _ := a.JWT.Issue(uid, authn.SourcePassword)
	admin, _, _, _ := a.JWT.Issue(aid, authn.SourcePassword)
	return &harness{t, a, user, admin, pub}
}

// do sends body as JSON; token "" is anonymous and a 64-hex token is a bridged mTLS peer.
func (h *harness) do(method, path, token string, body any, out any) int {
	h.t.Helper()
	var rd *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if len(token) == 64 && !strings.Contains(token, ".") {
		req = req.WithContext(authn.WithBridgePeer(req.Context(), token))
	} else if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.app.Handler.ServeHTTP(rec, req)
	if out != nil {
		var env struct{ Data json.RawMessage }
		_ = json.Unmarshal(rec.Body.Bytes(), &env)
		_ = json.Unmarshal(env.Data, out)
	}
	return rec.Code
}

func settings(caID int64) map[string]any {
	return map[string]any{"clusters": []Cluster{
		{ID: "cluster1", Name: "Cluster 1", Enabled: true, Address: "cluster1.login.example.org", SSHHost: "127.0.0.1", SSHPort: 9933, SSHLocalPort: 22,
			SSHCAID: caID, GatewayID: "cluster1-gw", GatewayURL: "https://cluster1.login.example.org:19933/", AllowedRoles: []string{"user"}},
		{ID: "cluster2", Name: "Cluster 2", Enabled: true, Address: "cluster2.login.example.org", SSHHost: "cluster2.login.example.org", SSHPort: 7000,
			GatewayID: "cluster2-gw", GatewayURL: "https://cluster2.login.example.org:7000", AllowedRoles: []string{"admin"}},
	}}
}

// online registers both bound gateways with every feature, as running gateways do each minute.
func (h *harness) online() {
	h.t.Helper()
	for _, g := range []struct{ fp, id, cluster string }{{fpGateway, "cluster1-gw", "cluster1"}, {fpOther, "cluster2-gw", "cluster2"}} {
		reg := registration{Version: 1, GatewayID: g.id, InstanceID: g.id + "-1", BootID: "boot-1", ClusterID: g.cluster,
			Plugins: []string{"web-terminal", "web-files", "file-transfer"}, UserCAKeys: []string{}}
		if code := h.do("POST", RegisterPath, g.fp, reg, nil); code != 200 {
			h.t.Fatalf("register %s: %d", g.id, code)
		}
	}
}

func clustersOf(cfg map[string]any) []Cluster { return cfg["clusters"].([]Cluster) }

func TestConfigAndClusters(t *testing.T) {
	h := newHarness(t)
	if code := h.do("PUT", "/api/v2/web-access/config", h.user, settings(0), nil); code != http.StatusForbidden {
		t.Fatalf("user edited config: %d", code)
	}
	// Bound clusters exist before any configuration, but are not offered until set up and registered.
	var admin struct{ Clusters []AdminCluster }
	if code := h.do("GET", "/api/v2/web-access/config", h.admin, nil, &admin); code != 200 || len(admin.Clusters) != 2 || admin.Clusters[0].GatewayID != "cluster1-gw" ||
		admin.Clusters[0].Ready || admin.Clusters[0].Registration != nil {
		t.Fatalf("default clusters %d %+v", code, admin)
	}
	bad := settings(0)
	clustersOf(bad)[0].GatewayURL = "http://cluster1.example.org"
	if code := h.do("PUT", "/api/v2/web-access/config", h.admin, bad, nil); code != http.StatusBadRequest {
		t.Fatalf("plain HTTP gateway accepted: %d", code)
	}
	bad = settings(0)
	clustersOf(bad)[1].ID = "nowhere"
	if code := h.do("PUT", "/api/v2/web-access/config", h.admin, bad, nil); code != http.StatusBadRequest {
		t.Fatalf("unbound cluster accepted: %d", code)
	}
	if code := h.do("PUT", "/api/v2/web-access/config", h.admin, settings(0), &admin); code != 200 || admin.Clusters[0].GatewayURL != "https://cluster1.login.example.org:19933" {
		t.Fatalf("save %d %+v", code, admin)
	}
	var view struct {
		Capabilities Capabilities
		Clusters     []publicCluster
	}
	if code := h.do("GET", "/api/v2/web-access/clusters", h.user, nil, &view); code != 200 || view.Capabilities.Enabled || len(view.Clusters) != 0 {
		t.Fatalf("unregistered cluster offered %d %+v", code, view)
	}
	h.online()
	if code := h.do("GET", "/api/v2/web-access/clusters", h.user, nil, &view); code != 200 || !view.Capabilities.TerminalEnabled || view.Capabilities.TransferEnabled ||
		len(view.Clusters) != 1 || view.Clusters[0].ID != "cluster1" || !view.Clusters[0].Features.Files {
		t.Fatalf("user clusters %d %+v", code, view)
	}
	// cluster2 is admin-only; a single reachable cluster has nowhere to transfer to.
	if code := h.do("GET", "/api/v2/web-access/clusters", h.admin, nil, &view); code != 200 || view.Capabilities.TransferEnabled || len(view.Clusters) != 1 || view.Clusters[0].ID != "cluster2" {
		t.Fatalf("admin clusters %d %+v", code, view)
	}
	if code := h.do("GET", "/api/v2/web-access/config", h.admin, nil, &admin); code != 200 || !admin.Clusters[0].Ready || !admin.Clusters[0].Features.Transfer {
		t.Fatalf("registered admin view %d %+v", code, admin)
	}
	var keys struct{ Keys []jwk }
	if code := h.do("GET", "/api/v2/web-access/keys", h.admin, nil, &keys); code != 200 || keys.Keys[0].X != base64.RawURLEncoding.EncodeToString(h.ticketPublic) {
		t.Fatalf("keys %d %+v", code, keys)
	}
}

func TestTicket(t *testing.T) {
	h := newHarness(t)
	h.do("PUT", "/api/v2/web-access/config", h.admin, settings(0), nil)
	h.online()
	var out struct {
		Ticket     string
		GatewayURL string
	}
	if code := h.do("POST", "/api/v2/web-access/ticket", h.user, ticketRequest{ClusterID: "cluster1", Scopes: []string{"terminal", "session:create", "terminal"}}, &out); code != 200 {
		t.Fatalf("ticket %d", code)
	}
	var claims ticketClaims
	tok, err := jwt.ParseWithClaims(out.Ticket, &claims, func(tk *jwt.Token) (any, error) { return h.ticketPublic, nil },
		jwt.WithValidMethods([]string{"EdDSA"}), jwt.WithIssuer("go-ldap-admin"), jwt.WithAudience("cluster1-gw"), jwt.WithExpirationRequired())
	if err != nil || !tok.Valid || tok.Header["kid"] != "web-access-v1" || claims.Username != "alice" || claims.ClusterID != "cluster1" ||
		strings.Join(claims.Scopes, ",") != "session:create,terminal" || strings.Join(claims.DestinationClusterIDs, ",") != "cluster1" ||
		claims.ExpiresAt.Sub(claims.IssuedAt.Time) != ticketTTL {
		t.Fatalf("claims %v %+v", err, claims)
	}
	for name, req := range map[string]ticketRequest{
		"other role's cluster": {ClusterID: "cluster2", Scopes: []string{"terminal"}},
		"diagnostics":          {ClusterID: "cluster1", Scopes: []string{"diagnostics"}},
	} {
		if code := h.do("POST", "/api/v2/web-access/ticket", h.user, req, nil); code != http.StatusForbidden {
			t.Errorf("%s: %d", name, code)
		}
	}
	if code := h.do("POST", "/api/v2/web-access/ticket", h.user, ticketRequest{ClusterID: "cluster1", Scopes: []string{"shell"}}, nil); code != http.StatusBadRequest {
		t.Errorf("unknown scope: %d", code)
	}
	if code := h.do("POST", "/api/v2/web-access/ticket", h.admin, ticketRequest{ClusterID: "cluster2", Scopes: []string{"diagnostics"}}, nil); code != 200 {
		t.Errorf("admin diagnostics ticket: %d", code)
	}
}

func browserKey(t *testing.T) string {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	k, _ := ssh.NewPublicKey(pub)
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k)))
}

func TestSSHCertificate(t *testing.T) {
	h := newHarness(t)
	ca, _, err := h.app.PKI.GenerateSSH(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	h.do("PUT", "/api/v2/web-access/config", h.admin, settings(0), nil)
	req := certRequest{ClusterID: "cluster1", Purpose: "access", PublicKey: browserKey(t)}
	if code := h.do("POST", "/api/v2/web-access/ssh-certificate", h.user, req, nil); code != http.StatusForbidden {
		t.Fatalf("signed for an unregistered gateway: %d", code)
	}
	h.online()
	if code := h.do("POST", "/api/v2/web-access/ssh-certificate", h.user, req, nil); code != http.StatusServiceUnavailable {
		t.Fatalf("signed without a trusted CA: %d", code)
	}
	reg := registration{Version: 1, GatewayID: "cluster1-gw", InstanceID: "cluster1-gw-1", BootID: "boot-1", ClusterID: "cluster1", Plugins: []string{"web-terminal"},
		UserCAKeys: []string{strings.TrimSpace(ca.CertificatePEM) + " comment"}}
	var regOut struct {
		Clusters         []gatewayCluster `json:"clusters"`
		VerificationKeys []jwk            `json:"verification_keys"`
	}
	if code := h.do("POST", RegisterPath, fpGateway, reg, &regOut); code != 200 || len(regOut.Clusters) != 2 || regOut.Clusters[0].SSHLocalPort != 22 || len(regOut.VerificationKeys) != 1 {
		t.Fatalf("register %d %+v", code, regOut)
	}
	var cert struct {
		Certificate string
		CAID        int64 `json:"caId"`
	}
	if code := h.do("POST", "/api/v2/web-access/ssh-certificate", h.user, req, &cert); code != 200 || cert.CAID != ca.ID {
		t.Fatalf("certificate %d %+v", code, cert)
	}
	parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(cert.Certificate))
	c, ok := parsed.(*ssh.Certificate)
	if err != nil || !ok || c.ValidPrincipals[0] != "alice" || !strings.HasPrefix(c.KeyId, "web-access/cluster1/access/user:") ||
		!strings.HasSuffix(c.KeyId, fmt.Sprintf("/%x", c.Serial)) || c.ValidBefore-c.ValidAfter != 300 {
		t.Fatalf("certificate fields %v %+v", err, c)
	}
	checker := ssh.CertChecker{IsUserAuthority: func(k ssh.PublicKey) bool { return string(k.Marshal()) == string(c.SignatureKey.Marshal()) }}
	if err := checker.CheckCert("alice", c); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]certRequest{
		"rsa key":       {ClusterID: "cluster1", Purpose: "access", PublicKey: "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQ"},
		"other purpose": {ClusterID: "cluster1", Purpose: "terminal", PublicKey: req.PublicKey},
	} {
		if code := h.do("POST", "/api/v2/web-access/ssh-certificate", h.user, bad, nil); code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, code)
		}
	}
	if code := h.do("POST", "/api/v2/web-access/ssh-certificate", h.user, certRequest{ClusterID: "cluster2", Purpose: "access", PublicKey: req.PublicKey}, nil); code != http.StatusForbidden {
		t.Errorf("other cluster: %d", code)
	}
}

func TestRegisterIdentity(t *testing.T) {
	h := newHarness(t)
	h.do("PUT", "/api/v2/web-access/config", h.admin, settings(0), nil)
	reg := registration{Version: 1, GatewayID: "cluster1-gw", InstanceID: "cluster1-gw-1", BootID: "boot-1", ClusterID: "cluster1", Plugins: []string{}, UserCAKeys: []string{}}
	if code := h.do("POST", RegisterPath, fpSlave, reg, nil); code != http.StatusForbidden {
		t.Errorf("slave certificate registered as gateway: %d", code)
	}
	if code := h.do("POST", RegisterPath, h.admin, reg, nil); code != http.StatusUnauthorized {
		t.Errorf("user token registered: %d", code)
	}
	reg.ClusterID = "cluster2"
	if code := h.do("POST", RegisterPath, fpGateway, reg, nil); code != http.StatusBadRequest {
		t.Errorf("claimed another cluster: %d", code)
	}
	cfg := settings(0)
	clustersOf(cfg)[0].Enabled = false
	h.do("PUT", "/api/v2/web-access/config", h.admin, cfg, nil)
	reg.ClusterID = "cluster1"
	if code := h.do("POST", RegisterPath, fpGateway, reg, nil); code != http.StatusForbidden {
		t.Errorf("disabled cluster registered: %d", code)
	}
}

func transferEvent() map[string]any {
	return map[string]any{"version": 1, "event_id": "6f1c1c55-2a8e-4c8a-9d9e-0d3f4a1b2c3d", "transfer_id": "6f1c1c55-2a8e-4c8a-9d9e-0d3f4a1b2c3d",
		"gateway_id": "cluster1-gw", "cluster_id": "cluster1", "user_id": 1, "username": "alice", "destination_cluster_id": "cluster2",
		"source_paths": []string{"/home/alice/data"}, "destination_path": "/home/alice", "status": "completed", "exit_code": 0,
		"started_at": "2026-10-30T01:00:00Z", "finished_at": "2026-10-30T01:05:00.5Z", "bytes_sent": 2e9, "bytes_received": 1000,
		"total_bytes": 2e9, "elapsed_seconds": 300.5, "average_mb_per_second": 6.65}
}

func TestTransferEvents(t *testing.T) {
	h := newHarness(t)
	h.do("PUT", "/api/v2/web-access/config", h.admin, settings(0), nil)
	var out eventOutcome
	if code := h.do("POST", EventsPath, fpGateway, transferEvent(), &out); code != 200 || out.Outcome != "skipped" || out.Reason != "transfer_email_disabled" || out.Duplicate {
		t.Fatalf("event %d %+v", code, out)
	}
	if code := h.do("POST", EventsPath, fpGateway, transferEvent(), &out); code != 200 || !out.Duplicate {
		t.Fatalf("duplicate %d %+v", code, out)
	}
	changed := transferEvent()
	changed["exit_code"], changed["status"] = 23, "partial"
	if code := h.do("POST", EventsPath, fpGateway, changed, nil); code != http.StatusConflict {
		t.Fatalf("rewritten event: %d", code)
	}
	for name, mutate := range map[string]func(map[string]any){
		"other gateway":     func(e map[string]any) { e["gateway_id"] = "cluster2-gw" },
		"status mismatch":   func(e map[string]any) { e["exit_code"] = 1 },
		"relative path":     func(e map[string]any) { e["destination_path"] = "home" },
		"root":              func(e map[string]any) { e["username"] = "root" },
		"future":            func(e map[string]any) { e["finished_at"] = "2099-01-01T00:00:00Z" },
		"finished<started":  func(e map[string]any) { e["started_at"] = "2026-10-30T02:00:00Z" },
		"unknown field":     func(e map[string]any) { e["callback"] = "x" },
		"event id mismatch": func(e map[string]any) { e["event_id"] = "00000000-0000-4000-8000-000000000000" },
	} {
		e := transferEvent()
		e["transfer_id"], e["event_id"] = "00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000001"
		mutate(e)
		if code := h.do("POST", EventsPath, fpGateway, e, nil); code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, code)
		}
	}
	if code := h.do("POST", EventsPath, fpSlave, transferEvent(), nil); code != http.StatusForbidden {
		t.Errorf("slave posted transfer event: %d", code)
	}
	// The finished transfer is kept as the template sample even though mail is off.
	var sample struct {
		Source  string
		Context map[string]any
	}
	if code := h.do("GET", "/api/v2/web-access/transfer-notification/sample", h.admin, nil, &sample); code != 200 || sample.Source != "latest" ||
		sample.Context["transfer"].(map[string]any)["size"] != "2.00 GB" || sample.Context["user"].(map[string]any)["username"] != "alice" {
		t.Fatalf("sample %d %+v", code, sample)
	}
}

func TestTransferNotificationSettings(t *testing.T) {
	h := newHarness(t)
	var te TransferEmail
	if code := h.do("GET", "/api/v2/web-access/transfer-notification", h.admin, nil, &te); code != 200 || te.Enabled || te.TimeZone != "Asia/Shanghai" ||
		!strings.Contains(te.Template.Subject, "{{ transfer.state_text }}") {
		t.Fatalf("defaults %d %+v", code, te)
	}
	var sample struct{ Source string }
	if code := h.do("GET", "/api/v2/web-access/transfer-notification/sample", h.admin, nil, &sample); code != 200 || sample.Source != "example" {
		t.Fatalf("example sample %d %+v", code, sample)
	}
	te.Enabled = true
	if code := h.do("PUT", "/api/v2/web-access/transfer-notification", h.admin, te, nil); code != http.StatusBadRequest {
		t.Fatalf("enabled without a mail provider: %d", code)
	}
	te.Enabled = false
	te.Template.HTML = "<p>{{ job.name }}</p>"
	if code := h.do("PUT", "/api/v2/web-access/transfer-notification", h.admin, te, nil); code != http.StatusBadRequest {
		t.Fatalf("foreign placeholder accepted: %d", code)
	}
	te.Template.HTML = "<p>{{ transfer.source_paths }} {{ constants.team }}</p>"
	te.Template.Constants = map[string]any{"team": "HPC"}
	if code := h.do("PUT", "/api/v2/web-access/transfer-notification", h.user, te, nil); code != http.StatusForbidden {
		t.Fatalf("user edited transfer mail: %d", code)
	}
	if code := h.do("PUT", "/api/v2/web-access/transfer-notification", h.admin, te, nil); code != 200 {
		t.Fatalf("save %d", code)
	}
	var out struct{ HTML, Subject string }
	if code := h.do("POST", "/api/v2/web-access/transfer-notification/preview", h.admin, map[string]any{}, &out); code != 200 ||
		out.HTML != "<p>/HOME/alice/data/run-42, /HOME/alice/data/notes.txt HPC</p>" {
		t.Fatalf("preview %d %+v", code, out)
	}
}

func TestTransferTemplateEscapes(t *testing.T) {
	var ev TransferEvent
	b, _ := json.Marshal(transferEvent())
	_ = json.Unmarshal(b, &ev)
	ev.SourcePaths = []string{"/home/alice/<script>"}
	data := transferContext(ev, mailUser{Username: "alice"}, Cluster{ID: "cluster1", Name: "Cluster 1"}, Cluster{ID: "cluster2", Name: "HK"}, "UTC")
	data["constants"] = map[string]any{}
	out, err := mail.Render(defaultTransferEmail().Template, transferRoots, data, nil)
	if err != nil || out.Subject != "[Slurm Notification] 文件传输完成：Cluster 1 → HK" || !strings.Contains(out.HTML, "2.00 GB") || !strings.Contains(out.HTML, "00:05:00") ||
		!strings.Contains(out.HTML, "6.65 MB/s") || strings.Contains(out.HTML, "<script>") || !strings.Contains(out.HTML, "&lt;script&gt;") ||
		!strings.Contains(out.HTML, "2026-10-30 01:05:00 (UTC)") {
		t.Fatalf("%v %q\n%s", err, out.Subject, out.HTML)
	}
}
