package resourcestatus

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"time"
	"unicode"

	"github.com/TING-HiuYu/SlurmPortal/internal/apperr"
	"github.com/TING-HiuYu/SlurmPortal/internal/authz"
	"github.com/TING-HiuYu/SlurmPortal/internal/httpx"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugin"
	"github.com/TING-HiuYu/SlurmPortal/internal/statusreport"
)

const maxCount = 1 << 40

var (
	jobIDPattern = regexp.MustCompile(`^[0-9][0-9_+.\[\]%-]{0,79}$`)
	jobState     = regexp.MustCompile(`^[A-Z][A-Z_+]{0,47}$`)
	errInvalid   = apperr.BadRequest("invalid resource status snapshot")
)

// MasterConfig is the master-side plugin configuration.
type MasterConfig struct {
	StaleSeconds int `yaml:"stale-seconds"`
}

// Plugin is the resource-status plugin: slaves push queue and capacity reports, the UI reads them.
type Plugin struct{}

func (Plugin) ID() string                  { return ID }
func (Plugin) Roles() []plugin.Role        { return []plugin.Role{plugin.RoleStandalone, plugin.RoleMaster} }
func (Plugin) Close(context.Context) error { return nil }

func (Plugin) Init(_ context.Context, h *plugin.Host) error {
	var cfg MasterConfig
	if err := h.Decode(&cfg); err != nil {
		return err
	}
	s := &service{reports: statusreport.Store{DB: h.Store, Kind: ID}, clusters: h.Clusters,
		stale: max(statusreport.MinStale, time.Duration(cfg.StaleSeconds)*time.Second), now: time.Now}
	h.Router.Handle(httpx.Route{Method: http.MethodPost, Path: SnapshotPath, Permission: authz.Peer(), Handler: s.ingest, Routine: true})
	h.Router.Handle(httpx.Route{Method: http.MethodGet, Path: "/api/v2/resource-status", Permission: authz.Authenticated(), Handler: s.query})
	return nil
}

type service struct {
	reports  statusreport.Store
	clusters plugin.Clusters
	stale    time.Duration
	now      func() time.Time
}

func cleanText(v string, maxBytes int) bool {
	if len(v) > maxBytes {
		return false
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func countOK(p *int64, limit int64) bool { return p == nil || *p >= 0 && *p <= limit }

// Check enforces the v1 resource-status contract: allowlisted scheduling metadata only.
func (r *Report) Check(cluster string, now time.Time, maxJobs int) error {
	if r.Version != 1 || r.ClusterID != cluster || r.ObservedAt.IsZero() || r.ObservedAt.After(now.Add(5*time.Second)) {
		return errInvalid
	}
	if r.Health == "error" {
		if r.Error == nil || *r.Error != "collection_failed" || r.Resources != nil || len(r.Jobs) != 0 || r.JobsTotal != 0 || r.JobsTruncated {
			return errInvalid
		}
		return nil
	}
	if r.Health != "ok" || r.Error != nil && *r.Error != "" || r.Resources == nil {
		return errInvalid
	}
	for _, c := range []Capacity{r.Resources.CPU, r.Resources.MemoryMiB, r.Resources.GPU} {
		if c.Total < 0 || c.Total > maxCount || c.Available < 0 || c.Available > c.Total || !countOK(c.Allocated, maxCount) {
			return errInvalid
		}
	}
	if len(r.Jobs) > maxJobs || r.JobsTotal < len(r.Jobs) || r.JobsTotal > 10_000_000 || r.JobsTruncated != (r.JobsTotal > len(r.Jobs)) {
		return errInvalid
	}
	seen := map[string]bool{}
	for _, j := range r.Jobs {
		if !jobIDPattern.MatchString(j.ID) || seen[j.ID] || !jobState.MatchString(j.State) ||
			!cleanText(j.Name, 256) || !cleanText(j.User, 128) || !cleanText(j.Partition, 128) || !cleanText(j.Reason, 256) ||
			!countOK(j.ElapsedSeconds, 3155760000) || !countOK(j.TimeLimitSeconds, 3155760000) || !countOK(j.NodeCount, 1<<20) {
			return errInvalid
		}
		for _, x := range []JobResources{j.Allocated, j.Requested} {
			if !countOK(x.CPU, maxCount) || !countOK(x.MemoryMiB, maxCount) || !countOK(x.GPU, maxCount) {
				return errInvalid
			}
		}
		seen[j.ID] = true
	}
	return nil
}

type contents struct {
	Resources     *Resources `json:"resources"`
	Jobs          []Job      `json:"jobs"`
	JobsTotal     int        `json:"jobsTotal"`
	JobsTruncated bool       `json:"jobsTruncated"`
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
	if err := rep.Check(cl.ID, now, MaxJobs); err != nil {
		return err
	}
	var observed *time.Time
	var payload *string
	if rep.Health == "ok" {
		b, _ := json.Marshal(contents{rep.Resources, rep.Jobs, rep.JobsTotal, rep.JobsTruncated})
		p := string(b)
		observed, payload = &rep.ObservedAt, &p
	}
	won, err := s.reports.Put(r.Context(), cl.ID, rep.ObservedAt, now, rep.Health, observed, "{}", payload)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"outcome": map[bool]string{true: "accepted", false: "ignored"}[won]})
	return nil
}

type view struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Health     string     `json:"health"`
	Error      *string    `json:"error"`
	ObservedAt *time.Time `json:"observedAt"`
	ReceivedAt *time.Time `json:"receivedAt"`
	contents
}

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
	str := func(v string) *string { return &v }
	out := make([]view, 0, len(selected))
	for _, c := range selected {
		v := view{ID: c.ID, Name: c.Name, Health: "unavailable", Error: str("never_observed"), contents: contents{Jobs: []Job{}}}
		if x, ok := rows[c.ID]; ok {
			v.ReceivedAt = &x.Received
			switch {
			case now.Sub(x.Measured()) > s.stale:
				v.Health, v.Error = "stale", str("stale_snapshot")
			case x.Health == "error":
				v.Health, v.Error = "error", str("collection_failed")
			case x.Payload != nil && json.Unmarshal([]byte(*x.Payload), &v.contents) == nil:
				v.Health, v.Error, v.ObservedAt = "ok", nil, x.Observed
			}
		}
		out = append(out, v)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"updatedAt": now.UTC(), "staleAfterSeconds": int(s.stale.Seconds()), "clusters": out})
	return nil
}
