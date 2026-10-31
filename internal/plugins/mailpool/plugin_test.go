package mailpool_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/app"
	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
	"github.com/TING-HiuYu/SlurmPortal/internal/config"
	"github.com/TING-HiuYu/SlurmPortal/internal/mail"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugin"
	"github.com/TING-HiuYu/SlurmPortal/internal/testutil"

	. "github.com/TING-HiuYu/SlurmPortal/internal/plugins/mailpool"
)

func init() { plugin.Register(Plugin{}) }

// transport records which account (sender address) carried each message and fails chosen accounts.
type transport struct {
	mu    sync.Mutex
	sent  []string
	fails map[string]error
}

func (f *transport) Send(_ context.Context, cfg mail.SMTPConfig, _, _ string, _ []string, _ []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	account := strings.TrimSuffix(cfg.FromAddress, "@example.com")
	if err := f.fails[account]; err != nil {
		return err
	}
	f.sent = append(f.sent, account)
	return nil
}

func (f *transport) take() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := strings.Join(f.sent, ",")
	f.sent = nil
	return s
}

func setup(t *testing.T, plugins ...string) (*app.App, func(method, path, body string) *httptest.ResponseRecorder, *transport) {
	t.Helper()
	db := testutil.DB(t)
	admin := testutil.User(t, db, "admin", 1, "admin")
	tr := &transport{fails: map[string]error{}}
	t.Cleanup(mail.SetTransport(tr))
	a, err := app.New(context.Background(), &config.Config{
		Role: "master", HTTP: config.HTTP{RatePerSecond: 1000, RateBurst: 1000},
		Auth: config.Auth{Issuer: "i", Audience: "a", TokenTTL: time.Hour}, Plugins: config.Plugins{Enabled: plugins},
		JWTKeys:      authn.Keyring{ActiveKID: "k", Keys: map[string][]byte{"k": []byte("0123456789abcdef0123456789abcdef")}},
		PKIMasterKey: base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
	}, db)
	if err != nil {
		t.Fatal(err)
	}
	tok, _, _, err := a.JWT.Issue(admin, authn.SourcePassword)
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		a.Handler.ServeHTTP(rec, req)
		return rec
	}
	for _, name := range []string{"smtp1", "smtp2", "smtp3"} {
		body := fmt.Sprintf(`{"kind":"smtp","enabled":true,"config":{"host":"smtp.example.com","port":465,"security":"tls","username":"u","fromAddress":"%s@example.com","timeoutMs":2000},"secret":"pw"}`, name)
		if rec := do("PUT", "/api/v2/mail/providers/"+name, body); rec.Code != 200 {
			t.Fatalf("provider %s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	return a, do, tr
}

func pool(strategy string, failover bool, members string) string {
	return fmt.Sprintf(`{"kind":"mail-pool","enabled":true,"default":true,"config":{"strategy":%q,"period":"day","failover":%t,"members":%s}}`, strategy, failover, members)
}

func send(t *testing.T, a *app.App, n int) []error {
	t.Helper()
	var errs []error
	for i := range n {
		errs = append(errs, a.Mail.Send(context.Background(), "", mail.Message{ID: fmt.Sprint(i), To: "user@example.org", Subject: "s", Text: "t", HTML: "t"}))
	}
	return errs
}

func TestPriorityQuotas(t *testing.T) {
	a, do, tr := setup(t, ID)
	// The first 3 a day through smtp1, the next 2 through smtp2, the rest through smtp3.
	if rec := do("PUT", "/api/v2/mail/providers/free", pool(StrategyPriority, true, `[{"provider":"smtp1","quota":3},{"provider":"smtp2","quota":2},{"provider":"smtp3"}]`)); rec.Code != 200 {
		t.Fatalf("pool: %d %s", rec.Code, rec.Body.String())
	}
	send(t, a, 7)
	if got := tr.take(); got != "smtp1,smtp1,smtp1,smtp2,smtp2,smtp3,smtp3" {
		t.Fatalf("priority order %s", got)
	}
	// The hub counts each delivery against the member that sent it, never against the pool.
	var counts struct {
		Data []mail.UsageRow `json:"data"`
	}
	if rec := do("GET", "/api/v2/mail/usage", ""); rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &counts) != nil {
		t.Fatalf("mail usage %d %s", rec.Code, rec.Body.String())
	}
	byProvider := map[string]int{}
	for _, r := range counts.Data {
		byProvider[r.Provider] += r.Sent
	}
	if byProvider["smtp1"] != 3 || byProvider["smtp2"] != 2 || byProvider["smtp3"] != 2 || byProvider["free"] != 0 {
		t.Fatalf("delivery counts %v", byProvider)
	}
	var usage struct {
		Data []PoolUsage `json:"data"`
	}
	rec := do("GET", "/api/v2/mail-pool/usage", "")
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &usage) != nil || len(usage.Data) != 1 {
		t.Fatalf("usage %d %s", rec.Code, rec.Body.String())
	}
	m := usage.Data[0].Members
	if m[0].Sent != 3 || *m[0].Remaining != 0 || m[1].Sent != 2 || m[2].Sent != 2 || m[2].Remaining != nil || usage.Data[0].PeriodKey != time.Now().UTC().Format("2006-01-02") {
		t.Fatalf("usage counters %+v", usage.Data[0])
	}

	// A failing member is skipped (failover) and its failure counted; quota is not consumed.
	tr.fails["smtp3"] = &mail.Error{Kind: "smtp_unavailable"}
	if errs := send(t, a, 1); errs[0] == nil {
		t.Fatal("all remaining members failed, want error")
	}
	do("GET", "/api/v2/mail-pool/usage", "")
	// A rejected recipient is final; no other member is tried.
	tr.fails["smtp3"] = &mail.Error{Kind: "smtp_recipient_rejected", Permanent: true}
	if errs := send(t, a, 1); !mail.RecipientRejected(errs[0]) {
		t.Fatalf("rejection %v", errs[0])
	}

	// Exhausted pool.
	if rec := do("PUT", "/api/v2/mail/providers/free", pool(StrategyPriority, true, `[{"provider":"smtp1","quota":3},{"provider":"smtp2","quota":2}]`)); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	if errs := send(t, a, 1); !errors.Is(errs[0], ErrExhausted) {
		t.Fatalf("exhausted %v", errs[0])
	}
	// Members in use cannot be deleted; the pool cannot contain itself or unknown providers.
	if rec := do("DELETE", "/api/v2/mail/providers/smtp1", ""); rec.Code != 409 {
		t.Fatalf("delete in-use %d", rec.Code)
	}
	for _, members := range []string{`[{"provider":"free"}]`, `[{"provider":"ghost"}]`, `[{"provider":"smtp1"},{"provider":"smtp1"}]`, `[{"provider":"smtp1","quota":-1}]`, `[]`} {
		if rec := do("PUT", "/api/v2/mail/providers/free", pool(StrategyPriority, true, members)); rec.Code != 400 {
			t.Fatalf("members %s accepted: %d", members, rec.Code)
		}
	}
	if rec := do("PUT", "/api/v2/mail/providers/free", pool("random", true, `[{"provider":"smtp1"}]`)); rec.Code != 400 {
		t.Fatal("unknown strategy accepted")
	}
}

