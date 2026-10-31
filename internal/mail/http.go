package mail

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/TING-HiuYu/SlurmPortal/internal/apperr"
	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
	"github.com/TING-HiuYu/SlurmPortal/internal/authz"
	"github.com/TING-HiuYu/SlurmPortal/internal/httpx"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
)

// View is the API shape of a provider; the secret is never returned.
type View struct {
	Name      string          `json:"name"`
	Kind      string          `json:"kind"`
	Enabled   bool            `json:"enabled"`
	Default   bool            `json:"default"`
	Config    json.RawMessage `json:"config"`
	SecretSet bool            `json:"secretSet"`
	UpdatedAt string          `json:"updatedAt"`
}

func view(s Stored) View {
	return View{Name: s.Name, Kind: s.Kind, Enabled: s.Enabled, Default: s.Default, Config: s.Config, SecretSet: s.Sealed != "", UpdatedAt: s.UpdatedAt}
}

// Register adds the provider admin API under /api/v2/mail.
func (h *Hub) Register(rt *httpx.Router) {
	read, update := authz.Policy("mail", "read"), authz.Policy("mail", "update")
	for _, r := range []httpx.Route{
		{Method: http.MethodGet, Path: "/api/v2/mail/kinds", Permission: read, Handler: h.httpKinds},
		{Method: http.MethodGet, Path: "/api/v2/mail/providers", Permission: read, Handler: h.httpList},
		{Method: http.MethodGet, Path: "/api/v2/mail/usage", Permission: read, Handler: h.httpUsage},
		{Method: http.MethodPut, Path: "/api/v2/mail/providers/{name}", Permission: update, Handler: h.httpPut},
		{Method: http.MethodDelete, Path: "/api/v2/mail/providers/{name}", Permission: update, Handler: h.httpDelete},
		{Method: http.MethodPost, Path: "/api/v2/mail/providers/{name}/test", Permission: update, Handler: h.httpTest},
	} {
		rt.Handle(r)
	}
}

func (h *Hub) httpKinds(w http.ResponseWriter, _ *http.Request) error {
	type kind struct {
		ID           string        `json:"id"`
		Label        string        `json:"label"`
		Secret       bool          `json:"secret"`
		Placeholders []Placeholder `json:"placeholders,omitempty"`
	}
	out := []kind{}
	for _, k := range h.Kinds() {
		e := kind{ID: k.ID(), Label: k.Label(), Secret: k.SecretPurpose() != ""}
		if p, ok := k.(interface{ Placeholders() []Placeholder }); ok {
			e.Placeholders = p.Placeholders()
		}
		out = append(out, e)
	}
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}

// httpUsage returns the last 30 days of delivery counts per provider and purpose.
func (h *Hub) httpUsage(w http.ResponseWriter, r *http.Request) error {
	rows, err := h.Usage(r.Context(), 30)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, rows)
	return nil
}

func (h *Hub) httpList(w http.ResponseWriter, r *http.Request) error {
	all, err := h.List(r.Context())
	if err != nil {
		return err
	}
	out := make([]View, 0, len(all))
	for _, s := range all {
		out = append(out, view(s))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}

func (h *Hub) httpPut(w http.ResponseWriter, r *http.Request) error {
	var in Input
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		return err
	}
	s, err := h.Save(r.Context(), r.PathValue("name"), in)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, view(s))
	return nil
}

func (h *Hub) httpDelete(w http.ResponseWriter, r *http.Request) error {
	if err := h.Delete(r.Context(), r.PathValue("name")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *Hub) callerMail(ctx context.Context) (string, error) {
	p := authn.FromContext(ctx)
	if p == nil {
		return "", apperr.ErrUnauthenticated
	}
	var mail string
	err := store.QueryRow(ctx, h.db, "SELECT COALESCE(mail, '') FROM users WHERE id = ? AND deleted_at IS NULL", []any{p.UserID}, &mail)
	if errors.Is(err, store.ErrNoRows) {
		return "", apperr.ErrUnauthenticated
	}
	return mail, err
}

// httpTest sends a short message through one provider, enabled or not, to the caller or "to".
func (h *Hub) httpTest(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		To *string `json:"to"`
	}
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		return err
	}
	ctx := r.Context()
	to, err := h.callerMail(ctx)
	if err != nil {
		return err
	}
	if in.To != nil {
		to = *in.To
	}
	if !ValidAddress(to) {
		return invalid("recipient email is invalid")
	}
	s, err := h.load(ctx, r.PathValue("name"))
	if errors.Is(err, ErrNoProvider) {
		return apperr.ErrNotFound
	}
	if err != nil {
		return err
	}
	p, err := h.open(context.WithValue(ctx, depthKey{}, 1), s)
	if err != nil {
		return err
	}
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	err = h.deliver(ctx, s, p, Message{ID: "mail-provider-test/" + hex.EncodeToString(nonce), To: to, Purpose: "provider-test",
		Subject: "go-ldap-admin 邮件服务测试 / mail provider test (" + s.Name + ")",
		Text:    "This test message was sent through mail provider " + s.Name + ".",
		HTML:    "<p>This test message was sent through mail provider <b>" + s.Name + "</b>.</p>"})
	var me *Error
	switch {
	case errors.As(err, &me):
		return apperr.New(http.StatusBadGateway, "mail_failed", me.Kind)
	case Unavailable(err):
		return apperr.New(http.StatusBadGateway, "mail_failed", "no enabled provider accepted the message")
	case err != nil:
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"outcome": "accepted"})
	return nil
}
