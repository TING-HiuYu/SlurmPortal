package webaccess

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/apperr"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
)

var (
	identifier  = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	roleKeyword = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	hostLabel   = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
)

// Settings is the administrator-edited web access configuration, stored in D1. Web access is on
// wherever a bound gateway registers: Clusters only override each bound cluster's details, and the
// terminal / files / transfer features follow what that gateway reports it serves.
type Settings struct {
	Clusters      []Cluster     `json:"clusters"`
	TransferEmail TransferEmail `json:"transferEmail"`
	// Rows written before features became automatic carried these; they seed TransferEmail once.
	LegacyTransferEmail bool   `json:"transferEmailEnabled,omitempty"`
	LegacyMailProvider  string `json:"mailProvider,omitempty"`
}

// Cluster is one login node reachable through its gateway.
type Cluster struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	// Address is the public SSH host other clusters use as a transfer destination.
	Address string `json:"address"`
	// SSHHost/SSHLocalPort are what this cluster's own gateway dials.
	SSHHost      string `json:"sshHost"`
	SSHPort      int    `json:"sshPort"`
	SSHLocalPort int    `json:"sshLocalPort"`
	// SSHCAID pins the signing CA; 0 selects the CA that the login node's TrustedUserCAKeys accepts.
	SSHCAID      int64    `json:"sshCaId"`
	GatewayID    string   `json:"gatewayId"`
	GatewayURL   string   `json:"gatewayUrl"`
	AllowedRoles []string `json:"allowedRoles"`
}

func checkHost(v string) (string, error) {
	v = strings.ToLower(v)
	if ip := net.ParseIP(v); ip != nil {
		return ip.String(), nil
	}
	if v == "" || len(v) > 253 {
		return "", errors.New("invalid SSH host")
	}
	for _, label := range strings.Split(v, ".") {
		if !hostLabel.MatchString(label) {
			return "", errors.New("invalid SSH host")
		}
	}
	return v, nil
}

func checkOrigin(v string) (string, error) {
	u, err := url.Parse(v)
	if err != nil || len(v) > 512 || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", errors.New("gateway URL must be an HTTPS origin")
	}
	return u.Scheme + "://" + strings.ToLower(u.Host), nil
}

func validPort(p int) bool { return p >= 1 && p <= 65535 }

// Validate normalizes s in place.
func (s *Settings) Validate() error {
	if len(s.Clusters) > 64 {
		return errors.New("too many clusters")
	}
	if err := s.TransferEmail.validate(); err != nil {
		return err
	}
	ids, gateways := map[string]bool{}, map[string]bool{}
	for i := range s.Clusters {
		c := &s.Clusters[i]
		c.Name = strings.TrimSpace(c.Name)
		if c.SSHLocalPort == 0 {
			c.SSHLocalPort = c.SSHPort
		}
		if !identifier.MatchString(c.ID) || ids[c.ID] || !identifier.MatchString(c.GatewayID) || gateways[c.GatewayID] ||
			c.Name == "" || len(c.Name) > 200 || strings.ContainsAny(c.Name, "\r\n\x00") || !validPort(c.SSHPort) || !validPort(c.SSHLocalPort) || c.SSHCAID < 0 ||
			len(c.AllowedRoles) == 0 || len(c.AllowedRoles) > 32 {
			return fmt.Errorf("cluster %d: invalid settings", i)
		}
		for _, r := range c.AllowedRoles {
			if !roleKeyword.MatchString(r) {
				return fmt.Errorf("cluster %q: invalid role keyword", c.ID)
			}
		}
		slices.Sort(c.AllowedRoles)
		c.AllowedRoles = slices.Compact(c.AllowedRoles)
		var err error
		if c.Address, err = checkHost(c.Address); err != nil {
			return fmt.Errorf("cluster %q: %w", c.ID, err)
		}
		if c.SSHHost, err = checkHost(c.SSHHost); err != nil {
			return fmt.Errorf("cluster %q: %w", c.ID, err)
		}
		// An empty URL leaves the cluster registered but not offered to users until it is set.
		if c.GatewayURL != "" {
			if c.GatewayURL, err = checkOrigin(c.GatewayURL); err != nil {
				return fmt.Errorf("cluster %q: %w", c.ID, err)
			}
		}
		ids[c.ID], gateways[c.GatewayID] = true, true
	}
	if s.Clusters == nil {
		s.Clusters = []Cluster{}
	}
	return nil
}

// Capabilities summarise what the caller can use across their clusters.
type Capabilities struct {
	Enabled              bool `json:"enabled"`
	TerminalEnabled      bool `json:"terminalEnabled"`
	FilesEnabled         bool `json:"filesEnabled"`
	TransferEnabled      bool `json:"transferEnabled"`
	TransferEmailEnabled bool `json:"transferEmailEnabled"`
}

// Features are what one cluster's gateway currently serves (from its last fresh registration).
type Features struct {
	Terminal bool `json:"terminal"`
	Files    bool `json:"files"`
	Transfer bool `json:"transfer"`
}

func (f Features) any() bool { return f.Terminal || f.Files || f.Transfer }

func featuresOf(plugins []string) Features {
	return Features{Terminal: slices.Contains(plugins, "web-terminal"), Files: slices.Contains(plugins, "web-files"), Transfer: slices.Contains(plugins, "file-transfer")}
}

func (s Settings) cluster(id string) (Cluster, bool) {
	for _, c := range s.Clusters {
		if c.ID == id {
			return c, true
		}
	}
	return Cluster{}, false
}

func (svc *service) load(ctx context.Context) (Settings, error) {
	var raw string
	err := store.QueryRow(ctx, svc.db, "SELECT config_json FROM v2_web_access_config WHERE id = 1", nil, &raw)
	if errors.Is(err, store.ErrNoRows) {
		return Settings{Clusters: []Cluster{}, TransferEmail: defaultTransferEmail()}, nil
	}
	if err != nil {
		return Settings{}, err
	}
	var s Settings
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return Settings{}, apperr.ErrUnavailable.Wrap(err)
	}
	if s.TransferEmail.Template.Subject == "" {
		s.TransferEmail = defaultTransferEmail()
		s.TransferEmail.Enabled, s.TransferEmail.Provider = s.LegacyTransferEmail, s.LegacyMailProvider
	}
	s.LegacyTransferEmail, s.LegacyMailProvider = false, ""
	return s, s.Validate()
}

func (svc *service) save(ctx context.Context, s Settings) error {
	s.LegacyTransferEmail, s.LegacyMailProvider = false, ""
	raw, _ := json.Marshal(s)
	_, err := svc.db.Exec(ctx, `INSERT INTO v2_web_access_config (id, config_json, updated_at) VALUES (1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET config_json = excluded.config_json, updated_at = excluded.updated_at`, string(raw), svc.now().UTC().Format(time.RFC3339Nano))
	return err
}
