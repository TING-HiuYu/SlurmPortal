// Package webaccess is the master side of browser terminal, file and transfer access: administrator
// settings, short-lived gateway tickets, temporary SSH certificates for browser-held keys, gateway
// registration and transfer completion intake. The gateway itself is internal/gateway.
package webaccess

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/apperr"
	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
	"github.com/TING-HiuYu/SlurmPortal/internal/authz"
	"github.com/TING-HiuYu/SlurmPortal/internal/domain/identity"
	"github.com/TING-HiuYu/SlurmPortal/internal/domain/pki"
	"github.com/TING-HiuYu/SlurmPortal/internal/httpx"
	"github.com/TING-HiuYu/SlurmPortal/internal/mail"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugin"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"
)

const (
	ID            = "web-access"
	RegisterPath  = "/sync/v2/web-access/register"
	EventsPath    = "/sync/v2/web-access/events"
	ticketTTL     = 300 * time.Second
	certTTL       = pki.MaxTemporarySSH
	caObservation = 180 * time.Second
)

var (
	sshUsername = regexp.MustCompile(`(?i)^[a-z_][a-z0-9_-]{0,31}\$?$`)
	keyIDPat    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	scopeNames  = []string{"session:create", "terminal", "sftp", "transfer", "transfer:read", "diagnostics"}
)

// Gateway binds one gateway mTLS certificate to its cluster; the fingerprint must also be a sync peer.
type Gateway struct {
	Fingerprint string `yaml:"fingerprint"`
	ClusterID   string `yaml:"cluster-id"`
	GatewayID   string `yaml:"gateway-id"`
	InstanceID  string `yaml:"instance-id"`
}

// MasterConfig is static deployment config. The Ed25519 ticket key is PLUGIN_WEB_ACCESS_TICKET_KEY
// (base64 of the 32-byte seed); without it, ticket issuance answers 503.
type MasterConfig struct {
	TicketKeyID string    `yaml:"ticket-key-id"`
	Gateways    []Gateway `yaml:"gateways"`
}

// Plugin is the web-access plugin.
type Plugin struct{}

func (Plugin) ID() string                  { return ID }
func (Plugin) Roles() []plugin.Role        { return []plugin.Role{plugin.RoleStandalone, plugin.RoleMaster} }
func (Plugin) Close(context.Context) error { return nil }

func (Plugin) Init(_ context.Context, h *plugin.Host) error {
	cfg := MasterConfig{TicketKeyID: "web-access-v1"}
	if err := h.Decode(&cfg); err != nil {
		return err
	}
	if !keyIDPat.MatchString(cfg.TicketKeyID) {
		return errors.New("web-access: invalid ticket-key-id")
	}
	gateways := map[string]Gateway{}
	for _, g := range cfg.Gateways {
		g.Fingerprint = strings.ToLower(g.Fingerprint)
		_, dupGateway := gateways[g.Fingerprint]
		if len(g.Fingerprint) != 64 || dupGateway || !identifier.MatchString(g.ClusterID) || !identifier.MatchString(g.GatewayID) || !identifier.MatchString(g.InstanceID) {
			return fmt.Errorf("web-access: invalid gateway binding %q", g.GatewayID)
		}
		if slices.ContainsFunc(h.Clusters, func(c plugin.Cluster) bool { return c.Fingerprint == g.Fingerprint }) {
			return fmt.Errorf("web-access: gateway %q reuses a slave certificate", g.GatewayID)
		}
		gateways[g.Fingerprint] = g
	}
	s := &service{db: h.Store, identity: h.Identity, pki: h.PKI, gateways: gateways, keyID: cfg.TicketKeyID, now: time.Now,
		mail: h.Mail, names: map[string]string{}}
	for _, g := range gateways {
		s.bindings = append(s.bindings, g)
	}
	slices.SortFunc(s.bindings, func(a, b Gateway) int { return strings.Compare(a.ClusterID, b.ClusterID) })
	for _, c := range h.Clusters {
		s.names[c.ID] = c.Name
	}
	if seed := h.Getenv("PLUGIN_WEB_ACCESS_TICKET_KEY"); seed != "" {
		raw, err := base64.StdEncoding.DecodeString(seed)
		if err != nil || len(raw) != ed25519.SeedSize {
			return errors.New("web-access: PLUGIN_WEB_ACCESS_TICKET_KEY must be base64 of a 32-byte Ed25519 seed")
		}
		s.ticketKey = ed25519.NewKeyFromSeed(raw)
	}
	read, update := authz.Policy(ID, "read"), authz.Policy(ID, "update")
	for _, r := range []httpx.Route{
		{Method: http.MethodGet, Path: "/api/v2/web-access/config", Permission: read, Handler: s.getConfig},
		{Method: http.MethodPut, Path: "/api/v2/web-access/config", Permission: update, Handler: s.putConfig},
		{Method: http.MethodGet, Path: "/api/v2/web-access/keys", Permission: read, Handler: s.keys},
		{Method: http.MethodGet, Path: "/api/v2/web-access/transfer-notification", Permission: read, Handler: s.getTransferEmail},
		{Method: http.MethodPut, Path: "/api/v2/web-access/transfer-notification", Permission: update, Handler: s.putTransferEmail},
		{Method: http.MethodPost, Path: "/api/v2/web-access/transfer-notification/preview", Permission: read, Handler: s.previewTransfer},
		{Method: http.MethodPost, Path: "/api/v2/web-access/transfer-notification/test", Permission: update, Handler: s.testTransfer},
		{Method: http.MethodGet, Path: "/api/v2/web-access/transfer-notification/sample", Permission: read, Handler: s.transferSample},
		{Method: http.MethodGet, Path: "/api/v2/web-access/clusters", Permission: authz.Authenticated(), Handler: s.clusters},
		{Method: http.MethodPost, Path: "/api/v2/web-access/ticket", Permission: authz.Authenticated(), Handler: s.ticket},
		{Method: http.MethodPost, Path: "/api/v2/web-access/ssh-certificate", Permission: authz.Authenticated(), Handler: s.sshCertificate},
		{Method: http.MethodPost, Path: RegisterPath, Permission: authz.Peer(), Handler: s.register, Routine: true},
		{Method: http.MethodPost, Path: EventsPath, Permission: authz.Peer(), Handler: s.events},
	} {
		h.Router.Handle(r)
	}
	return nil
}

