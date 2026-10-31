package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/apperr"
	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
	"github.com/TING-HiuYu/SlurmPortal/internal/domain/identity"
	"github.com/TING-HiuYu/SlurmPortal/internal/secretbox"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
)

const secretPurpose = "oauth-client-secret"

var envRef = regexp.MustCompile(`^env:[A-Z_][A-Z0-9_]{0,63}$`)

// ClientConfig is the provider-facing part stored in oauth_connectors.config.
type ClientConfig struct {
	ClientID    string `json:"clientId"`
	RedirectURI string `json:"redirectUri"`
	// ClientSecret is "env:NAME", a sealed value, or (legacy rows only) plaintext. Never returned.
	ClientSecret string `json:"clientSecret,omitempty"`
	// Rules filter and place users; rows written before rules existed have none (see defaultRules).
	Rules []Rule `json:"rules,omitempty"`
}

// Connector is one configured sign-in app of a provider, with its sign-in rules.
type Connector struct {
	ID             int64   `json:"id"`
	Name           string  `json:"name"`
	Provider       string  `json:"provider"`
	CAID           int64   `json:"caId"`
	Enabled        bool    `json:"enabled"`
	DefaultRoleIDs []int64 `json:"defaultRoleIds"`
	DefaultStatus  int     `json:"defaultStatus"`
	GroupID        int64   `json:"groupId"`
	Description    string  `json:"description"`
	Creator        string  `json:"creator"`
	ClientID       string  `json:"clientId"`
	RedirectURI    string  `json:"redirectUri"`
	SecretEnv      string  `json:"clientSecretEnv,omitempty"`
	SecretSet      bool    `json:"clientSecretSet"`
	Rules          []Rule  `json:"rules"`

	secret string
}

// ConnectorInput is the body for creating or updating a connector.
type ConnectorInput struct {
	Name           string  `json:"name"`
	Provider       string  `json:"provider"`
	CAID           int64   `json:"caId"`
	Enabled        bool    `json:"enabled"`
	DefaultRoleIDs []int64 `json:"defaultRoleIds"`
	DefaultStatus  int     `json:"defaultStatus"`
	GroupID        int64   `json:"groupId"`
	Description    string  `json:"description"`
	ClientID       string  `json:"clientId"`
	RedirectURI    string  `json:"redirectUri"`
	// ClientSecret: "env:NAME" stores a reference, any other value is sealed; omitted keeps the current one.
	ClientSecret *string `json:"clientSecret"`
	// Rules replace GroupID; omitted means one match-anyone rule into GroupID.
	Rules []Rule `json:"rules"`
	// Read-only fields a client may send back from a GET.
	ID        *int64  `json:"id"`
	Creator   *string `json:"creator"`
	SecretEnv *string `json:"clientSecretEnv"`
	SecretSet *bool   `json:"clientSecretSet"`
}

// Validate checks the connector settings and its rules.
func (in *ConnectorInput) Validate() error {
	in.Name, in.Provider, in.ClientID = strings.TrimSpace(in.Name), strings.ToLower(strings.TrimSpace(in.Provider)), strings.TrimSpace(in.ClientID)
	if in.DefaultStatus == 0 {
		in.DefaultStatus = identity.StatusActive
	}
	switch {
	case in.Name == "" || len([]rune(in.Name)) > 100:
		return apperr.BadRequest("name must be 1-100 characters")
	case providers[in.Provider] == nil:
		return apperr.BadRequest("unsupported provider")
	case in.CAID <= 0:
		return apperr.BadRequest("caId is required")
	case in.GroupID < 0:
		return apperr.BadRequest("groupId must not be negative")
	case in.ClientID == "" || len(in.ClientID) > 200:
		return apperr.BadRequest("clientId must be 1-200 characters")
	case len([]rune(in.Description)) > 255:
		return apperr.BadRequest("description is too long")
	}
	u, err := url.Parse(in.RedirectURI)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "/api/v2/oauth/callback/"+in.Provider ||
		!(u.Scheme == "https" || u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1")) {
		return apperr.BadRequest("redirectUri must be https://<host>/api/v2/oauth/callback/" + in.Provider)
	}
	if in.ClientSecret != nil {
		s := *in.ClientSecret
		if s == "" || len(s) > 512 || strings.HasPrefix(s, "env:") && !envRef.MatchString(s) {
			return apperr.BadRequest("clientSecret must be a secret or env:NAME")
		}
	}
	if in.Rules == nil {
		in.Rules = defaultRules(in.GroupID)
	}
	rules, err := validateRules(in.Rules)
	if err != nil {
		return err
	}
	// Groups now live on rules; the legacy single group column is cleared on write.
	in.Rules, in.GroupID = rules, 0
	return nil
}

type connectors struct {
	db       store.Store
	identity *identity.Service
	secrets  *secretbox.Box
	getenv   func(string) string
	now      func() time.Time
}

var errNotFound = apperr.ErrNotFound

