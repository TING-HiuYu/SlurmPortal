package clusterstatus

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/apperr"
	"github.com/TING-HiuYu/SlurmPortal/internal/authz"
	"github.com/TING-HiuYu/SlurmPortal/internal/httpx"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugin"
	"github.com/TING-HiuYu/SlurmPortal/internal/statusreport"
)

// MasterConfig is the master-side plugin configuration.
type MasterConfig struct {
	// StaleSeconds marks a cluster offline when its last report is older (minimum 90).
	StaleSeconds int `yaml:"stale-seconds"`
}

// Plugin is the cluster-status plugin: slaves push node reports, the UI reads them.
type Plugin struct{}

func (Plugin) ID() string                  { return ID }
func (Plugin) Roles() []plugin.Role        { return []plugin.Role{plugin.RoleStandalone, plugin.RoleMaster} }
func (Plugin) Close(context.Context) error { return nil }

func (Plugin) Init(_ context.Context, h *plugin.Host) error {
	var cfg MasterConfig
	if err := h.Decode(&cfg); err != nil {
		return err
	}
	s := &service{reports: statusreport.Store{DB: h.Store, Kind: ID}, clusters: h.Clusters, stale: max(statusreport.MinStale, time.Duration(cfg.StaleSeconds)*time.Second), now: time.Now}
	h.Router.Handle(httpx.Route{Method: http.MethodPost, Path: SnapshotPath, Permission: authz.Peer(), Handler: s.ingest, Routine: true})
	h.Router.Handle(httpx.Route{Method: http.MethodGet, Path: "/api/v2/cluster-status", Permission: authz.Authenticated(), Handler: s.query})
	return nil
}

type service struct {
	reports  statusreport.Store
	clusters plugin.Clusters
	stale    time.Duration
	now      func() time.Time
}

var errInvalid = apperr.BadRequest("invalid cluster status snapshot")

// Check enforces the v1 snapshot contract before anything is stored.
func (r *Report) Check(cluster string, now time.Time, maxNodes int) error {
	if r.Version != 1 || r.ClusterID != cluster || r.ObservedAt.IsZero() || r.ObservedAt.After(now.Add(5*time.Second)) {
		return errInvalid
	}
	if err := r.checkAddress(); err != nil {
		return err
	}
	if len(r.NodeCounts) != len(Statuses) || len(r.Nodes) > maxNodes || len(r.Nodes) > r.NodesTotal || r.NodesTruncated != (r.NodesTotal > len(r.Nodes)) {
		return errInvalid
	}
	sum := 0
	for _, st := range Statuses {
		n, ok := r.NodeCounts[st]
		if !ok || n < 0 || n > 20000 {
			return errInvalid
		}
		sum += n
	}
	if sum != r.NodesTotal {
		return errInvalid
	}
	switch {
	case r.Health == "ok" && r.Error == nil && r.NodesTotal > 0:
	case r.Health == "error" && r.Error != nil && *r.Error == "collection_failed" && r.NodesTotal == 0:
	default:
		return errInvalid
	}
	seen, shown := map[string]bool{}, map[string]int{}
	for _, n := range r.Nodes {
		if !nodeName.MatchString(n.Name) || net.ParseIP(n.Name) != nil || seen[n.Name] || !stateText.MatchString(n.SlurmState) || n.Status != classify(n.SlurmState) {
			return errInvalid
		}
		seen[n.Name] = true
		shown[n.Status]++
	}
	for st, n := range shown {
		if n > r.NodeCounts[st] {
			return errInvalid
		}
	}
	return nil
}

func (r *Report) checkAddress() error {
	if len(r.CurrentIPs) > 16 {
		return errInvalid
	}
	for i, v := range r.CurrentIPs {
		ip := net.ParseIP(v)
		if ip == nil || !acceptableIP(ip) || strings.Contains(v, ":") && ip.To4() != nil {
			return errInvalid
		}
		r.CurrentIPs[i] = ip.String()
	}
	slices.Sort(r.CurrentIPs)
	r.CurrentIPs = slices.Compact(r.CurrentIPs)
	if r.Address != nil {
		norm, err := NormalizeAddress(*r.Address)
		if err != nil || norm == "" {
			return errInvalid
		}
		r.Address = &norm
	}
	switch r.AddressHealth {
	case "not_configured":
		if r.Address != nil || len(r.CurrentIPs) > 0 {
			return errInvalid
		}
	case "ok":
		if r.Address == nil || len(r.CurrentIPs) == 0 {
			return errInvalid
		}
	case "unresolved":
		if r.Address == nil || len(r.CurrentIPs) > 0 {
			return errInvalid
		}
	default:
		return errInvalid
	}
	return nil
}