type service struct {
	db       store.Store
	identity *identity.Service
	pki      *pki.Service
	gateways map[string]Gateway
	// bindings are the gateways sorted by cluster; names are the configured cluster display names.
	bindings  []Gateway
	names     map[string]string
	keyID     string
	ticketKey ed25519.PrivateKey
	mail      *mail.Hub
	now       func() time.Time
}

var errUnavailable = apperr.New(http.StatusServiceUnavailable, "web_access_unavailable", "web access unavailable")

// AdminCluster is one bound cluster as the settings page shows it: the editable details plus the
// gateway's last registration and the features it serves.
type AdminCluster struct {
	Cluster
	Registration *Registration `json:"registration"`
	Features     Features      `json:"features"`
	// Ready means users can reach it: enabled, gateway URL set and a fresh registration.
	Ready bool `json:"ready"`
}

func (s *service) adminView(r *http.Request, cfg Settings) (map[string]any, error) {
	regs, err := s.registrations(r.Context())
	if err != nil {
		return nil, err
	}
	out := []AdminCluster{}
	for _, c := range s.effective(cfg) {
		a := AdminCluster{Cluster: c}
		if reg, ok := regs[c.GatewayID]; ok {
			a.Registration = &reg
			if reg.Fresh {
				a.Features = featuresOf(reg.Plugins)
			}
		}
		a.Ready = c.Enabled && c.GatewayURL != "" && a.Features.any()
		out = append(out, a)
	}
	return map[string]any{"clusters": out}, nil
}

func (s *service) getConfig(w http.ResponseWriter, r *http.Request) error {
	cfg, err := s.load(r.Context())
	if err != nil {
		return err
	}
	v, err := s.adminView(r, cfg)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, v)
	return nil
}

// putConfig replaces the per-cluster details; features and transfer mail are not set here.
func (s *service) putConfig(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Clusters []Cluster `json:"clusters"`
	}
	if err := httpx.DecodeJSONLimit(w, r, &in, 128<<10); err != nil {
		return err
	}
	cfg, err := s.load(r.Context())
	if err != nil {
		return err
	}
	cfg.Clusters = in.Clusters
	for i := range cfg.Clusters {
		for _, g := range s.bindings {
			if g.ClusterID == cfg.Clusters[i].ID {
				cfg.Clusters[i].GatewayID = g.GatewayID
			}
		}
	}
	if err := s.checkBound(cfg); err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return apperr.BadRequest(err.Error())
	}
	if err := s.save(r.Context(), cfg); err != nil {
		return err
	}
	httpx.SetTarget(r.Context(), "web-access:config")
	v, err := s.adminView(r, cfg)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, v)
	return nil
}

type jwk struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Kid string `json:"kid"`
	Alg string `json:"alg,omitempty"`
	Use string `json:"use,omitempty"`
}

func (s *service) publicKey() (jwk, error) {
	if s.ticketKey == nil {
		return jwk{}, errUnavailable
	}
	pub := s.ticketKey.Public().(ed25519.PublicKey)
	return jwk{Kty: "OKP", Crv: "Ed25519", X: base64.RawURLEncoding.EncodeToString(pub), Kid: s.keyID, Alg: "EdDSA", Use: "sig"}, nil
}

