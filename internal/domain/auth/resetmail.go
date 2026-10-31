package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/apperr"
	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
	"github.com/TING-HiuYu/SlurmPortal/internal/authz"
	"github.com/TING-HiuYu/SlurmPortal/internal/httpx"
	"github.com/TING-HiuYu/SlurmPortal/internal/mail"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
)

// PasswordMail configures the password e-mail code (集成与通知 → 密码验证码). Disabled, neither the
// login page's 忘记密码 nor the 邮箱验证码 option of 修改密码 is offered.
type PasswordMail struct {
	Enabled bool `json:"enabled"`
	// Provider names the mail hub provider; "" uses the default provider.
	Provider string        `json:"provider"`
	TimeZone string        `json:"timeZone"`
	Template mail.Template `json:"template"`
}

var (
	providerName       = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	errResetDisabled   = apperr.New(http.StatusForbidden, "password_mail_disabled", "password e-mail codes are disabled")
	passwordMailFields = []mail.Variable{
		{Name: "user.username", Description: "用户名"}, {Name: "user.nickname", Description: "昵称（未设置时为用户名）"},
		{Name: "reset.code", Description: "6 位验证码"}, {Name: "reset.minutes", Description: "有效分钟数"},
		{Name: "reset.expires_at_local", Description: "失效时间（配置的时区）"}, {Name: "constants.<名称>", Description: "模板常量"},
	}
)

func defaultPasswordMail(constants map[string]any) PasswordMail {
	return PasswordMail{Enabled: true, TimeZone: mail.DefaultTimeZone,
		Template: mail.Template{Subject: resetSubject, HTML: resetHTML, Constants: constants}}
}

func (c *PasswordMail) validate() error {
	if c.Provider != "" && !providerName.MatchString(c.Provider) {
		return errors.New("invalid mail provider name")
	}
	zone, err := mail.NormalizeTimeZone(c.TimeZone)
	if err != nil {
		return err
	}
	c.TimeZone = zone
	if c.Template.Constants == nil {
		c.Template.Constants = map[string]any{}
	}
	c.Template, err = c.Template.Validate(resetRoots)
	return err
}

// passwordMail loads the settings; before anything is saved the built-in card is used, with the
// organisation constants of the job-notification template.
func (s *Service) passwordMail(ctx context.Context) (PasswordMail, error) {
	var raw string
	err := store.QueryRow(ctx, s.DB, "SELECT config_json FROM v2_password_mail_config WHERE id = 1", nil, &raw)
	if errors.Is(err, store.ErrNoRows) {
		return defaultPasswordMail(s.resetConstants(ctx)), nil
	}
	if err != nil {
		return PasswordMail{}, err
	}
	var c PasswordMail
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return PasswordMail{}, apperr.ErrUnavailable.Wrap(err)
	}
	return c, nil
}

func (s *Service) savePasswordMail(ctx context.Context, c PasswordMail) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(ctx, `INSERT INTO v2_password_mail_config (id, config_json, updated_at) VALUES (1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET config_json = excluded.config_json, updated_at = excluded.updated_at`, string(raw), s.Now().UTC())
	return err
}

// resetContext is the data a code mail is rendered with.
func resetContext(a resetAccount, code string, now time.Time, zone string, constants map[string]any) map[string]any {
	loc, err := time.LoadLocation(zone)
	if err != nil {
		loc, zone = time.UTC, "UTC"
	}
	expires := now.UTC().Add(resetTTL).Format(time.RFC3339)
	return map[string]any{
		"user":      map[string]any{"username": a.username, "nickname": cmpOr(a.nickname, a.username)},
		"reset":     map[string]any{"code": code, "minutes": int(resetTTL / time.Minute), "expires_at_local": mail.LocalTime(expires, loc, zone)},
		"constants": constants,
	}
}

