package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/apperr"
	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
	"github.com/TING-HiuYu/SlurmPortal/internal/authz"
	"github.com/TING-HiuYu/SlurmPortal/internal/httpx"
	"github.com/TING-HiuYu/SlurmPortal/internal/mail"
	"github.com/TING-HiuYu/SlurmPortal/internal/password"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
)

// Password reset by e-mailed one-time code. Only an HMAC of the code is stored (keyed with
// ResetKey), a code lives resetTTL, a new one can be sent every resendGap, and resetAttempts wrong
// codes discard it. Limits live in the database so they hold across Worker instances. The request
// endpoint answers the same whether or not the account exists.

const (
	resetTTL      = 10 * time.Minute
	resendGap     = time.Minute
	resetAttempts = 5
)

var errBadCode = apperr.New(http.StatusBadRequest, "invalid_code", "the code is wrong or has expired")

// Mailer sends through the mail hub; "" selects the default provider.
type Mailer interface {
	Send(ctx context.Context, provider string, m mail.Message) error
}

type resetAccount struct {
	id                       int64
	username, nickname, mail string
}

// account finds the active account by username or e-mail (case-insensitive).
func (s *Service) account(ctx context.Context, login string) (resetAccount, error) {
	var a resetAccount
	login = strings.TrimSpace(login)
	q := `SELECT id, username, COALESCE(nickname,''), COALESCE(mail,'') FROM users
		WHERE username = ? AND status = 1 AND deleted_at IS NULL`
	args := []any{login}
	if strings.Contains(login, "@") {
		// trim(mail) <> '' lets SQLite use the partial unique index on lower(trim(mail)).
		q = `SELECT id, username, COALESCE(nickname,''), COALESCE(mail,'') FROM users
			WHERE lower(trim(mail)) = ? AND trim(mail) <> '' AND status = 1 AND deleted_at IS NULL LIMIT 2`
		args = []any{strings.ToLower(login)}
	}
	rows, err := s.DB.Query(ctx, q, args...)
	if err != nil {
		return a, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		if err := rows.Scan(&a.id, &a.username, &a.nickname, &a.mail); err != nil {
			return a, err
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return a, err
	}
	if n != 1 || strings.TrimSpace(a.mail) == "" {
		return resetAccount{}, store.ErrNoRows
	}
	return a, nil
}

// accountByID is the signed-in variant: the active account and its e-mail address.
func (s *Service) accountByID(ctx context.Context, id int64) (resetAccount, error) {
	a := resetAccount{id: id}
	err := store.QueryRow(ctx, s.DB, `SELECT username, COALESCE(nickname,''), COALESCE(mail,'') FROM users
		WHERE id = ? AND status = 1 AND deleted_at IS NULL`, []any{id}, &a.username, &a.nickname, &a.mail)
	return a, err
}

var (
	errNoEmail    = apperr.New(http.StatusBadRequest, "no_email", "the account has no e-mail address")
	errCodeRecent = apperr.New(http.StatusTooManyRequests, "code_recently_sent", "a code was sent less than a minute ago")
)

func (s *Service) codeHash(userID int64, code string) string {
	m := hmac.New(sha256.New, s.ResetKey)
	fmt.Fprintf(m, "password-reset\x00%d\x00%s", userID, code)
	return hex.EncodeToString(m.Sum(nil))
}

func newCode() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", binary.BigEndian.Uint64(b[:])%1_000_000), nil
}