type metadata struct {
	Address       *string  `json:"address"`
	CurrentIPs    []string `json:"currentIps"`
	AddressHealth string   `json:"addressHealth"`
}

type inventory struct {
	Nodes          []Node         `json:"nodes"`
	NodeCounts     map[string]int `json:"nodeCounts"`
	NodesTotal     int            `json:"nodesTotal"`
	NodesTruncated bool           `json:"nodesTruncated"`
}

func (s *service) ingest(w http.ResponseWriter, r *http.Request) error {
	cl, err := statusreport.PeerCluster(r, s.clusters)
	if err != nil {
		return err
	}
	var rep Report
	if err := httpx.DecodeJSON(w, r, &rep); err != nil {
		return err
	}
	now := s.now().UTC()
	if err := rep.Check(cl.ID, now, MaxNodes); err != nil {
		return err
	}
	meta, _ := json.Marshal(metadata{rep.Address, rep.CurrentIPs, rep.AddressHealth})
	var observed *time.Time
	var payload *string
	if rep.Health == "ok" {
		inv, _ := json.Marshal(inventory{rep.Nodes, rep.NodeCounts, rep.NodesTotal, rep.NodesTruncated})
		observed, payload = &rep.ObservedAt, ptr(string(inv))
	}
	won, err := s.reports.Put(r.Context(), cl.ID, rep.ObservedAt, now, rep.Health, observed, string(meta), payload)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"outcome": map[bool]string{true: "accepted", false: "ignored"}[won]})
	return nil
}

// ClusterView is one cluster's latest status as the API returns it.
type ClusterView struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Status     string     `json:"status"`
	Health     string     `json:"health"`
	Error      *string    `json:"error"`
	ReportedAt *time.Time `json:"reportedAt"`
	ObservedAt *time.Time `json:"observedAt"`
	ReceivedAt *time.Time `json:"receivedAt"`
	metadata
	inventory
}

func overall(c map[string]int) string {
	total := 0
	for _, n := range c {
		total += n
	}
	healthy := c["idle"] + c["full"] + c["running"]
	switch {
	case total == 0:
		return "unavailable"
	case c["offline"] == total:
		return "offline"
	case c["offline"] > 0 || c["unknown"] > 0 || healthy == 0:
		return "abnormal"
	case c["drain"] > 0 || c["maintenance"] > 0:
		return "partial_maintenance"
	}
	return "normal"
}

func ptr[T any](v T) *T { return &v }

func (s *service) query(w http.ResponseWriter, r *http.Request) error {
	selected, err := statusreport.Select(r, s.clusters)
	if err != nil {
		return err
	}
	rows, err := s.reports.All(r.Context())
	if err != nil {
		return err
	}
	now := s.now()
	out := make([]ClusterView, 0, len(selected))
	for _, c := range selected {
		v := ClusterView{ID: c.ID, Name: c.Name, Status: "unavailable", Health: "unavailable", Error: ptr("never_observed"),
			metadata: metadata{CurrentIPs: []string{}, AddressHealth: "not_configured"}, inventory: inventory{Nodes: []Node{}}}
		if x, ok := rows[c.ID]; ok {
			_ = json.Unmarshal([]byte(x.Meta), &v.metadata)
			v.ReportedAt, v.ReceivedAt = &x.Attempted, &x.Received
			switch {
			case now.Sub(x.Measured()) > s.stale:
				v.Status, v.Health, v.Error = "offline", "stale", ptr("stale_snapshot")
				v.CurrentIPs = []string{}
				if v.Address != nil {
					v.AddressHealth = "unresolved"
				}
			case x.Health == "error":
				v.Status, v.Health, v.Error = "abnormal", "error", ptr("collection_failed")
			case x.Payload != nil && json.Unmarshal([]byte(*x.Payload), &v.inventory) == nil:
				v.ObservedAt = x.Observed
				v.Status, v.Health, v.Error = overall(v.NodeCounts), "ok", nil
			}
		}
		out = append(out, v)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"updatedAt": now.UTC(), "staleAfterSeconds": int(s.stale.Seconds()), "clusters": out})
	return nil
}
