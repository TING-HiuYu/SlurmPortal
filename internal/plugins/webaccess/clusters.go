package webaccess

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/apperr"
)

// registrationFresh is how long a gateway's last registration keeps its features on; gateways
// re-register every minute.
const registrationFresh = 5 * time.Minute

// defaultCluster is how a newly bound gateway's cluster starts before an administrator edits it.
func (s *service) defaultCluster(g Gateway) Cluster {
	return Cluster{ID: g.ClusterID, Name: cmp.Or(s.names[g.ClusterID], g.ClusterID), Enabled: true, SSHHost: "127.0.0.1", SSHPort: 22, SSHLocalPort: 22,
		GatewayID: g.GatewayID, AllowedRoles: []string{"admin", "user"}}
}

// effective lists one cluster per gateway binding: the stored override if any, else the default.
// The gateway identity always comes from the binding, never from stored settings.
func (s *service) effective(cfg Settings) []Cluster {
	out := make([]Cluster, 0, len(s.bindings))
	for _, g := range s.bindings {
		c, ok := cfg.cluster(g.ClusterID)
		if !ok {
			c = s.defaultCluster(g)
		}
		c.GatewayID = g.GatewayID
		out = append(out, c)
	}
	return out
}

// Registration is a gateway's last report.
type Registration struct {
	InstanceID   string    `json:"instanceId"`
	Plugins      []string  `json:"plugins"`
	RegisteredAt time.Time `json:"registeredAt"`
	Fresh        bool      `json:"fresh"`
}

func (s *service) registrations(ctx context.Context) (map[string]Registration, error) {
	rows, err := s.db.Query(ctx, "SELECT gateway_id, instance_id, capabilities_json, registered_ms FROM v2_gateway_registrations")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Registration{}
	now := s.now()
	for rows.Next() {
		var id, instance, caps string
		var ms int64
		if err := rows.Scan(&id, &instance, &caps, &ms); err != nil {
			return nil, err
		}
		at := time.UnixMilli(ms).UTC()
		out[id] = Registration{InstanceID: instance, Plugins: decodeStrings(caps), RegisteredAt: at, Fresh: now.Sub(at) < registrationFresh}
	}
	return out, rows.Err()
}

// liveCluster is a cluster a user may reach, with the features its gateway serves right now.
type liveCluster struct {
	Cluster
	Features Features
}

// userClusters are the enabled, configured, freshly registered clusters one of roles may use.
func (s *service) userClusters(ctx context.Context, cfg Settings, roles []string) ([]liveCluster, error) {
	regs, err := s.registrations(ctx)
	if err != nil {
		return nil, err
	}
	out := []liveCluster{}
	for _, c := range s.effective(cfg) {
		reg, ok := regs[c.GatewayID]
		f := featuresOf(reg.Plugins)
		if !c.Enabled || c.GatewayURL == "" || !ok || !reg.Fresh || !f.any() ||
			!slices.ContainsFunc(c.AllowedRoles, func(r string) bool { return slices.Contains(roles, r) }) {
			continue
		}
		out = append(out, liveCluster{c, f})
	}
	return out, nil
}

func capabilitiesOf(cfg Settings, clusters []liveCluster) Capabilities {
	var c Capabilities
	transfer := 0
	for _, l := range clusters {
		c.Enabled = true
		c.TerminalEnabled = c.TerminalEnabled || l.Features.Terminal
		c.FilesEnabled = c.FilesEnabled || l.Features.Files
		if l.Features.Transfer {
			transfer++
		}
	}
	c.TransferEnabled = transfer >= 2
	c.TransferEmailEnabled = c.TransferEnabled && cfg.TransferEmail.Enabled
	return c
}

// checkBound refuses overrides for clusters no gateway is bound to.
func (s *service) checkBound(in Settings) error {
	for _, c := range in.Clusters {
		if !slices.ContainsFunc(s.bindings, func(g Gateway) bool { return g.ClusterID == c.ID }) {
			return apperr.BadRequest("cluster " + c.ID + " has no gateway binding")
		}
	}
	return nil
}