func (s *service) keys(w http.ResponseWriter, _ *http.Request) error {
	k, err := s.publicKey()
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"keys": []jwk{k}})
	return nil
}

type publicCluster struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Address    string   `json:"address"`
	SSHPort    int      `json:"sshPort"`
	GatewayID  string   `json:"gatewayId"`
	GatewayURL string   `json:"gatewayUrl"`
	Features   Features `json:"features"`
}

func (s *service) clusters(w http.ResponseWriter, r *http.Request) error {
	cfg, err := s.load(r.Context())
	if err != nil {
		return err
	}
	live, err := s.userClusters(r.Context(), cfg, authn.FromContext(r.Context()).Roles)
	if err != nil {
		return err
	}
	out := []publicCluster{}
	for _, c := range live {
		out = append(out, publicCluster{c.ID, c.Name, c.Address, c.SSHPort, c.GatewayID, c.GatewayURL, c.Features})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"capabilities": capabilitiesOf(cfg, live), "clusters": out})
	return nil
}

// eligible returns the caller's active account when it may hold an SSH session.
func (s *service) eligible(ctx context.Context) (*authn.Principal, identity.User, error) {
	p := authn.FromContext(ctx)
	u, err := s.identity.GetUser(ctx, p.UserID)
	if err != nil {
		return nil, identity.User{}, err
	}
	if u.Status != 1 || !sshUsername.MatchString(u.Username) || strings.EqualFold(u.Username, "root") {
		return nil, identity.User{}, apperr.New(http.StatusForbidden, "ssh_account_ineligible", "SSH account is not eligible")
	}
	return p, u, nil
}

type ticketRequest struct {
	ClusterID string   `json:"clusterId"`
	Scopes    []string `json:"scopes"`
}

func (t *ticketRequest) Validate() error {
	if len(t.Scopes) == 0 || len(t.Scopes) > len(scopeNames) {
		return errors.New("invalid scopes")
	}
	for _, sc := range t.Scopes {
		if !slices.Contains(scopeNames, sc) {
			return errors.New("invalid scopes")
		}
	}
	slices.Sort(t.Scopes)
	t.Scopes = slices.Compact(t.Scopes)
	return nil
}

func scopeAllowed(scope string, f Features, admin bool) bool {
	switch scope {
	case "session:create":
		return f.any()
	case "terminal":
		return f.Terminal
	case "sftp":
		return f.Files
	case "transfer", "transfer:read":
		return f.Transfer
	case "diagnostics":
		return admin
	}
	return false
}

type ticketClaims struct {
	jwt.RegisteredClaims
	Username              string   `json:"username"`
	ClusterID             string   `json:"cluster_id"`
	Scopes                []string `json:"scopes"`
	DestinationClusterIDs []string `json:"destination_cluster_ids"`
}

// ticket signs a 5-minute EdDSA JWT for one gateway; the gateway verifies it with the published key.
func (s *service) ticket(w http.ResponseWriter, r *http.Request) error {
	var in ticketRequest
	if err := httpx.DecodeJSONLimit(w, r, &in, 2048); err != nil {
		return err
	}
	if s.ticketKey == nil {
		return errUnavailable
	}
	p, u, err := s.eligible(r.Context())
	if err != nil {
		return err
	}
	cfg, err := s.load(r.Context())
	if err != nil {
		return err
	}
	clusters, err := s.userClusters(r.Context(), cfg, p.Roles)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(clusters, func(c liveCluster) bool { return c.ID == in.ClusterID })
	if i < 0 || slices.ContainsFunc(in.Scopes, func(sc string) bool { return !scopeAllowed(sc, clusters[i].Features, p.HasRole("admin")) }) {
		return apperr.New(http.StatusForbidden, "cluster_access_denied", "cluster access denied")
	}
	c := clusters[i]
	dest := make([]string, 0, len(clusters))
	for _, d := range clusters {
		if d.Features.Transfer {
			dest = append(dest, d.ID)
		}
	}
	now := s.now().UTC().Truncate(time.Second)
	exp := now.Add(ticketTTL)
	claims := ticketClaims{RegisteredClaims: jwt.RegisteredClaims{Issuer: "go-ldap-admin", Audience: jwt.ClaimStrings{c.GatewayID},
		Subject: fmt.Sprint(u.ID), ID: uuid.NewString(), IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(exp)},
		Username: u.Username, ClusterID: c.ID, Scopes: in.Scopes, DestinationClusterIDs: dest}
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	tok.Header["kid"] = s.keyID
	signed, err := tok.SignedString(s.ticketKey)
	if err != nil {
		return err
	}
	httpx.SetTarget(r.Context(), "web-access:"+c.ID)
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ticket": signed, "expiresAt": exp, "gatewayUrl": c.GatewayURL, "gatewayId": c.GatewayID, "username": u.Username})
	return nil
}

