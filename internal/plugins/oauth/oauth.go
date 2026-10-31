// Package oauth is the OAuth login plugin: admin-managed connectors, a popup authorization-code flow
// bound to a first-party cookie, and account linking/provisioning by verified email.
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/apperr"
	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
	"github.com/TING-HiuYu/SlurmPortal/internal/authz"
	"github.com/TING-HiuYu/SlurmPortal/internal/domain/auth"
	"github.com/TING-HiuYu/SlurmPortal/internal/domain/identity"
	"github.com/TING-HiuYu/SlurmPortal/internal/httpx"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugin"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
)

const (
	ID         = "oauth"
	stateTTL   = 10 * time.Minute
	cookieName = "__Host-gla-oauth"
)

// Config holds the plugin settings that are not stored per connector.
type Config struct {
	// FrontendOrigin receives the result via postMessage; defaults to the first CORS origin, else same origin.
	FrontendOrigin string `yaml:"frontend-origin"`
}

// Plugin is the oauth login plugin.
type Plugin struct{}

func (Plugin) ID() string                  { return ID }
func (Plugin) Roles() []plugin.Role        { return []plugin.Role{plugin.RoleStandalone, plugin.RoleMaster} }
func (Plugin) Close(context.Context) error { return nil }
func (Plugin) Init(ctx context.Context, h *plugin.Host) error {
	var cfg Config
	if err := h.Decode(&cfg); err != nil {
		return err
	}
	if cfg.FrontendOrigin == "" && len(h.Origins) > 0 {
		cfg.FrontendOrigin = h.Origins[0]
	}
	if cfg.FrontendOrigin != "" {
		u, err := url.Parse(cfg.FrontendOrigin)
		if err != nil || u.Host == "" || u.Path != "" || (u.Scheme != "https" && u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1") {
			return errors.New("oauth: frontend-origin must be an https origin")
		}
	}
	f := &flow{cfg: cfg, tokens: h.Tokens, http: newHTTPClient(),
		c: &connectors{db: h.Store, identity: h.Identity, secrets: h.Secrets, getenv: h.Getenv, now: time.Now}}
	f.register(h.Router)
	return nil
}

type flow struct {
	cfg    Config
	c      *connectors
	tokens *authn.JWT
	http   *http.Client
}

func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, apperr.BadRequest("invalid id")
	}
	return id, nil
}

func (f *flow) register(rt *httpx.Router) {
	const base = "/api/v2/oauth/connectors"
	type public struct {
		ID       int64  `json:"id"`
		Name     string `json:"name"`
		Provider string `json:"provider"`
	}
	rt.Handle(httpx.Route{Method: http.MethodGet, Path: base + "/public", Permission: authz.Public(),
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			ks, err := f.c.list(r.Context(), true)
			if err != nil {
				return err
			}
			out := make([]public, 0, len(ks))
			for _, k := range ks {
				out = append(out, public{k.ID, k.Name, k.Provider})
			}
			httpx.WriteJSON(w, http.StatusOK, out)
			return nil
		}})
	rt.Handle(httpx.Route{Method: http.MethodGet, Path: base, Permission: authz.Policy("oauth", "read"),
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			ks, err := f.c.list(r.Context(), false)
			if err != nil {
				return err
			}
			httpx.WriteJSON(w, http.StatusOK, ks)
			return nil
		}})
	rt.Handle(httpx.Route{Method: http.MethodPost, Path: base, Permission: authz.Policy("oauth", "create"),
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			var in ConnectorInput
			if err := httpx.DecodeJSON(w, r, &in); err != nil {
				return err
			}
			k, err := f.c.create(r.Context(), in)
			if err != nil {
				return err
			}
			httpx.SetTarget(r.Context(), "oauth-connector:"+strconv.FormatInt(k.ID, 10))
			httpx.WriteJSON(w, http.StatusCreated, k)
			return nil
		}})
	rt.Handle(httpx.Route{Method: http.MethodPut, Path: base + "/{id}", Permission: authz.Policy("oauth", "update"),
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			id, err := pathID(r)
			if err != nil {
				return err
			}
			httpx.SetTarget(r.Context(), "oauth-connector:"+strconv.FormatInt(id, 10))
			var in ConnectorInput
			if err := httpx.DecodeJSON(w, r, &in); err != nil {
				return err
			}
			k, err := f.c.update(r.Context(), id, in)
			if err != nil {
				return err
			}
			httpx.WriteJSON(w, http.StatusOK, k)
			return nil
		}})
	rt.Handle(httpx.Route{Method: http.MethodDelete, Path: base + "/{id}", Permission: authz.Policy("oauth", "delete"),
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			id, err := pathID(r)
			if err != nil {
				return err
			}
			httpx.SetTarget(r.Context(), "oauth-connector:"+strconv.FormatInt(id, 10))
			if err := f.c.delete(r.Context(), id); err != nil {
				return err
			}
			httpx.WriteJSON(w, http.StatusNoContent, nil)
			return nil
		}})
	rt.Handle(httpx.Route{Method: http.MethodGet, Path: "/api/v2/oauth/authorize/{id}", Permission: authz.Public(), Handler: f.authorize})
	rt.Handle(httpx.Route{Method: http.MethodGet, Path: "/api/v2/oauth/callback/{provider}", Permission: authz.Public(), Handler: f.callback})
}

