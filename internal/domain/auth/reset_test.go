package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/mail"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
)

type fakeMailer struct {
	mu  sync.Mutex
	got []mail.Message
	err error
}

func (f *fakeMailer) Send(_ context.Context, _ string, m mail.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.got = append(f.got, m)
	return nil
}

func (f *fakeMailer) lastCode(t *testing.T) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.got) == 0 {
		t.Fatal("no mail sent")
	}
	m := regexp.MustCompile(`\b(\d{6})\b`).FindStringSubmatch(f.got[len(f.got)-1].Subject)
	if m == nil {
		t.Fatalf("no code in %q", f.got[len(f.got)-1].Subject)
	}
	return m[1]
}

func TestPasswordResetByEmailCode(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if _, err := e.db.Exec(ctx, "UPDATE users SET mail = 'Alice@Example.org', nickname = 'Alice' WHERE username = 'alice'"); err != nil {
		t.Fatal(err)
	}
	mailer := &fakeMailer{}
	now := time.Date(2026, 10, 31, 2, 0, 0, 0, time.UTC)
	e.svc.Mailer, e.svc.ResetKey, e.svc.Now = mailer, []byte("reset-key"), func() time.Time { return now }
	req := func(login string) int {
		return e.do(http.MethodPost, "/api/v2/auth/password-reset/request", `{"login":"`+login+`"}`, "", "127.0.0.1:1", false).Code
	}
	confirm := func(login, code, pw string) int {
		return e.do(http.MethodPost, "/api/v2/auth/password-reset/confirm", `{"login":"`+login+`","code":"`+code+`","newPassword":"`+pw+`"}`, "", "127.0.0.1:1", false).Code
	}

	// Unknown, disabled or mail-less accounts answer the same and send nothing.
	for _, login := range []string{"nobody", "bob", "corrupt", "nobody@example.org"} {
		if c := req(login); c != http.StatusNoContent {
			t.Fatalf("%s: %d", login, c)
		}
	}
	if len(mailer.got) != 0 {
		t.Fatalf("mail sent for unusable accounts: %d", len(mailer.got))
	}
	if c := e.do(http.MethodPost, "/api/v2/auth/password-reset/request", `{"login":"alice"}`, "", "10.0.0.1:1", false).Code; c != http.StatusForbidden {
		t.Fatalf("plain http accepted: %d", c)
	}

	// By e-mail (case-insensitive): one mail with the code; the code itself is never stored.
	if c := req("alice@example.org"); c != http.StatusNoContent {
		t.Fatalf("request %d", c)
	}
	code := mailer.lastCode(t)
	m := mailer.got[0]
	if m.To != "Alice@Example.org" || !strings.Contains(m.HTML, code) || !strings.Contains(m.HTML, "Alice") || !strings.Contains(m.Text, code) {
		t.Fatalf("mail %+v", m)
	}
	var stored string
	_ = store.QueryRow(ctx, e.db, "SELECT code_hash FROM v2_password_resets", nil, &stored)
	if stored == "" || strings.Contains(stored, code) {
		t.Fatalf("stored %q", stored)
	}
	// A second request within the resend gap sends nothing new.
	req("alice")
	if len(mailer.got) != 1 {
		t.Fatalf("resent within gap: %d", len(mailer.got))
	}
	// Requests per address are limited.
	for i := 0; i < 5; i++ {
		req("nobody")
	}
	if c := req("nobody"); c != http.StatusTooManyRequests {
		t.Fatalf("request limit not applied: %d", c)
	}
	e.svc.Throttle = NewThrottle(5, 15*time.Minute)

	// Wrong codes count; a weak password is refused before the code is checked.
	wrong := "000000"
	if wrong == code {
		wrong = "111111"
	}
	if c := confirm("alice", wrong, "N3w-passw0rd"); c != http.StatusBadRequest {
		t.Fatalf("wrong code %d", c)
	}
	if c := confirm("alice", code, "123"); c != http.StatusBadRequest {
		t.Fatalf("weak password %d", c)
	}
	if c := confirm("alice", code, "N3w-passw0rd"); c != http.StatusNoContent {
		t.Fatalf("confirm %d", c)
	}
	if c, _ := e.login(t, "alice", "N3w-passw0rd"); c != http.StatusOK {
		t.Fatalf("login with new password %d", c)
	}
	// Single use.
	if c := confirm("alice", code, "An0ther-passw0rd"); c != http.StatusBadRequest {
		t.Fatalf("code reused %d", c)
	}

	// Expiry and the attempt limit discard a code.
	now = now.Add(2 * resendGap)
	req("alice")
	code = mailer.lastCode(t)
	now = now.Add(resetTTL + time.Second)
	if c := confirm("alice", code, "An0ther-passw0rd"); c != http.StatusBadRequest {
		t.Fatalf("expired code accepted %d", c)
	}
	now = now.Add(2 * resendGap)
	req("alice")
	code = mailer.lastCode(t)
	for i := 0; i < resetAttempts; i++ {
		confirm("alice", wrong, "An0ther-passw0rd")
	}
	if c := confirm("alice", code, "An0ther-passw0rd"); c != http.StatusBadRequest {
		t.Fatalf("code survived %d wrong attempts: %d", resetAttempts, c)
	}

	// A failed mail drops the code so the user can retry at once.
	now = now.Add(2 * resendGap)
	e.svc.Throttle = NewThrottle(5, 15*time.Minute) // the per-address request limit is covered above
	mailer.err = &mail.Error{Kind: "smtp_unavailable"}
	if c := req("alice"); c != http.StatusServiceUnavailable {
		t.Fatalf("mail failure %d", c)
	}
	var n int
	_ = store.QueryRow(ctx, e.db, "SELECT COUNT(*) FROM v2_password_resets", nil, &n)
	if n != 0 {
		t.Fatalf("code kept after failed mail")
	}
}

