package gateway

import (
	"crypto/ed25519"
	"errors"
	"github.com/golang-jwt/jwt/v5"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Claims are the fields of a gateway ticket: who, on which cluster, and which scopes.
type Claims struct {
	jwt.RegisteredClaims
	Username              string   `json:"username"`
	ClusterID             string   `json:"cluster_id"`
	Scopes                []string `json:"scopes"`
	DestinationClusterIDs []string `json:"destination_cluster_ids"`
}

var userIDPattern = regexp.MustCompile(`^[1-9][0-9]{0,17}$`)

// VerifyTicket checks a ticket's signature against master's keys, its expiry and that it grants scope.
func (c Config) VerifyTicket(raw, scope string, now time.Time) (Claims, error) {
	var claims Claims
	if len(raw) == 0 || len(raw) > 8192 {
		return claims, errors.New("invalid ticket")
	}
	token, e := jwt.ParseWithClaims(raw, &claims, func(t *jwt.Token) (any, error) {
		if t.Method != jwt.SigningMethodEdDSA {
			return nil, errors.New("invalid algorithm")
		}
		kid, ok := t.Header["kid"].(string)
		key, found := c.parsedKeys[kid]
		if !ok || !found || len(key) != ed25519.PublicKeySize {
			return nil, errors.New("unknown key")
		}
		for k := range t.Header {
			if k != "kid" && k != "alg" && k != "typ" {
				return nil, errors.New("unsupported header")
			}
		}
		if t.Header["typ"] != "JWT" {
			return nil, errors.New("invalid type")
		}
		return key, nil
	}, jwt.WithValidMethods([]string{"EdDSA"}), jwt.WithIssuer("go-ldap-admin"), jwt.WithAudience(c.GatewayID), jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithTimeFunc(func() time.Time { return now }))
	if e != nil || token == nil || !token.Valid || !userIDPattern.MatchString(claims.Subject) || !usernamePattern.MatchString(claims.Username) || claims.ClusterID != c.ClusterID || claims.ID == "" || len(claims.ID) > 128 || claims.IssuedAt == nil || claims.NotBefore == nil || claims.ExpiresAt == nil || len(claims.Audience) != 1 || len(claims.Scopes) == 0 || len(claims.Scopes) > 8 {
		return Claims{}, errors.New("invalid ticket")
	}
	uid, uidErr := strconv.ParseInt(claims.Subject, 10, 64)
	if uidErr != nil || uid > 1<<53-1 || strings.EqualFold(claims.Username, "root") {
		return Claims{}, errors.New("invalid user")
	}
	i, n, x := claims.IssuedAt.Time, claims.NotBefore.Time, claims.ExpiresAt.Time
	if x.Sub(i) > 300*time.Second || x.Sub(n) > 300*time.Second || !x.After(i) || !x.After(n) || i.After(now) || n.After(now) {
		return Claims{}, errors.New("invalid ticket interval")
	}
	if len(claims.DestinationClusterIDs) > 64 {
		return Claims{}, errors.New("too many destinations")
	}
	for _, id := range claims.DestinationClusterIDs {
		if !identifier.MatchString(id) {
			return Claims{}, errors.New("invalid destination")
		}
	}
	found := scope == ""
	seen := map[string]bool{}
	for _, s := range claims.Scopes {
		if seen[s] {
			return Claims{}, errors.New("duplicate scope")
		}
		seen[s] = true
		switch s {
		case "session:create", "terminal", "sftp", "transfer", "transfer:read", "diagnostics":
		default:
			return Claims{}, errors.New("unknown scope")
		}
		if s == scope {
			found = true
		}
	}
	if !found {
		return Claims{}, errors.New("missing scope")
	}
	return claims, nil
}