const connectorCols = `id, name, provider, certificate_authority_id, enabled, COALESCE(default_role_ids,'[]'), COALESCE(default_status,1),
	department_id, COALESCE(default_login_shell,''), COALESCE(config,'{}'), COALESCE(description,''), COALESCE(creator,'')`

func (c *connectors) list(ctx context.Context, onlyEnabled bool) ([]Connector, error) {
	rows, err := c.db.Query(ctx, "SELECT "+connectorCols+" FROM oauth_connectors WHERE deleted_at IS NULL ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Connector{}
	for rows.Next() {
		k, err := scanConnector(rows)
		if err != nil {
			return nil, err
		}
		if !onlyEnabled || k.Enabled {
			out = append(out, k)
		}
	}
	return out, rows.Err()
}

func (c *connectors) get(ctx context.Context, id int64) (Connector, error) {
	rows, err := c.db.Query(ctx, "SELECT "+connectorCols+" FROM oauth_connectors WHERE id = ? AND deleted_at IS NULL", id)
	if err != nil {
		return Connector{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return Connector{}, err
		}
		return Connector{}, errNotFound
	}
	return scanConnector(rows)
}

func scanConnector(rows store.Rows) (Connector, error) {
	var k Connector
	var enabled any
	var roles, cfg, shell string
	if err := rows.Scan(&k.ID, &k.Name, &k.Provider, &k.CAID, &enabled, &roles, &k.DefaultStatus, &k.GroupID, &shell, &cfg, &k.Description, &k.Creator); err != nil {
		return k, err
	}
	k.Enabled = truthy(enabled)
	if err := json.Unmarshal([]byte(roles), &k.DefaultRoleIDs); err != nil {
		return k, errors.New("oauth connector default_role_ids is corrupt")
	}
	var cc ClientConfig
	if err := json.Unmarshal([]byte(cfg), &cc); err != nil {
		return k, errors.New("oauth connector config is corrupt")
	}
	k.ClientID, k.RedirectURI, k.secret, k.Rules = cc.ClientID, cc.RedirectURI, cc.ClientSecret, cc.Rules
	if len(k.Rules) == 0 {
		k.Rules = defaultRules(k.GroupID)
	}
	k.SecretSet = cc.ClientSecret != ""
	if strings.HasPrefix(cc.ClientSecret, "env:") {
		k.SecretEnv = strings.TrimPrefix(cc.ClientSecret, "env:")
	}
	if k.DefaultRoleIDs == nil {
		k.DefaultRoleIDs = []int64{}
	}
	return k, nil
}

func truthy(v any) bool {
	switch x := v.(type) {
	case int64:
		return x != 0
	case float64:
		return x != 0
	case bool:
		return x
	case string:
		return x == "1" || strings.EqualFold(x, "true")
	case []byte:
		return string(x) == "1" || strings.EqualFold(string(x), "true")
	}
	return false
}

// provisioning is how an account created by rule r is set up; its home and shell come from the
// rule's main group.
func (k Connector) provisioning(r Rule) identity.Provisioning {
	return identity.Provisioning{RoleIDs: k.DefaultRoleIDs, MainGroupID: r.MainGroupID, GroupIDs: r.GroupIDs, Status: k.DefaultStatus}
}

// clientSecret resolves the stored reference at use time.
func (c *connectors) clientSecret(k Connector) (string, error) {
	switch {
	case strings.HasPrefix(k.secret, "env:"):
		if v := c.getenv(k.SecretEnv); v != "" {
			return v, nil
		}
		return "", errors.New("oauth client secret env " + k.SecretEnv + " is empty")
	case secretbox.IsSealed(k.secret):
		b, err := c.secrets.Open(secretPurpose, k.secret)
		return string(b), err
	case k.secret == "":
		return "", errors.New("oauth client secret is not configured")
	}
	return k.secret, nil
}

func (c *connectors) checkRefs(ctx context.Context, in *ConnectorInput) error {
	var kind string
	if err := store.QueryRow(ctx, c.db, "SELECT COALESCE(kind,'x509') FROM certificate_authorities WHERE id = ? AND deleted_at IS NULL", []any{in.CAID}, &kind); err != nil {
		if errors.Is(err, store.ErrNoRows) {
			return apperr.BadRequest("unknown caId")
		}
		return err
	}
	if kind != "ssh" {
		return apperr.BadRequest("caId must reference an SSH CA")
	}
	// Each rule has one main group, a POSIX group that becomes the account's gidNumber and supplies
	// its home and shell presets; the other groups a rule names must be non-POSIX (v1: one main group
	// per account). A rule saved before main groups existed has its single POSIX group promoted.
	for i := range in.Rules {
		r := &in.Rules[i]
		groups, err := c.identity.GroupsByIDs(ctx, r.GroupIDs)
		if err != nil {
			return err
		}
		extra := []int64{}
		for _, g := range groups {
			switch {
			case !g.IsPOSIX():
				extra = append(extra, g.ID)
			case r.MainGroupID == 0:
				r.MainGroupID = g.ID
			default:
				return apperr.BadRequest("rule " + ruleLabel(*r, i) + ": other groups must not be POSIX groups; an account has one main group")
			}
		}
		if r.MainGroupID == 0 {
			return apperr.BadRequest("rule " + ruleLabel(*r, i) + " needs a main group")
		}
		main, err := c.identity.GroupsByIDs(ctx, []int64{r.MainGroupID})
		if err != nil {
			return err
		}
		if !main[0].IsPOSIX() || main[0].GIDNumber == 0 {
			return apperr.BadRequest("rule " + ruleLabel(*r, i) + ": the main group must be a POSIX group")
		}
		r.GroupIDs = extra
	}
	// Every rule's groups must be within the configurer's reach.
	k := Connector{DefaultRoleIDs: in.DefaultRoleIDs, DefaultStatus: in.DefaultStatus}
	for _, r := range in.Rules {
		if err := c.identity.CheckProvisioning(ctx, k.provisioning(r)); err != nil {
			return err
		}
	}
	return nil
}

func (c *connectors) encodeConfig(in ConnectorInput, current string) (string, string, error) {
	cc := ClientConfig{ClientID: in.ClientID, RedirectURI: in.RedirectURI, ClientSecret: current, Rules: in.Rules}
	if in.ClientSecret != nil {
		if strings.HasPrefix(*in.ClientSecret, "env:") {
			cc.ClientSecret = *in.ClientSecret
		} else {
			sealed, err := c.secrets.Seal(secretPurpose, []byte(*in.ClientSecret))
			if errors.Is(err, secretbox.ErrNoKey) {
				return "", "", apperr.BadRequest("without PKI_MASTER_KEY the client secret must be env:NAME")
			}
			if err != nil {
				return "", "", err
			}
			cc.ClientSecret = sealed
		}
	}
	if cc.ClientSecret == "" {
		return "", "", apperr.BadRequest("clientSecret is required")
	}
	raw, err := json.Marshal(cc)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(raw)
	return string(raw), hex.EncodeToString(sum[:]), nil
}

func (c *connectors) create(ctx context.Context, in ConnectorInput) (Connector, error) {
	if err := c.checkRefs(ctx, &in); err != nil {
		return Connector{}, err
	}
	cfg, hash, err := c.encodeConfig(in, "")
	if err != nil {
		return Connector{}, err
	}
	roles, _ := json.Marshal(nonNil(in.DefaultRoleIDs))
	now := c.now().UTC()
	res, err := c.db.Exec(ctx, `INSERT INTO oauth_connectors (created_at, updated_at, name, provider, certificate_authority_id, enabled,
		default_role_ids, default_status, department_id, default_login_shell, config, config_hash, description, creator)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		now, now, in.Name, in.Provider, in.CAID, in.Enabled, string(roles), in.DefaultStatus, in.GroupID, "", cfg, hash, in.Description, creator(ctx))
	if err != nil {
		return Connector{}, err
	}
	return c.get(ctx, res.LastInsertID)
}

func (c *connectors) update(ctx context.Context, id int64, in ConnectorInput) (Connector, error) {
	cur, err := c.get(ctx, id)
	if err != nil {
		return Connector{}, err
	}
	if cur.Provider != in.Provider {
		return Connector{}, apperr.BadRequest("provider cannot change; create a new connector")
	}
	if err := c.checkRefs(ctx, &in); err != nil {
		return Connector{}, err
	}
	cfg, hash, err := c.encodeConfig(in, cur.secret)
	if err != nil {
		return Connector{}, err
	}
	roles, _ := json.Marshal(nonNil(in.DefaultRoleIDs))
	_, err = c.db.Exec(ctx, `UPDATE oauth_connectors SET updated_at = ?, name = ?, certificate_authority_id = ?, enabled = ?, default_role_ids = ?,
		default_status = ?, department_id = ?, default_login_shell = ?, config = ?, config_hash = ?, description = ? WHERE id = ? AND deleted_at IS NULL`,
		c.now().UTC(), in.Name, in.CAID, in.Enabled, string(roles), in.DefaultStatus, in.GroupID, "", cfg, hash, in.Description, id)
	if err != nil {
		return Connector{}, err
	}
	return c.get(ctx, id)
}

func (c *connectors) delete(ctx context.Context, id int64) error {
	res, err := c.db.Exec(ctx, "UPDATE oauth_connectors SET deleted_at = ?, enabled = ? WHERE id = ? AND deleted_at IS NULL", c.now().UTC(), false, id)
	if err != nil {
		return err
	}
	if res.RowsAffected == 0 {
		return errNotFound
	}
	return nil
}

func nonNil(ids []int64) []int64 {
	if ids == nil {
		return []int64{}
	}
	return ids
}

func creator(ctx context.Context) string {
	if p := authn.FromContext(ctx); p != nil {
		return p.Username
	}
	return ""
}

var errCAMismatch = apperr.New(http.StatusConflict, "certificate_ca_mismatch", "the account already holds a certificate from another CA")