func TestChangeOwnPasswordWithEmailCode(t *testing.T) {
	e := setup(t)
	e.convert(t)
	mailer := &fakeMailer{}
	now := time.Date(2026, 10, 31, 3, 0, 0, 0, time.UTC)
	e.svc.Mailer, e.svc.ResetKey, e.svc.Now = mailer, []byte("reset-key"), func() time.Time { return now }
	_, tok := e.login(t, "alice", "abc12345")
	send := func() int {
		return e.do(http.MethodPost, "/api/v2/me/password/code", ``, tok, "127.0.0.1:1", false).Code
	}
	change := func(body string) int {
		return e.do(http.MethodPut, "/api/v2/me/password", body, tok, "127.0.0.1:1", false).Code
	}

	if c := send(); c != http.StatusBadRequest {
		t.Fatalf("account without e-mail: %d", c)
	}
	if _, err := e.db.Exec(context.Background(), "UPDATE users SET mail = 'alice@example.org' WHERE username = 'alice'"); err != nil {
		t.Fatal(err)
	}
	if c := send(); c != http.StatusNoContent {
		t.Fatalf("send %d", c)
	}
	if c := send(); c != http.StatusTooManyRequests {
		t.Fatalf("resend within a minute: %d", c)
	}
	code := mailer.lastCode(t)
	if mailer.got[0].To != "alice@example.org" {
		t.Fatalf("sent to %q", mailer.got[0].To)
	}
	if c := change(`{"currentPassword":"abc12345","code":"` + code + `","newPassword":"N3w-passw0rd"}`); c != http.StatusBadRequest {
		t.Fatalf("both proofs accepted: %d", c)
	}
	bad := "000000"
	if bad == code {
		bad = "111111"
	}
	if c := change(`{"code":"` + bad + `","newPassword":"N3w-passw0rd"}`); c != http.StatusBadRequest {
		t.Fatalf("wrong code %d", c)
	}
	if c := change(`{"code":"` + code + `","newPassword":"N3w-passw0rd"}`); c != http.StatusNoContent {
		t.Fatalf("change with code %d", c)
	}
	if c, _ := e.login(t, "alice", "N3w-passw0rd"); c != http.StatusOK {
		t.Fatalf("new password %d", c)
	}
	// The current-password path is unchanged.
	if c := change(`{"currentPassword":"N3w-passw0rd","newPassword":"Thi3d-passw0rd"}`); c != http.StatusNoContent {
		t.Fatalf("change with current password %d", c)
	}
}