func (s *Service) registerPasswordMail(rt *httpx.Router) {
	read, update := authz.Policy("notification", "read"), authz.Policy("notification", "update")
	// Public: whether the login page and 修改密码 offer e-mail codes.
	rt.Handle(httpx.Route{Method: http.MethodGet, Path: "/api/v2/auth/password-reset", Permission: authz.Public(),
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			c, err := s.passwordMail(r.Context())
			if err != nil {
				return err
			}
			httpx.WriteJSON(w, http.StatusOK, map[string]bool{"enabled": c.Enabled && s.Mailer != nil && len(s.ResetKey) > 0})
			return nil
		}})
	rt.Handle(httpx.Route{Method: http.MethodGet, Path: "/api/v2/password-mail/config", Permission: read,
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			c, err := s.passwordMail(r.Context())
			if err != nil {
				return err
			}
			httpx.WriteJSON(w, http.StatusOK, c)
			return nil
		}})
	rt.Handle(httpx.Route{Method: http.MethodPut, Path: "/api/v2/password-mail/config", Permission: update,
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			var in PasswordMail
			if err := httpx.DecodeJSONLimit(w, r, &in, 256<<10); err != nil {
				return err
			}
			if err := in.validate(); err != nil {
				return apperr.BadRequest(err.Error())
			}
			if hub, ok := s.Mailer.(interface {
				Exists(context.Context, string) (bool, error)
			}); ok && (in.Enabled || in.Provider != "") {
				exists, err := hub.Exists(r.Context(), in.Provider)
				if err != nil {
					return err
				}
				if !exists {
					return apperr.BadRequest("mail provider not found")
				}
			}
			if err := s.savePasswordMail(r.Context(), in); err != nil {
				return err
			}
			httpx.SetTarget(r.Context(), "password-mail")
			httpx.WriteJSON(w, http.StatusOK, in)
			return nil
		}})
	rt.Handle(httpx.Route{Method: http.MethodGet, Path: "/api/v2/password-mail/sample", Permission: read,
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			c, err := s.passwordMail(r.Context())
			if err != nil {
				return err
			}
			data, err := s.exampleResetContext(r, c)
			if err != nil {
				return err
			}
			// Real codes are never kept, so the data is always an example.
			httpx.WriteJSON(w, http.StatusOK, mail.Sample{Source: "example", Context: data, Variables: passwordMailFields})
			return nil
		}})
	rt.Handle(httpx.Route{Method: http.MethodPost, Path: "/api/v2/password-mail/preview", Permission: read,
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			var in struct {
				Template *mail.Template `json:"template"`
				TimeZone *string        `json:"timeZone"`
			}
			if err := httpx.DecodeJSONLimit(w, r, &in, 256<<10); err != nil {
				return err
			}
			c, err := s.passwordMail(r.Context())
			if err != nil {
				return err
			}
			if in.Template != nil {
				c.Template = *in.Template
			}
			if in.TimeZone != nil {
				c.TimeZone = *in.TimeZone
			}
			data, err := s.exampleResetContext(r, c)
			if err != nil {
				return err
			}
			out, err := mail.Render(c.Template, resetRoots, data, passwordMailFields)
			if err != nil {
				return err
			}
			httpx.WriteJSON(w, http.StatusOK, out)
			return nil
		}})
	rt.Handle(httpx.Route{Method: http.MethodPost, Path: "/api/v2/password-mail/test", Permission: update,
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			var in struct {
				To *string `json:"to"`
			}
			if err := httpx.DecodeJSONLimit(w, r, &in, 4096); err != nil {
				return err
			}
			if s.Mailer == nil {
				return apperr.ErrUnavailable
			}
			c, err := s.passwordMail(r.Context())
			if err != nil {
				return err
			}
			p := authn.FromContext(r.Context())
			a, err := s.accountByID(r.Context(), p.UserID)
			if err != nil {
				return err
			}
			to := a.mail
			if in.To != nil {
				to = *in.To
			}
			if !mail.ValidAddress(to) {
				return apperr.BadRequest("recipient email is invalid")
			}
			data, err := s.exampleResetContext(r, c)
			if err != nil {
				return err
			}
			out, err := mail.Render(c.Template, resetRoots, data, nil)
			if err != nil {
				return err
			}
			nonce := make([]byte, 16)
			_, _ = rand.Read(nonce)
			err = s.Mailer.Send(r.Context(), c.Provider, mail.Message{ID: "password-test/" + hex.EncodeToString(nonce), To: to,
				Subject: out.Subject, Text: out.Text, HTML: out.HTML, Purpose: "password-test"})
			var me *mail.Error
			switch {
			case mail.Unavailable(err):
				return apperr.BadRequest("the mail provider is missing or disabled")
			case errors.As(err, &me):
				return apperr.New(http.StatusBadGateway, "smtp_failed", me.Kind)
			case err != nil:
				return err
			}
			httpx.WriteJSON(w, http.StatusOK, map[string]string{"outcome": "smtp_accepted"})
			return nil
		}})
}

// exampleResetContext fills the template with the caller's account and an example code.
func (s *Service) exampleResetContext(r *http.Request, c PasswordMail) (map[string]any, error) {
	p := authn.FromContext(r.Context())
	if p == nil {
		return nil, apperr.ErrUnauthenticated
	}
	a, err := s.accountByID(r.Context(), p.UserID)
	if err != nil {
		return nil, err
	}
	return resetContext(a, "123456", s.Now(), c.TimeZone, c.Template.Constants), nil
}