func TestRoundRobinAndWeighted(t *testing.T) {
	a, do, tr := setup(t, ID)
	do("PUT", "/api/v2/mail/providers/rr", pool(StrategyRoundRobin, false, `[{"provider":"smtp1"},{"provider":"smtp2"},{"provider":"smtp3"}]`))
	send(t, a, 6)
	if got := tr.take(); got != "smtp1,smtp2,smtp3,smtp1,smtp2,smtp3" {
		t.Fatalf("round robin %s", got)
	}
	// Without failover a failure is returned as is.
	tr.fails["smtp1"] = &mail.Error{Kind: "smtp_unavailable"}
	if errs := send(t, a, 1); errs[0] == nil {
		t.Fatal("failover disabled but message delivered")
	}
	delete(tr.fails, "smtp1")
	tr.take()

	do("PUT", "/api/v2/mail/providers/rr", `{"kind":"mail-pool","enabled":false,"config":{"strategy":"round-robin","members":[{"provider":"smtp1"}]}}`)
	do("PUT", "/api/v2/mail/providers/w", pool(StrategyWeighted, true, `[{"provider":"smtp1","weight":3},{"provider":"smtp2","weight":1}]`))
	send(t, a, 8)
	counts := map[string]int{}
	for _, s := range strings.Split(tr.take(), ",") {
		counts[s]++
	}
	if counts["smtp1"] != 6 || counts["smtp2"] != 2 {
		t.Fatalf("weighted %v", counts)
	}
	// A disabled member is skipped without counting as a failure.
	do("PUT", "/api/v2/mail/providers/smtp1", `{"kind":"smtp","enabled":false,"config":{"host":"smtp.example.com","port":465,"security":"tls","username":"u","fromAddress":"smtp1@example.com","timeoutMs":2000}}`)
	send(t, a, 2)
	if got := tr.take(); got != "smtp2,smtp2" {
		t.Fatalf("disabled member used: %s", got)
	}
}

// Without the plugin, pools stay stored but cannot deliver, and their kind cannot be saved.
func TestPluginDisabled(t *testing.T) {
	_, do, _ := setup(t)
	if rec := do("PUT", "/api/v2/mail/providers/free", pool(StrategyPriority, true, `[{"provider":"smtp1"}]`)); rec.Code != 400 {
		t.Fatalf("pool saved without plugin: %d", rec.Code)
	}
	if rec := do("GET", "/api/v2/mail-pool/usage", ""); rec.Code != 404 {
		t.Fatalf("usage route without plugin: %d", rec.Code)
	}
}