func TestPasswordMailSettings(t *testing.T) {
	e := setup(t)
	e.convert(t)
	ctx := context.Background()
	if _, err := e.db.Exec(ctx, "UPDATE users SET mail = 'alice@example.org' WHERE username = 'alice'"); err != nil {
		t.Fatal(err)
	}
	mailer := &fakeMailer{}
	now := time.Date(2026, 10, 31, 4, 0, 0, 0, time.UTC)
	e.svc.Mailer, e.svc.ResetKey, e.svc.Now = mailer, []byte("reset-key"), func() time.Time { return now }
	_, tok := e.login(t, "alice", "abc12345")
	get := func(path string) *httptest.ResponseRecorder {
		return e.do(http.MethodGet, path, "", tok, "127.0.0.1:1", false)
	}

	// Enabled by default with the built-in card.
	if rec := e.do(http.MethodGet, "/api/v2/auth/password-reset", "", "", "127.0.0.1:1", false); !strings.Contains(rec.Body.String(), `"enabled":true`) {
		t.Fatalf("status %s", rec.Body.String())
	}
	var cfg struct {
		Data PasswordMail `json:"data"`
	}
	if rec := get("/api/v2/password-mail/config"); rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &cfg) != nil || !cfg.Data.Enabled || !strings.Contains(cfg.Data.Template.HTML, "{{ reset.code }}") {
		t.Fatalf("config %d %s", rec.Code, rec.Body.String())
	}
	// Sample data is always an example; real codes are never kept.
	if rec := get("/api/v2/password-mail/sample"); !strings.Contains(rec.Body.String(), `"source":"example"`) || !strings.Contains(rec.Body.String(), "123456") {
		t.Fatalf("sample %s", rec.Body.String())
	}
	// A saved template is what the code mail uses; unknown variables are refused.
	put := func(body string) int {
		return e.do(http.MethodPut, "/api/v2/password-mail/config", body, tok, "127.0.0.1:1", false).Code
	}
	if c := put(`{"enabled":true,"timeZone":"UTC","template":{"subject":"Code {{ secret.code }}","html":"x"}}`); c != http.StatusBadRequest {
		t.Fatalf("unknown variable accepted: %d", c)
	}
	if c := put(`{"enabled":true,"provider":"","timeZone":"UTC","template":{"subject":"Your code {{ reset.code }}","html":"<p>{{ user.username }} {{ reset.code }} {{ constants.org }}</p>","constants":{"org":"Lab"}}}`); c != http.StatusOK {
		t.Fatalf("save %d", c)
	}
	if c := e.do(http.MethodPost, "/api/v2/me/password/code", "", tok, "127.0.0.1:1", false).Code; c != http.StatusNoContent {
		t.Fatalf("send %d", c)
	}
	m := mailer.got[len(mailer.got)-1]
	code := mailer.lastCode(t)
	if !strings.HasPrefix(m.Subject, "Your code ") || m.HTML != "<p>alice "+code+" Lab</p>" || m.Purpose != "password" {
		t.Fatalf("mail %+v", m)
	}

	// Disabled: no codes are sent or accepted, and the option is hidden.
	if c := put(`{"enabled":false,"timeZone":"UTC","template":{"subject":"Your code {{ reset.code }}","html":"x"}}`); c != http.StatusOK {
		t.Fatalf("disable %d", c)
	}
	if rec := e.do(http.MethodGet, "/api/v2/auth/password-reset", "", "", "127.0.0.1:1", false); !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Fatalf("status %s", rec.Body.String())
	}
	if c := e.do(http.MethodPost, "/api/v2/auth/password-reset/request", `{"login":"alice"}`, "", "127.0.0.1:1", false).Code; c != http.StatusForbidden {
		t.Fatalf("request while disabled %d", c)
	}
	if c := e.do(http.MethodPut, "/api/v2/me/password", `{"code":"`+code+`","newPassword":"N3w-passw0rd"}`, tok, "127.0.0.1:1", false).Code; c != http.StatusForbidden {
		t.Fatalf("pending code accepted while disabled %d", c)
	}
}
