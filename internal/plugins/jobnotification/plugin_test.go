package jobnotification_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/app"
	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
	"github.com/TING-HiuYu/SlurmPortal/internal/config"
	"github.com/TING-HiuYu/SlurmPortal/internal/lifecycle/jobnotify"
	"github.com/TING-HiuYu/SlurmPortal/internal/mail"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugin"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
	"github.com/TING-HiuYu/SlurmPortal/internal/testutil"

	. "github.com/TING-HiuYu/SlurmPortal/internal/plugins/jobnotification"
)

func init() { plugin.Register(Plugin{}) }

var (
	fp      = strings.Repeat("a", 64)
	unbound = strings.Repeat("b", 64)
)

type sent struct {
	to  []string
	msg string
	pw  string
}

type fakeMailer struct {
	mu   sync.Mutex
	sent []sent
	err  error
}

func (f *fakeMailer) Send(_ context.Context, _ mail.SMTPConfig, pw, _ string, to []string, msg []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, sent{to: to, msg: string(msg), pw: pw})
	return nil
}

func (f *fakeMailer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func completion(t *testing.T, sluid string, uid uint32, active int64) string {
	t.Helper()
	part := "gpu"
	ev := jobnotify.Event{Version: 1, EventID: "cluster1/" + sluid + "/ended", ClusterID: "cluster1",
		Job:      jobnotify.Job{JobID: 42, SLUID: sluid, UID: uid, Name: "train<1>", Partition: &part},
		Result:   jobnotify.Result{State: "COMPLETED", ExitCode: json.RawMessage(`{"status":0,"signal":0}`)},
		Duration: jobnotify.Duration{ActiveSeconds: active, Scope: "last_attempt", ExcludesSuspend: true},
		Raw:      jobnotify.Raw{Controller: json.RawMessage(`{"job_id":42}`), Complete: true},
		Usage: map[string]any{"gpu": map[string]any{"allocated_gpu_seconds": 7200, "observed_gpu_seconds": 7200,
			"utilization_percent_seconds": 360000, "memory_utilization_percent_seconds": 144000,
			"complete": true, "missing_reasons": []string{}}},
		ObservedAt: time.Now().UTC(),
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestJobNotification(t *testing.T) {
	db := testutil.DB(t)
	ctx := context.Background()
	alice := testutil.User(t, db, "alice", 1, "user")
	bob := testutil.User(t, db, "bob", 0, "user")
	admin := testutil.User(t, db, "admin", 1, "admin")
	for id, v := range map[int64][2]any{alice: {10001, "alice@example.com"}, bob: {10002, "bob@example.com"}, admin: {0, "admin@example.com"}} {
		if _, err := db.Exec(ctx, "UPDATE users SET uid_number = ?, mail = ?, nickname = ? WHERE id = ?", v[0], v[1], "Nick", id); err != nil {
			t.Fatal(err)
		}
	}
	mailer := &fakeMailer{}
	defer mail.SetTransport(mailer)()
	a, err := app.New(ctx, &config.Config{
		Role: "master", HTTP: config.HTTP{RatePerSecond: 1000, RateBurst: 1000},
		Auth: config.Auth{Issuer: "i", Audience: "a", TokenTTL: time.Hour}, Plugins: config.Plugins{Enabled: []string{ID}},
		Sync: config.Sync{PeerFingerprints: []string{fp, unbound}}, Clusters: plugin.Clusters{{ID: "cluster1", Name: "Cluster 1", Fingerprint: fp}},
		JWTKeys:      authn.Keyring{ActiveKID: "k", Keys: map[string][]byte{"k": []byte("0123456789abcdef0123456789abcdef")}},
		PKIMasterKey: base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
	}, db)
	if err != nil {
		t.Fatal(err)
	}
	token := func(id int64) string {
		tok, _, _, err := a.JWT.Issue(id, authn.SourcePassword)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	do := func(method, path, peer string, user int64, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if peer != "" {
			req = req.WithContext(authn.WithBridgePeer(req.Context(), peer))
		}
		if user != 0 {
			req.Header.Set("Authorization", "Bearer "+token(user))
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		a.Handler.ServeHTTP(rec, req)
		return rec
	}
	ingest := func(body string) *httptest.ResponseRecorder { return do("POST", CompletionPath, fp, 0, body) }
	expect := func(name string, rec *httptest.ResponseRecorder, code int, body string) {
		t.Helper()
		if rec.Code != code || (body != "" && strings.TrimSpace(rec.Body.String()) != body) {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}

	expect("disabled", ingest(completion(t, "s0123456789AB0", 10001, 3600)), 200, `{"outcome":"skipped","reason":"disabled"}`)

	expect("user config get", do("GET", "/api/v2/notification/config", "", alice, ""), 403, "")
	rec := do("GET", "/api/v2/notification/config", "", admin, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"provider":""`) || !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Fatalf("default config %d %s", rec.Code, rec.Body.String())
	}

	tmpl := `{"subject":"[{{ cluster.display_name }}] 作业 {{ job.job_id }} {{ result.state }}","html":"<p>{{ user.nickname }} {{ job.name }} {{ job.duration }} {{ constants.help }} {{ usage.gpu.mean_utilization_percent }}</p>","constants":{"help":"ops@example.com"}}`
	cfg := func(enabled bool, extra, template string) string {
		return `{"enabled":` + map[bool]string{true: "true", false: "false"}[enabled] + extra + `,"template":` + template + `}`
	}
	const secret = "s3cr3t-Pa55word"
	expect("user put", do("PUT", "/api/v2/notification/config", "", alice, cfg(true, "", tmpl)), 403, "")
	expect("enable without provider", do("PUT", "/api/v2/notification/config", "", admin, cfg(true, "", tmpl)), 400, "")
	expect("unknown provider", do("PUT", "/api/v2/notification/config", "", admin, cfg(false, `,"provider":"nope"`, tmpl)), 400, "")
	for name, body := range map[string]string{
		"bad root":     cfg(false, "", `{"subject":"x","html":"{{ env.SECRET }}"}`),
		"bad syntax":   cfg(false, "", `{"subject":"x","html":"{{#if}}"}`),
		"empty html":   cfg(false, "", `{"subject":"x","html":"  "}`),
		"subject crlf": cfg(false, "", `{"subject":"a\r\nBcc: x@example.com","html":"x"}`),
		"proto":        cfg(false, "", `{"subject":"x","html":"{{ job.__proto__ }}"}`),
		"bad const":    cfg(false, "", `{"subject":"x","html":"x","constants":{"a":{}}}`),
		"bad tz":       cfg(false, `,"timeZone":"Mars/Base"`, tmpl),
		"old smtp":     cfg(false, `,"smtp":{"host":"smtp.example.com"}`, tmpl),
	} {
		expect(name, do("PUT", "/api/v2/notification/config", "", admin, body), 400, "")
	}

	// The SMTP account is a mail hub provider now.
	provider := func(enabled bool, extra string) string {
		return `{"kind":"smtp","enabled":` + map[bool]string{true: "true", false: "false"}[enabled] + `,"default":true,"config":{"host":"SMTP.Example.com","port":465,"security":"tls","username":"bot","fromAddress":"bot@example.com","fromName":"HPC 通知","timeoutMs":5000}` + extra + `}`
	}
	expect("provider user", do("PUT", "/api/v2/mail/providers/default", "", alice, provider(true, `,"secret":"`+secret+`"`)), 403, "")
	expect("provider without password", do("PUT", "/api/v2/mail/providers/default", "", admin, provider(true, "")), 400, "")
	for name, body := range map[string]string{
		"port 25":      strings.Replace(provider(false, ""), `"port":465`, `"port":25`, 1),
		"tls on 587":   strings.Replace(provider(false, ""), `"port":465`, `"port":587`, 1),
		"timeout":      strings.Replace(provider(false, ""), `"timeoutMs":5000`, `"timeoutMs":999`, 1),
		"ip host":      strings.Replace(provider(false, ""), `SMTP.Example.com`, `127.0.0.1`, 1),
		"short ip":     strings.Replace(provider(false, ""), `SMTP.Example.com`, `mail.0x7f`, 1),
		"local host":   strings.Replace(provider(false, ""), `SMTP.Example.com`, `smtp.corp.internal`, 1),
		"bad sender":   strings.Replace(provider(false, ""), `bot@example.com`, `bot@@example.com`, 1),
		"pw and clear": provider(false, `,"secret":"x","clearSecret":true`),
		"unknown":      strings.Replace(provider(false, ""), `"timeoutMs":5000`, `"timeoutMs":5000,"bogus":1`, 1),
		"bad kind":     strings.Replace(provider(false, ""), `"kind":"smtp"`, `"kind":"carrier-pigeon"`, 1),
	} {
		expect(name, do("PUT", "/api/v2/mail/providers/default", "", admin, body), 400, "")
	}
	expect("bad name", do("PUT", "/api/v2/mail/providers/Bad%20Name", "", admin, provider(false, "")), 400, "")
	rec = do("PUT", "/api/v2/mail/providers/default", "", admin, provider(true, `,"secret":"`+secret+`"`))
	if rec.Code != 200 || strings.Contains(rec.Body.String(), secret) || !strings.Contains(rec.Body.String(), `"secretSet":true`) ||
		!strings.Contains(rec.Body.String(), `"host":"smtp.example.com"`) {
		t.Fatalf("provider put %d %s", rec.Code, rec.Body.String())
	}
	var sealed, raw string
	if err := store.QueryRow(ctx, db, "SELECT secret_sealed, config_json FROM v2_mail_providers WHERE name = 'default'", nil, &sealed, &raw); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sealed, "sb1:") || strings.Contains(sealed, secret) || strings.Contains(raw, secret) {
		t.Fatalf("password stored in clear: %q %q", sealed, raw)
	}
	// Omitted secret keeps the stored one.
	rec = do("PUT", "/api/v2/mail/providers/default", "", admin, provider(true, `,"secretSet":true`))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"secretSet":true`) {
		t.Fatalf("keep password %d %s", rec.Code, rec.Body.String())
	}
	rec = do("GET", "/api/v2/mail/providers", "", admin, "")
	if rec.Code != 200 || strings.Contains(rec.Body.String(), secret) || strings.Contains(rec.Body.String(), "sb1:") {
		t.Fatalf("list leaks password %s", rec.Body.String())
	}
	expect("provider list user", do("GET", "/api/v2/mail/providers", "", alice, ""), 403, "")

	rec = do("PUT", "/api/v2/notification/config", "", admin, cfg(true, `,"timeZone":"Asia/Hong_Kong"`, tmpl))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"timeZone":"Asia/Hong_Kong"`) {
		t.Fatalf("put %d %s", rec.Code, rec.Body.String())
	}
	// Omitted timeZone keeps the stored one.
	rec = do("PUT", "/api/v2/notification/config", "", admin, cfg(true, "", tmpl))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"timeZone":"Asia/Hong_Kong"`) {
		t.Fatalf("keep zone %d %s", rec.Code, rec.Body.String())
	}

	good := completion(t, "s0123456789ABC", 10001, 3600)
	expect("send", ingest(good), 200, `{"outcome":"smtp_accepted"}`)
	if mailer.count() != 1 {
		t.Fatalf("sent %d", mailer.count())
	}
	m := mailer.sent[0]
	if len(m.to) != 1 || m.to[0] != "alice@example.com" || m.pw != secret || !strings.Contains(m.msg, "To: <alice@example.com>") ||
		!strings.Contains(m.msg, "Subject: =?UTF-8?B?") || !strings.Contains(m.msg, "From: =?UTF-8?B?") {
		t.Fatalf("message %+v", m)
	}
	var html string
	for _, part := range strings.Split(m.msg, "--go-ldap-admin-") {
		if strings.Contains(part, "text/html") {
			b, _ := base64.StdEncoding.DecodeString(strings.ReplaceAll(strings.SplitN(part, "\r\n\r\n", 2)[1], "\r\n", ""))
			html = string(b)
		}
	}
	if html != "<p>Nick train&lt;1&gt; 1:00:00 ops@example.com 50</p>" {
		t.Fatalf("html %q", html)
	}
	var sample struct {
		Data struct {
			Source    string
			Context   map[string]any
			Variables []struct{ Name string }
		}
	}
	rec = do("GET", "/api/v2/notification/sample", "", admin, "")
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &sample) != nil || sample.Data.Source != "latest" || len(sample.Data.Variables) == 0 ||
		sample.Data.Context["user"].(map[string]any)["username"] != "alice" || sample.Data.Context["constants"].(map[string]any)["help"] != "ops@example.com" {
		t.Fatalf("sample %d %s", rec.Code, rec.Body.String())
	}
	expect("sample user", do("GET", "/api/v2/notification/sample", "", alice, ""), 403, "")
	expect("duplicate", ingest(good), 200, `{"outcome":"smtp_accepted","duplicate":true}`)
	if mailer.count() != 1 {
		t.Fatal("duplicate re-sent")
	}
	expect("identity conflict", ingest(strings.Replace(good, `"uid":10001`, `"uid":10002`, 1)), 409, "")
	expect("threshold", ingest(completion(t, "s0123456789ABD", 10001, 60)), 200, `{"outcome":"skipped","reason":"below_threshold"}`)
	expect("unmanaged", ingest(completion(t, "s0123456789ABE", 0, 3600)), 200, `{"outcome":"skipped","reason":"unmanaged_uid"}`)
	expect("disabled user", ingest(completion(t, "s0123456789ABF", 10002, 3600)), 200, `{"outcome":"skipped","reason":"user_disabled"}`)
	expect("missing user", ingest(completion(t, "s0123456789ABG", 10009, 3600)), 200, `{"outcome":"skipped","reason":"user_missing"}`)
	expect("skip duplicate", ingest(completion(t, "s0123456789ABF", 10002, 3600)), 200, `{"outcome":"skipped","reason":"user_disabled","duplicate":true}`)

	mailer.err = errors.New("boom")
	retry := completion(t, "s0123456789ABH", 10001, 3600)
	expect("transient failure", ingest(retry), 503, "")
	mailer.err = &mail.Error{Kind: "smtp_recipient_rejected", Permanent: true}
	expect("rejected", ingest(completion(t, "s0123456789ABJ", 10001, 3600)), 200, `{"outcome":"skipped","reason":"smtp_recipient_rejected"}`)
	mailer.err = nil
	expect("retry after failure", ingest(retry), 200, `{"outcome":"smtp_accepted"}`)
	var status string
	var attempts int
	if err := store.QueryRow(ctx, db, "SELECT status, attempts FROM v2_job_notifications WHERE cluster_id = 'cluster1' AND event_id = 'cluster1/s0123456789ABH/ended'", nil, &status, &attempts); err != nil || status != "sent" || attempts != 2 {
		t.Fatalf("record %v %s %d", err, status, attempts)
	}

	expect("unbound peer", do("POST", CompletionPath, unbound, 0, good), 403, "")
	expect("user token", do("POST", CompletionPath, "", alice, good), 401, "")
	for name, bad := range map[string]string{
		"other cluster": strings.ReplaceAll(good, `cluster1`, `cluster2`),
		"event id":      strings.Replace(good, `"event_id":"cluster1/s0123456789ABC/ended"`, `"event_id":"cluster1/x"`, 1),
		"state":         strings.Replace(good, `"COMPLETED"`, `"RUNNING"`, 1),
		"scope":         strings.Replace(good, `"last_attempt"`, `"all"`, 1),
		"unknown field": strings.Replace(good, `"version":1`, `"version":1,"x":1`, 1),
		"unknown job":   strings.Replace(good, `"job_id":42`, `"job_id":42,"evil":1`, 1),
		"time":          strings.Replace(good, `"job_id":42`, `"job_id":42,"ended_at":"2026-02-30T00:00:00Z"`, 1),
		"time limit":    strings.Replace(good, `"job_id":42`, `"job_id":42,"time_limit":"5 hours"`, 1),
		"omitted":       strings.Replace(good, `"job_id":42`, `"job_id":42,"metadata_omitted":["job.secret"]`, 1),
		"usage lie":     strings.Replace(good, `"observed_gpu_seconds":7200`, `"observed_gpu_seconds":9000`, 1),
		"raw":           strings.Replace(good, `"controller":{"job_id":42}`, `"controller":null`, 1),
	} {
		expect(name, ingest(bad), 400, "")
	}
	expect("content type", func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", CompletionPath, strings.NewReader(good))
		req = req.WithContext(authn.WithBridgePeer(req.Context(), fp))
		rec := httptest.NewRecorder()
		a.Handler.ServeHTTP(rec, req)
		return rec
	}(), 415, "")

	var preview struct {
		Data struct {
			Subject, HTML, Text string
			Variables           []struct{ Name string }
		}
	}
	rec = do("POST", "/api/v2/notification/preview", "", admin, `{}`)
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &preview) != nil || preview.Data.Subject != "[example-cluster] 作业 12345 COMPLETED" ||
		!strings.Contains(preview.Data.HTML, "示例作业") || len(preview.Data.Variables) == 0 {
		t.Fatalf("preview %d %s", rec.Code, rec.Body.String())
	}
	rec = do("POST", "/api/v2/notification/preview", "", admin, `{"template":{"subject":"{{ job.ended_at_local }}","html":"{{ user.mail }}"},"timeZone":"UTC"}`)
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &preview) != nil || preview.Data.Subject != "2026-10-25 01:00:01 (UTC)" || preview.Data.Text != "admin@example.com" {
		t.Fatalf("preview override %d %s", rec.Code, rec.Body.String())
	}
	expect("preview bad template", do("POST", "/api/v2/notification/preview", "", admin, `{"template":{"subject":"x","html":"{{ secrets }}"}}`), 400, "")
	expect("preview user", do("POST", "/api/v2/notification/preview", "", alice, `{}`), 403, "")

	before := mailer.count()
	expect("test", do("POST", "/api/v2/notification/test", "", admin, `{}`), 200, `{"data":{"outcome":"smtp_accepted"}}`)
	expect("test to", do("POST", "/api/v2/notification/test", "", admin, `{"to":"ops@example.org"}`), 200, "")
	if mailer.count() != before+2 || mailer.sent[before].to[0] != "admin@example.com" || mailer.sent[before+1].to[0] != "ops@example.org" {
		t.Fatalf("test mails %+v", mailer.sent[before:])
	}
	expect("test bad to", do("POST", "/api/v2/notification/test", "", admin, `{"to":"x@localhost"}`), 400, "")
	expect("test user", do("POST", "/api/v2/notification/test", "", alice, `{}`), 403, "")
	mailer.err = &mail.Error{Kind: "smtp_authentication_failed"}
	expect("test smtp failure", do("POST", "/api/v2/notification/test", "", admin, `{}`), 502, "")
	mailer.err = nil

	expect("clear while enabled", do("PUT", "/api/v2/mail/providers/default", "", admin, provider(true, `,"clearSecret":true`)), 400, "")
	rec = do("PUT", "/api/v2/mail/providers/default", "", admin, provider(false, `,"clearSecret":true`))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"secretSet":false`) {
		t.Fatalf("clear %d %s", rec.Code, rec.Body.String())
	}
	expect("provider disabled", ingest(completion(t, "s0123456789ABK", 10001, 3600)), 200, `{"outcome":"skipped","reason":"mail_provider_unavailable"}`)
	expect("test with provider disabled", do("POST", "/api/v2/notification/test", "", admin, `{}`), 400, "")
}