func hashState(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// authorize runs inside the login popup: it binds a fresh state to this browser and redirects to the IdP.
func (f *flow) authorize(w http.ResponseWriter, r *http.Request) error {
	if !auth.Secure(r) {
		return apperr.New(http.StatusForbidden, "insecure_transport", "OAuth requires HTTPS")
	}
	id, err := pathID(r)
	if err != nil {
		return err
	}
	k, err := f.c.get(r.Context(), id)
	if err != nil {
		return err
	}
	if !k.Enabled {
		return errNotFound
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return err
	}
	state := base64.RawURLEncoding.EncodeToString(raw[:])
	now := f.c.now().UTC()
	if _, err := f.c.db.Batch(r.Context(), []store.Statement{
		{Query: "DELETE FROM oauth_states WHERE expires_at < ?", Args: []any{now}},
		{Query: "INSERT INTO oauth_states (state_hash, connector_id, expires_at) VALUES (?, ?, ?)", Args: []any{hashState(state), k.ID, now.Add(stateTTL)}},
	}); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: state, Path: "/", MaxAge: int(stateTTL.Seconds()),
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, providers[k.Provider].AuthorizeURL(ClientConfig{ClientID: k.ClientID, RedirectURI: k.RedirectURI, Rules: k.Rules}, state), http.StatusFound)
	return nil
}

