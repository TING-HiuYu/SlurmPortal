package mail_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"strings"
	"sync"
	"testing"

	"github.com/TING-HiuYu/SlurmPortal/internal/secretbox"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
	"github.com/TING-HiuYu/SlurmPortal/internal/testutil"
	"github.com/TING-HiuYu/SlurmPortal/migrations"

	. "github.com/TING-HiuYu/SlurmPortal/internal/mail"
)

type recorder struct {
	mu   sync.Mutex
	from []string
	err  error
}

func (r *recorder) Send(_ context.Context, cfg SMTPConfig, pw, from string, _ []string, _ []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.from = append(r.from, from+"/"+pw)
	return nil
}

func smtpInput(from string, enabled, def bool, secret string) Input {
	cfg, _ := json.Marshal(SMTPConfig{Host: "smtp.example.com", Port: 465, Security: "tls", Username: "u", FromAddress: from, TimeoutMs: 2000})
	in := Input{Kind: "smtp", Enabled: enabled, Default: def, Config: cfg}
	if secret != "" {
		in.Secret = &secret
	}
	return in
}

func newHub(t *testing.T, key string) (*Hub, store.Store) {
	t.Helper()
	db := testutil.DB(t)
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatal(err)
	}
	return NewHub(db, box), db
}

var masterKey = base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))

func TestHubProviders(t *testing.T) {
	rec := &recorder{}
	defer SetTransport(rec)()
	h, _ := newHub(t, masterKey)
	ctx := context.Background()
	msg := Message{ID: "m1", To: "user@example.org", Subject: "s", Text: "t", HTML: "<p>t</p>"}

	if err := h.Send(ctx, "", msg); !errors.Is(err, ErrNoProvider) || !Unavailable(err) {
		t.Fatalf("no default: %v", err)
	}
	if _, err := h.Save(ctx, "a", smtpInput("a@example.com", true, true, "pa")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Save(ctx, "b", smtpInput("b@example.com", true, true, "pb")); err != nil {
		t.Fatal(err)
	}
	all, _ := h.List(ctx)
	if len(all) != 2 || all[0].Default || !all[1].Default {
		t.Fatalf("default not moved: %+v", all)
	}
	if err := h.Send(ctx, "", msg); err != nil {
		t.Fatal(err)
	}
	if err := h.Send(ctx, "a", msg); err != nil {
		t.Fatal(err)
	}
	if strings.Join(rec.from, ",") != "b@example.com/pb,a@example.com/pa" {
		t.Fatalf("routed %v", rec.from)
	}
	if _, err := h.Save(ctx, "a", smtpInput("a@example.com", false, false, "")); err != nil {
		t.Fatal(err)
	}
	if err := h.Send(ctx, "a", msg); !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled: %v", err)
	}
	in := smtpInput("a@example.com", false, false, "")
	in.Kind = "smtp-other"
	if _, err := h.Save(ctx, "a", in); err == nil {
		t.Fatal("unknown kind accepted")
	}
	if err := h.Delete(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if err := h.Delete(ctx, "a"); err == nil {
		t.Fatal("double delete")
	}
	rec.err = &Error{Kind: "smtp_recipient_rejected", Permanent: true}
	if err := h.Send(ctx, "b", msg); !RecipientRejected(err) {
		t.Fatalf("rejection: %v", err)
	}
}

func TestHubSecretNeedsMasterKey(t *testing.T) {
	h, _ := newHub(t, "")
	_, err := h.Save(context.Background(), "a", smtpInput("a@example.com", false, false, "pw"))
	if err == nil || !strings.Contains(err.Error(), "PKI_MASTER_KEY") {
		t.Fatalf("%v", err)
	}
}

func TestBuildMessage(t *testing.T) {
	cfg := SMTPConfig{FromAddress: "bot@example.com", FromName: `Ops "Team"`, ReplyTo: "help@example.com"}
	msg, err := BuildMessageForTest(cfg, "u@example.com", "完成 ok", "hi")
	if err != nil {
		t.Fatal(err)
	}
	s := string(msg)
	for _, want := range []string{"From: \"Ops \\\"Team\\\"\" <bot@example.com>\r\n", "Reply-To: <help@example.com>\r\n", "Subject: =?UTF-8?B?", "MIME-Version: 1.0\r\n", "Message-ID: <", "Content-Type: text/plain; charset=UTF-8"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in %s", want, s)
		}
	}
	if _, err := BuildMessageForTest(cfg, "u@example.com\r\nBcc: x@example.com", "s", "t"); err == nil {
		t.Fatal("header injection via recipient")
	}
	cfg.FromName = "a\r\nBcc: x@example.com"
	if _, err := BuildMessageForTest(cfg, "u@example.com", "s", "t"); err == nil {
		t.Fatal("header injection via from name")
	}
}

// Migration 0010 moves an existing notification SMTP account, with its sealed password, into provider "default".
func TestMigrationMovesNotificationSMTP(t *testing.T) {
	h, db := newHub(t, masterKey)
	ctx := context.Background()
	box, _ := secretbox.New(masterKey)
	sealed, err := box.Seal(SMTPPasswordPurpose, []byte("old-pw"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO v2_notification_config (id, config_json, smtp_password_sealed, updated_at) VALUES (1, ?, ?, '2026-10-01T00:00:00Z')`,
		`{"enabled":true,"smtp":{"host":"smtp.example.com","port":465,"security":"tls","username":"u","fromAddress":"old@example.com","timeoutMs":3000}}`, sealed); err != nil {
		t.Fatal(err)
	}
	raw, err := fs.ReadFile(migrations.FS, "0010_mail_providers.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, strings.Split(string(raw), store.StatementSeparator)[1]); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	defer SetTransport(rec)()
	h = NewHub(db, box)
	if err := h.Send(ctx, "", Message{ID: "x", To: "u@example.org", Subject: "s"}); err != nil || rec.from[0] != "old@example.com/old-pw" {
		t.Fatalf("migrated provider: %v %v", err, rec.from)
	}
}

// Every delivery through a leaf provider is counted per day, provider and purpose.
func TestHubCountsDeliveries(t *testing.T) {
	rec := &recorder{}
	defer SetTransport(rec)()
	h, _ := newHub(t, masterKey)
	ctx := context.Background()
	if _, err := h.Save(ctx, "a", smtpInput("a@example.com", true, true, "pa")); err != nil {
		t.Fatal(err)
	}
	msg := func(purpose string) Message {
		return Message{ID: "m", To: "user@example.org", Subject: "s", Text: "t", Purpose: purpose}
	}
	for _, p := range []string{"job", "job", "password", ""} {
		if err := h.Send(ctx, "", msg(p)); err != nil {
			t.Fatal(err)
		}
	}
	rec.err = &Error{Kind: "smtp_unavailable"}
	_ = h.Send(ctx, "a", msg("job"))
	rows, err := h.Usage(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][2]int{}
	for _, r := range rows {
		if r.Provider != "a" {
			t.Fatalf("counted against %q", r.Provider)
		}
		got[r.Purpose] = [2]int{r.Sent, r.Failed}
	}
	if got["job"] != [2]int{2, 1} || got["password"] != [2]int{1, 0} || got["other"] != [2]int{1, 0} || len(got) != 3 {
		t.Fatalf("usage %v", got)
	}
}
