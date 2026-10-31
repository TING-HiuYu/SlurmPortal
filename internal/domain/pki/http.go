package pki

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/TING-HiuYu/SlurmPortal/internal/apperr"
	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
	"github.com/TING-HiuYu/SlurmPortal/internal/authz"
	"github.com/TING-HiuYu/SlurmPortal/internal/domain/auth"
	"github.com/TING-HiuYu/SlurmPortal/internal/httpx"
)

var errInsecure = apperr.New(http.StatusForbidden, "insecure_transport", "HTTPS is required for private key material")

type caRequest struct {
	Kind           string `json:"kind"`
	Name           string `json:"name"`
	CertificatePEM string `json:"certificatePem"`
	PublicKey      string `json:"publicKey"`
	PrivateKeyPEM  string `json:"privateKeyPem"`
}

func (r caRequest) Validate() error {
	if r.Kind != KindX509 && r.Kind != KindSSH {
		return errors.New("kind must be x509 or ssh")
	}
	if r.Kind == KindX509 && r.PublicKey != "" || r.Kind == KindSSH && r.CertificatePEM != "" {
		return errors.New("use certificatePem for x509 and publicKey for ssh")
	}
	return nil
}

type caCreated struct {
	CA CA `json:"ca"`
	// PrivateKeyPEM is returned only when the server generated the key, for offline backup.
	PrivateKeyPEM string `json:"privateKeyPem,omitempty"`
}

// Guard reports whether the caller may manage the target user's credentials.
type Guard func(ctx context.Context, userID int64) error

func pathInt(r *http.Request, name string) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id <= 0 {
		return 0, apperr.ErrNotFound
	}
	return id, nil
}

// Register adds the CA and user-certificate routes. guard checks that the caller may act on the user.
func (s *Service) Register(rt *httpx.Router, guard Guard) {
	route := func(method, path string, perm authz.Permission, h httpx.HandlerFunc) {
		rt.Handle(httpx.Route{Method: method, Path: path, Permission: perm, Handler: h})
	}
	reply := func(w http.ResponseWriter, status int, v any) error {
		httpx.WriteJSON(w, status, v)
		return nil
	}

	route(http.MethodGet, "/api/v2/pki/cas", authz.Policy("pki", authz.ActionRead), func(w http.ResponseWriter, r *http.Request) error {
		cas, err := s.ListCAs(r.Context())
		if err != nil {
			return err
		}
		return reply(w, http.StatusOK, cas)
	})
	route(http.MethodPost, "/api/v2/pki/cas", authz.Policy("pki", authz.ActionCreate), func(w http.ResponseWriter, r *http.Request) error {
		if !auth.Secure(r) {
			return errInsecure
		}
		var in caRequest
		if err := httpx.DecodeJSON(w, r, &in); err != nil {
			return err
		}
		httpx.SetTarget(r.Context(), "ca:"+in.Name)
		var (
			out caCreated
			key []byte
			err error
		)
		switch {
		case in.Kind == KindX509 && in.PrivateKeyPEM == "":
			out.CA, key, err = s.GenerateX509(r.Context(), in.Name)
		case in.Kind == KindX509:
			out.CA, err = s.ImportX509(r.Context(), in.Name, []byte(in.CertificatePEM), []byte(in.PrivateKeyPEM))
		case in.PrivateKeyPEM == "":
			out.CA, key, err = s.GenerateSSH(r.Context(), in.Name)
		default:
			out.CA, err = s.ImportSSH(r.Context(), in.Name, []byte(in.PublicKey), []byte(in.PrivateKeyPEM))
		}
		if err != nil {
			return err
		}
		out.PrivateKeyPEM = string(key)
		return reply(w, http.StatusCreated, out)
	})
	route(http.MethodPost, "/api/v2/pki/cas/{id}/revocations", authz.Policy("pki", authz.ActionDelete), func(w http.ResponseWriter, r *http.Request) error {
		id, err := pathInt(r, "id")
		if err != nil {
			return err
		}
		var in struct {
			Serial string `json:"serial"`
			Reason int    `json:"reason"`
		}
		if err := httpx.DecodeJSON(w, r, &in); err != nil {
			return err
		}
		httpx.SetTarget(r.Context(), "ca:"+r.PathValue("id")+"/serial:"+in.Serial)
		if err := s.Revoke(r.Context(), id, in.Serial, in.Reason); err != nil {
			return err
		}
		return reply(w, http.StatusNoContent, nil)
	})

	route(http.MethodPost, "/api/v2/users/{id}/certificate", authz.Policy("pki", authz.ActionCreate), func(w http.ResponseWriter, r *http.Request) error {
		if !auth.Secure(r) {
			return errInsecure
		}
		id, err := pathInt(r, "id")
		if err != nil {
			return err
		}
		httpx.SetTarget(r.Context(), "user#"+r.PathValue("id"))
		if err := guard(r.Context(), id); err != nil {
			return err
		}
		var in struct {
			CAID int64 `json:"caId"`
		}
		if err := httpx.DecodeJSON(w, r, &in); err != nil {
			return err
		}
		issued, err := s.Assign(r.Context(), id, in.CAID)
		if err != nil {
			return err
		}
		return reply(w, http.StatusCreated, issued)
	})
	route(http.MethodPost, "/api/v2/users/{id}/certificate/rotate", authz.Policy("pki", authz.ActionUpdate), func(w http.ResponseWriter, r *http.Request) error {
		if !auth.Secure(r) {
			return errInsecure
		}
		id, err := pathInt(r, "id")
		if err != nil {
			return err
		}
		httpx.SetTarget(r.Context(), "user#"+r.PathValue("id"))
		if err := guard(r.Context(), id); err != nil {
			return err
		}
		issued, err := s.Rotate(r.Context(), id)
		if err != nil {
			return err
		}
		return reply(w, http.StatusOK, issued)
	})
	route(http.MethodDelete, "/api/v2/users/{id}/certificate", authz.Policy("pki", authz.ActionDelete), func(w http.ResponseWriter, r *http.Request) error {
		id, err := pathInt(r, "id")
		if err != nil {
			return err
		}
		httpx.SetTarget(r.Context(), "user#"+r.PathValue("id"))
		if err := guard(r.Context(), id); err != nil {
			return err
		}
		if err := s.RevokeUser(r.Context(), id); err != nil {
			return err
		}
		return reply(w, http.StatusNoContent, nil)
	})

	route(http.MethodGet, "/api/v2/me/certificate", authz.Authenticated(), func(w http.ResponseWriter, r *http.Request) error {
		c, err := s.Mine(r.Context(), authn.FromContext(r.Context()).UserID)
		if err != nil {
			return err
		}
		return reply(w, http.StatusOK, c)
	})
	route(http.MethodPost, "/api/v2/me/certificate", authz.Authenticated(), func(w http.ResponseWriter, r *http.Request) error {
		if !auth.Secure(r) {
			return errInsecure
		}
		issued, err := s.IssueMine(r.Context(), authn.FromContext(r.Context()).UserID)
		if err != nil {
			return err
		}
		return reply(w, http.StatusOK, issued)
	})
}
