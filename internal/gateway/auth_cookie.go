package gateway

import (
	"net/http"
	"strings"
	"time"
)

const gatewayCookieName = "__Host-hpc_gateway_jwt"

func (g *Gateway) authSession(w http.ResponseWriter, r *http.Request) {
	if r.Method == "DELETE" {
		if _, e := g.auth(r, ""); e != nil {
			fail(w, 403, "ticket_invalid")
			return
		}
		http.SetCookie(w, &http.Cookie{Name: gatewayCookieName, Value: "", Path: "/", Expires: time.Unix(1, 0).UTC(), MaxAge: -1, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		reply(w, 200, map[string]bool{"authenticated": false})
		return
	}
	if r.Method != "POST" {
		w.Header().Set("Allow", "POST, DELETE")
		fail(w, 405, "method_not_allowed")
		return
	}
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		fail(w, 403, "ticket_invalid")
		return
	}
	raw := strings.TrimPrefix(header, "Bearer ")
	claims, e := g.cfg.VerifyTicket(raw, "", time.Now())
	if e != nil {
		fail(w, 403, "ticket_invalid")
		return
	}
	var input struct{}
	if body(r, &input) != nil {
		fail(w, 400, "request_invalid")
		return
	}
	if len(raw) > 3800 {
		fail(w, 400, "ticket_cookie_too_large")
		return
	}
	seconds := int(time.Until(claims.ExpiresAt.Time).Seconds())
	if seconds < 1 {
		fail(w, 403, "ticket_invalid")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: gatewayCookieName, Value: raw, Path: "/", Expires: claims.ExpiresAt.Time, MaxAge: seconds, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	reply(w, 200, map[string]any{"authenticated": true, "expires_at": claims.ExpiresAt.Time.UTC().Format(time.RFC3339)})
}