// RequestReset e-mails a code to the account's address. It reports no error for unknown accounts or
// while a recent code is still within resendGap, so callers cannot probe which accounts exist.
func (s *Service) RequestReset(ctx context.Context, login string) error {
	if s.Mailer == nil || len(s.ResetKey) == 0 {
		return apperr.ErrUnavailable
	}
	cfg, err := s.passwordMail(ctx)
	if err != nil {
		return err
	}
	if !cfg.Enabled {
		return errResetDisabled
	}
	a, err := s.account(ctx, login)
	if errors.Is(err, store.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := s.sendCode(ctx, a, cfg); err != nil && !errors.Is(err, errCodeRecent) {
		return err
	}
	return nil
}

// RequestOwnCode e-mails a code to the signed-in user's own address.
func (s *Service) RequestOwnCode(ctx context.Context, userID int64) error {
	if s.Mailer == nil || len(s.ResetKey) == 0 {
		return apperr.ErrUnavailable
	}
	cfg, err := s.passwordMail(ctx)
	if err != nil {
		return err
	}
	if !cfg.Enabled {
		return errResetDisabled
	}
	a, err := s.accountByID(ctx, userID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(a.mail) == "" {
		return errNoEmail
	}
	return s.sendCode(ctx, a, cfg)
}

// sendCode stores a fresh code and mails it; errCodeRecent while the last one is within resendGap.
func (s *Service) sendCode(ctx context.Context, a resetAccount, cfg PasswordMail) error {
	now := s.Now().UTC()
	var sent string
	err := store.QueryRow(ctx, s.DB, "SELECT sent_at FROM v2_password_resets WHERE user_id = ?", []any{a.id}, &sent)
	if err == nil {
		if t, perr := time.Parse(time.RFC3339Nano, sent); perr == nil && now.Sub(t) < resendGap {
			return errCodeRecent
		}
	} else if !errors.Is(err, store.ErrNoRows) {
		return err
	}
	code, err := newCode()
	if err != nil {
		return err
	}
	if _, err := s.DB.Exec(ctx, `INSERT INTO v2_password_resets(user_id, code_hash, expires_at, attempts, sent_at) VALUES (?, ?, ?, 0, ?)
		ON CONFLICT(user_id) DO UPDATE SET code_hash = excluded.code_hash, expires_at = excluded.expires_at, attempts = 0, sent_at = excluded.sent_at`,
		a.id, s.codeHash(a.id, code), now.Add(resetTTL).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		return err
	}
	out, err := mail.Render(cfg.Template, resetRoots, resetContext(a, code, now, cfg.TimeZone, cfg.Template.Constants), nil)
	if err != nil {
		return err
	}
	msg := mail.Message{ID: fmt.Sprintf("password-reset-%d-%d", a.id, now.UnixNano()), To: a.mail,
		Subject: out.Subject, Text: out.Text, HTML: out.HTML, Purpose: "password"}
	if err := s.Mailer.Send(ctx, cfg.Provider, msg); err != nil {
		// The code is unusable without the mail; drop it so a retry is not held back by resendGap.
		_, _ = s.DB.Exec(ctx, "DELETE FROM v2_password_resets WHERE user_id = ?", a.id)
		slog.ErrorContext(ctx, "password reset mail failed", "user", a.username, "err", err)
		return apperr.ErrUnavailable.Wrap(err)
	}
	return nil
}

// ConfirmReset sets a new password when the code matches; the code is single-use.
func (s *Service) ConfirmReset(ctx context.Context, login, code, newPassword string) (int64, error) {
	if len(s.ResetKey) == 0 {
		return 0, apperr.ErrUnavailable
	}
	a, err := s.account(ctx, login)
	if errors.Is(err, store.ErrNoRows) {
		return 0, errBadCode
	}
	if err != nil {
		return 0, err
	}
	return a.id, s.confirmCode(ctx, a.id, code, newPassword)
}

// confirmCode checks the user's pending code (counting wrong tries) and on success sets the password
// and consumes the code in one batch.
func (s *Service) confirmCode(ctx context.Context, userID int64, code, newPassword string) error {
	if len(s.ResetKey) == 0 {
		return apperr.ErrUnavailable
	}
	if cfg, err := s.passwordMail(ctx); err != nil {
		return err
	} else if !cfg.Enabled {
		return errResetDisabled
	}
	var hash, expires string
	var attempts int
	err := store.QueryRow(ctx, s.DB, "SELECT code_hash, expires_at, attempts FROM v2_password_resets WHERE user_id = ?", []any{userID}, &hash, &expires, &attempts)
	if errors.Is(err, store.ErrNoRows) {
		return errBadCode
	}
	if err != nil {
		return err
	}
	exp, perr := time.Parse(time.RFC3339Nano, expires)
	if perr != nil || !s.Now().Before(exp) || attempts >= resetAttempts {
		_, _ = s.DB.Exec(ctx, "DELETE FROM v2_password_resets WHERE user_id = ?", userID)
		return errBadCode
	}
	if !hmac.Equal([]byte(hash), []byte(s.codeHash(userID, strings.TrimSpace(code)))) {
		if attempts+1 >= resetAttempts {
			_, err = s.DB.Exec(ctx, "DELETE FROM v2_password_resets WHERE user_id = ?", userID)
		} else {
			_, err = s.DB.Exec(ctx, "UPDATE v2_password_resets SET attempts = attempts + 1 WHERE user_id = ?", userID)
		}
		if err != nil {
			return err
		}
		return errBadCode
	}
	c, err := password.Hash(newPassword)
	if err != nil {
		return apperr.BadRequest(err.Error())
	}
	_, err = s.DB.Batch(ctx, []store.Statement{
		{Query: "DELETE FROM v2_password_resets WHERE user_id = ? AND code_hash = ?", Args: []any{userID, hash}},
		{Query: `INSERT INTO v2_user_credentials(user_id, app_hash, ldap_hash, updated_at) VALUES (?, ?, ?, ?)
			ON CONFLICT(user_id) DO UPDATE SET app_hash = excluded.app_hash, ldap_hash = excluded.ldap_hash, updated_at = excluded.updated_at`,
			Args: []any{userID, c.App, c.LDAP, s.Now().UTC()}},
	})
	return err
}

var resetRoots = mail.Roots("user", "reset", "constants")

// resetConstants reuses the organisation, support address and footer of the job-notification
// template so the code mail carries the same branding.
func (s *Service) resetConstants(ctx context.Context) map[string]any {
	out := map[string]any{"organization": "HPC", "support_email": "-", "footer_note": "此邮件由系统自动发送，请勿回复。"}
	var raw string
	if err := store.QueryRow(ctx, s.DB, "SELECT config_json FROM v2_notification_config WHERE id = 1", nil, &raw); err != nil {
		return out
	}
	var cfg struct {
		Template struct {
			Constants map[string]any `json:"constants"`
		} `json:"template"`
	}
	if json.Unmarshal([]byte(raw), &cfg) == nil {
		for _, k := range []string{"organization", "support_email", "footer_note"} {
			if v, ok := cfg.Template.Constants[k].(string); ok && v != "" {
				out[k] = v
			}
		}
	}
	return out
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

const resetSubject = "[HPC] 密码重置验证码 {{ reset.code }}"

// resetHTML is the default template (editable under 集成与通知 → 密码验证码): the job and transfer
// notification layout in a violet palette.
const resetHTML = `<!doctype html>
<html lang="zh-CN">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>密码重置验证码</title></head>
<body style="margin:0;padding:0;background:#f0eef6;color:#33294d;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI','PingFang SC','Microsoft YaHei',sans-serif;">
<table role="presentation" width="100%" cellspacing="0" cellpadding="0" style="background:#f0eef6;"><tr><td align="center" style="padding:28px 16px;">
<table role="presentation" width="100%" cellspacing="0" cellpadding="0" style="max-width:620px;background:#ffffff;border:1px solid #e0dbed;border-radius:12px;overflow:hidden;">
<tr><td style="padding:30px 30px 24px;background:#3b2a6b;color:#ffffff;">
<p style="margin:0 0 15px;font-size:11px;letter-spacing:2px;color:#cdbfee;">{{ constants.organization }} / HPC Service</p>
<h1 style="margin:0;font-size:26px;font-weight:600;line-height:1.4;">密码重置验证码</h1>
<p style="margin:12px 0 0;font-size:13px;color:#d9cff2;">{{ user.nickname }} · {{ user.username }}</p>
</td></tr>
<tr><td style="padding:22px 30px 0;">
<table role="presentation" width="100%" cellspacing="0" cellpadding="0" style="border-collapse:collapse;">
<tr><td style="padding:18px 16px;background:#f2eefa;border-radius:7px;text-align:center;"><p style="margin:0 0 8px;font-size:11px;color:#8a7fa6;">验证码</p><strong style="font-size:32px;font-weight:600;letter-spacing:10px;font-family:'SFMono-Regular',Consolas,'Liberation Mono',monospace;">{{ reset.code }}</strong></td></tr>
</table>
</td></tr>
<tr><td style="padding:25px 30px 28px;">
<h2 style="font-size:15px;margin:0 0 13px;font-weight:600;">使用说明</h2>
<table role="presentation" width="100%" cellspacing="0" cellpadding="0" style="font-size:12px;line-height:1.8;border-top:1px solid #e8e3f1;">
<tr><td style="padding:10px 0 7px;width:31%;color:#8f88a0;vertical-align:top;">有效期</td><td style="padding:10px 0 7px;">{{ reset.minutes }} 分钟，至 {{ reset.expires_at_local }}</td></tr>
<tr><td style="padding:7px 0;color:#8f88a0;vertical-align:top;">用途</td><td style="padding:7px 0;">在登录页「忘记密码」中输入此验证码并设置新密码；验证码只能使用一次。</td></tr>
<tr><td style="padding:7px 0;color:#8f88a0;vertical-align:top;">并非本人操作？</td><td style="padding:7px 0;">请忽略此邮件，你的密码不会改变。</td></tr>
</table>
</td></tr>
<tr><td style="padding:18px 30px;background:#f8f7fb;border-top:1px solid #e8e3f1;color:#9a94a8;font-size:10px;line-height:1.9;">{{ constants.footer_note }}<br>支持联系：{{ constants.support_email }}</td></tr>
</table>
<p style="margin:18px 0 0;color:#aaa3bb;font-size:10px;letter-spacing:1px;">{{ constants.organization }} · Email Notification Service</p>
</td></tr></table>
</body></html>
`

type resetRequest struct {
	Login string `json:"login"`
}

func (r resetRequest) Validate() error {
	if l := strings.TrimSpace(r.Login); l == "" || len(l) > 254 {
		return errors.New("login is required")
	}
	return nil
}

type resetConfirm struct {
	Login       string `json:"login"`
	Code        string `json:"code"`
	NewPassword string `json:"newPassword"`
}

func (r resetConfirm) Validate() error {
	if l := strings.TrimSpace(r.Login); l == "" || len(l) > 254 {
		return errors.New("login is required")
	}
	if c := strings.TrimSpace(r.Code); len(c) != 6 || strings.Trim(c, "0123456789") != "" {
		return errors.New("the code has 6 digits")
	}
	return password.CheckPolicy(r.NewPassword)
}

func (s *Service) registerReset(rt *httpx.Router, ip httpx.ClientIP) {
	s.registerPasswordMail(rt)
	rt.Handle(httpx.Route{Method: http.MethodPost, Path: "/api/v2/auth/password-reset/request", Permission: authz.Public(),
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			if !Secure(r) {
				return errInsecure
			}
			var req resetRequest
			if err := httpx.DecodeJSON(w, r, &req); err != nil {
				return err
			}
			key := "reset|" + ip(r)
			if !s.Throttle.Allow(key) {
				return errLocked
			}
			s.Throttle.Fail(key) // counts requests: at most a few per window per address
			if err := s.RequestReset(r.Context(), req.Login); err != nil {
				return err
			}
			httpx.WriteJSON(w, http.StatusNoContent, nil)
			return nil
		}})
	rt.Handle(httpx.Route{Method: http.MethodPost, Path: "/api/v2/me/password/code", Permission: authz.Authenticated(),
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			if !Secure(r) {
				return errInsecure
			}
			p := authn.FromContext(r.Context())
			httpx.SetTarget(r.Context(), "user:"+p.Username)
			if err := s.RequestOwnCode(r.Context(), p.UserID); err != nil {
				return err
			}
			httpx.WriteJSON(w, http.StatusNoContent, nil)
			return nil
		}})
	rt.Handle(httpx.Route{Method: http.MethodPost, Path: "/api/v2/auth/password-reset/confirm", Permission: authz.Public(),
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			if !Secure(r) {
				return errInsecure
			}
			var req resetConfirm
			if err := httpx.DecodeJSON(w, r, &req); err != nil {
				return err
			}
			id, err := s.ConfirmReset(r.Context(), req.Login, req.Code, req.NewPassword)
			if err != nil {
				return err
			}
			httpx.SetTarget(r.Context(), fmt.Sprintf("user#%d", id))
			httpx.WriteJSON(w, http.StatusNoContent, nil)
			return nil
		}})
}