type certRequest struct {
	ClusterID string `json:"clusterId"`
	Purpose   string `json:"purpose"`
	PublicKey string `json:"publicKey"`
}

func (c *certRequest) Validate() error {
	if c.Purpose != "access" || len(c.PublicKey) > 1024 || !identifier.MatchString(c.ClusterID) {
		return errors.New("only purpose access, a cluster id and an Ed25519 public key are accepted")
	}
	return nil
}

// sshCertificate signs the browser's own Ed25519 key for 5 minutes. The KeyId encodes cluster,
// purpose, user and serial; the gateway rejects any certificate whose KeyId does not match its ticket.
func (s *service) sshCertificate(w http.ResponseWriter, r *http.Request) error {
	var in certRequest
	if err := httpx.DecodeJSONLimit(w, r, &in, 4096); err != nil {
		return err
	}
	key, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(in.PublicKey))
	if err != nil || len(strings.TrimSpace(string(rest))) != 0 || key.Type() != ssh.KeyAlgoED25519 {
		return apperr.BadRequest("public key must be one Ed25519 OpenSSH key")
	}
	p, u, err := s.eligible(r.Context())
	if err != nil {
		return err
	}
	cfg, err := s.load(r.Context())
	if err != nil {
		return err
	}
	live, err := s.userClusters(r.Context(), cfg, p.Roles)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(live, func(c liveCluster) bool { return c.ID == in.ClusterID })
	if i < 0 || !live[i].Features.Terminal && !live[i].Features.Files {
		return apperr.New(http.StatusForbidden, "cluster_access_denied", "cluster access denied")
	}
	c := live[i].Cluster
	caID, err := s.authority(r.Context(), c, u.ID)
	if err != nil {
		return err
	}
	cert, err := s.pki.SignTemporarySSH(r.Context(), caID, key, u.Username, func(serial uint64) string {
		return fmt.Sprintf("web-access/%s/%s/user:%d/%x", c.ID, in.Purpose, u.ID, serial)
	}, certTTL)
	if err != nil {
		return errUnavailable.Wrap(err)
	}
	httpx.SetTarget(r.Context(), "web-access:"+c.ID)
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"certificate": cert.Certificate, "expiresAt": cert.ExpiresAt, "caId": cert.CAID,
		"fingerprint": ssh.FingerprintSHA256(key), "username": u.Username, "serial": fmt.Sprint(cert.Serial)})
	return nil
}

var errNoAuthority = apperr.New(http.StatusServiceUnavailable, "ssh_user_ca_unavailable", "no signing CA matches the login node's trusted user CAs")

// authority picks the pinned CA, else an enabled SSH CA the login node currently trusts (as last
// reported by its gateway), preferring the CA of the user's own certificate.
func (s *service) authority(ctx context.Context, c Cluster, userID int64) (int64, error) {
	if c.SSHCAID > 0 {
		return c.SSHCAID, nil
	}
	var raw string
	err := store.QueryRow(ctx, s.db, "SELECT ca_keys_json FROM v2_gateway_registrations WHERE gateway_id = ? AND cluster_id = ? AND registered_ms > ?",
		[]any{c.GatewayID, c.ID, s.now().Add(-caObservation).UnixMilli()}, &raw)
	if errors.Is(err, store.ErrNoRows) {
		return 0, errNoAuthority
	}
	if err != nil {
		return 0, err
	}
	trusted := map[string]bool{}
	for _, k := range decodeStrings(raw) {
		trusted[k] = true
	}
	cas, err := s.pki.ListCAs(ctx)
	if err != nil {
		return 0, err
	}
	var matches []int64
	for _, ca := range cas {
		if ca.Kind == pki.KindSSH && ca.Enabled && trusted[normalizeKey(ca.CertificatePEM)] {
			matches = append(matches, ca.ID)
		}
	}
	if len(matches) == 0 {
		return 0, errNoAuthority
	}
	if mine, err := s.pki.Mine(ctx, userID); err == nil && slices.Contains(matches, mine.CAID) {
		return mine.CAID, nil
	}
	return matches[0], nil
}

// normalizeKey reduces an authorized_keys line to "type base64", or "" when it is not a CA key.
func normalizeKey(line string) string {
	f := strings.Fields(line)
	if len(f) < 2 {
		return ""
	}
	switch f[0] {
	case ssh.KeyAlgoED25519, ssh.KeyAlgoRSA, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521:
	default:
		return ""
	}
	if _, err := base64.StdEncoding.DecodeString(f[1]); err != nil {
		return ""
	}
	return f[0] + " " + f[1]
}