type result struct {
	Type            string    `json:"type"`
	Token           string    `json:"token,omitempty"`
	ExpiresAt       time.Time `json:"expiresAt,omitzero"`
	InitialPassword string    `json:"initialPassword,omitempty"`
	Error           *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

var page = template.Must(template.New("cb").Parse(`<!doctype html><html><head><meta charset="utf-8"><title>Sign in</title></head>` +
	`<body><p id="m">{{.Fallback}}</p><script nonce="{{.Nonce}}">(function(){var d={{.Data}},o={{.Origin}};` +
	`if(window.opener){window.opener.postMessage(d,o);window.close();}})();</script></body></html>`))

func (f *flow) callback(w http.ResponseWriter, r *http.Request) error {
	httpx.MarkAudited(r.Context())
	res := result{Type: "go-ldap-admin:oauth"}
	status := http.StatusOK
	if err := f.complete(r, &res); err != nil {
		var ae *apperr.Error
		if !errors.As(err, &ae) {
			slog.ErrorContext(r.Context(), "oauth callback failed", "err", err)
			ae = apperr.New(http.StatusBadGateway, "oauth_failed", "sign-in with the identity provider failed")
		}
		res.Token, res.InitialPassword = "", ""
		res.Error = &struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}{ae.Code, ae.Message}
		status = ae.Status
	}
	origin := f.cfg.FrontendOrigin
	if origin == "" {
		scheme := "https"
		if r.TLS == nil {
			scheme = "http"
		}
		origin = scheme + "://" + r.Host
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	n := base64.RawStdEncoding.EncodeToString(nonce[:])
	data, err := json.Marshal(res)
	if err != nil {
		return err
	}
	fallback := "Signed in. You can close this window."
	if res.Error != nil {
		fallback = res.Error.Message
	}
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'nonce-"+n+"'; frame-ancestors 'none'")
	h.Set("Content-Type", "text/html; charset=utf-8")
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	w.WriteHeader(status)
	return page.Execute(w, map[string]any{"Nonce": n, "Data": template.JS(data), "Origin": origin, "Fallback": fallback})
}

var errNotPermitted = apperr.New(http.StatusForbidden, "not_permitted", "this account is not allowed to sign in with this method")

var errState = apperr.New(http.StatusBadRequest, "invalid_state", "sign-in session expired or was not started in this browser")

func (f *flow) complete(r *http.Request, res *result) error {
	ctx := r.Context()
	if !auth.Secure(r) {
		return apperr.New(http.StatusForbidden, "insecure_transport", "OAuth requires HTTPS")
	}
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		return apperr.New(http.StatusForbidden, "provider_denied", "the identity provider did not authorize the sign-in")
	}
	state, code := q.Get("state"), q.Get("code")
	ck, err := r.Cookie(cookieName)
	if state == "" || code == "" || len(code) > 512 || err != nil || subtle.ConstantTimeCompare([]byte(ck.Value), []byte(state)) != 1 {
		return errState
	}
	now := f.c.now().UTC()
	var connectorID int64
	err = store.QueryRow(ctx, f.c.db, "SELECT connector_id FROM oauth_states WHERE state_hash = ? AND consumed_at IS NULL AND expires_at > ?",
		[]any{hashState(state), now}, &connectorID)
	if errors.Is(err, store.ErrNoRows) {
		return errState
	}
	if err != nil {
		return err
	}
	// Single use: only the request that flips consumed_at may continue.
	used, err := f.c.db.Exec(ctx, "UPDATE oauth_states SET consumed_at = ? WHERE state_hash = ? AND consumed_at IS NULL", now, hashState(state))
	if err != nil {
		return err
	}
	if used.RowsAffected != 1 {
		return errState
	}
	k, err := f.c.get(ctx, connectorID)
	if err != nil || !k.Enabled || k.Provider != r.PathValue("provider") {
		return errState
	}
	var caEnabled any
	if err := store.QueryRow(ctx, f.c.db, "SELECT enabled FROM certificate_authorities WHERE id = ? AND deleted_at IS NULL", []any{k.CAID}, &caEnabled); err != nil || !truthy(caEnabled) {
		return apperr.New(http.StatusConflict, "connector_unavailable", "this sign-in method's certificate authority is disabled")
	}
	secret, err := f.c.clientSecret(k)
	if err != nil {
		return err
	}
	prof, err := providers[k.Provider].Exchange(ctx, f.http, ClientConfig{ClientID: k.ClientID, RedirectURI: k.RedirectURI, Rules: k.Rules}, secret, code)
	if err != nil {
		return err
	}
	rule, ok := matchRule(k.Rules, prof)
	if !ok {
		return errNotPermitted
	}
	if existing, err := f.c.identity.FindByEmail(ctx, prof.Email); err == nil {
		var caID int64
		err := store.QueryRow(ctx, f.c.db, "SELECT certificate_authority_id FROM user_certificates WHERE user_id = ? AND deleted_at IS NULL", []any{existing}, &caID)
		if err == nil && caID != k.CAID {
			return errCAMismatch
		}
		if err != nil && !errors.Is(err, store.ErrNoRows) {
			return err
		}
	} else if !errors.Is(err, identity.ErrNoAccount) {
		return err
	}
	prov := k.provisioning(rule)
	id, initial, err := f.c.identity.LinkOrProvision(ctx, identity.ExternalProfile{
		Email: prof.Email, UsernameHint: prof.Login, Name: prof.Name, Avatar: prof.Avatar,
		Source: "oauth-" + k.Provider + "-" + strconv.FormatInt(k.ID, 10), SubjectID: prof.SubjectID,
	}, &prov)
	if err != nil {
		return err
	}
	httpx.SetTarget(ctx, "user:"+strconv.FormatInt(id, 10))
	if _, err := f.tokens.Directory.LoadPrincipal(ctx, id); err != nil {
		return apperr.New(http.StatusForbidden, "account_pending", "account exists but is not enabled; contact an administrator")
	}
	tok, _, exp, err := f.tokens.Issue(id, authn.SourceOAuth)
	if err != nil {
		return err
	}
	res.Token, res.ExpiresAt, res.InitialPassword = tok, exp, initial
	return nil
}
