package currentstatus

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/apperr"
	"github.com/TING-HiuYu/SlurmPortal/internal/authz"
	"github.com/TING-HiuYu/SlurmPortal/internal/httpx"
	"github.com/TING-HiuYu/SlurmPortal/internal/objstore"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugin"
	"github.com/TING-HiuYu/SlurmPortal/internal/statusreport"
)

const (
	casAttempts  = 4
	refreshAfter = 60
)

// MasterConfig selects the private bucket. Write credentials come from PLUGIN_CURRENT_STATUS_R2_ACCESS_KEY_ID /
// PLUGIN_CURRENT_STATUS_R2_SECRET_ACCESS_KEY; links are signed with PLUGIN_CURRENT_STATUS_R2_READ_ACCESS_KEY_ID /
// PLUGIN_CURRENT_STATUS_R2_READ_SECRET_ACCESS_KEY when set (an ObjectRead-only token), else with the write pair.
type MasterConfig struct {
	AccountID string `yaml:"account-id"`
	Bucket    string `yaml:"bucket"`
	// Endpoint overrides the R2 endpoint for an S3-compatible test server.
	Endpoint string `yaml:"endpoint"`
	// PublicEndpoint is the address browsers use for the bucket when it differs from Endpoint (for
	// example MinIO behind a published port); presigned links are signed for it.
	PublicEndpoint string `yaml:"public-endpoint"`
	StaleSeconds   int    `yaml:"stale-seconds"`
	LinkSeconds    int    `yaml:"link-seconds"`
}

// Plugin is the current-status plugin.
type Plugin struct{}

func (Plugin) ID() string                  { return ID }
func (Plugin) Roles() []plugin.Role        { return []plugin.Role{plugin.RoleStandalone, plugin.RoleMaster} }
func (Plugin) Close(context.Context) error { return nil }

func (Plugin) Init(_ context.Context, h *plugin.Host) error {
	cfg := MasterConfig{StaleSeconds: 180, LinkSeconds: 900}
	if err := h.Decode(&cfg); err != nil {
		return err
	}
	if cfg.StaleSeconds < 60 || cfg.StaleSeconds > 3600 || cfg.LinkSeconds < 60 || cfg.LinkSeconds > 3600 {
		return errors.New("current-status: stale-seconds and link-seconds must be within [60, 3600]")
	}
	creds := func(id, secret string) objstore.Credentials {
		return objstore.Credentials{AccessKeyID: h.Getenv(id), SecretAccessKey: h.Getenv(secret)}
	}
	write := creds("PLUGIN_CURRENT_STATUS_R2_ACCESS_KEY_ID", "PLUGIN_CURRENT_STATUS_R2_SECRET_ACCESS_KEY")
	read := creds("PLUGIN_CURRENT_STATUS_R2_READ_ACCESS_KEY_ID", "PLUGIN_CURRENT_STATUS_R2_READ_SECRET_ACCESS_KEY")
	if read.AccessKeyID == "" {
		read = write
	}
	writer, err := newClient(cfg, write)
	if err != nil {
		return err
	}
	reader, err := newClient(cfg, read)
	if err != nil {
		return err
	}
	linker := reader
	if cfg.PublicEndpoint != "" {
		pub := cfg
		pub.Endpoint = cfg.PublicEndpoint
		if linker, err = newClient(pub, read); err != nil {
			return err
		}
	}
	s := &service{writer: writer, reader: reader, linker: linker, clusters: h.Clusters, now: time.Now,
		stale: time.Duration(cfg.StaleSeconds) * time.Second, link: time.Duration(cfg.LinkSeconds) * time.Second}
	h.Router.Handle(httpx.Route{Method: http.MethodPut, Path: IngestPath, Permission: authz.Peer(), Handler: s.ingest, Routine: true})
	h.Router.Handle(httpx.Route{Method: http.MethodGet, Path: LinksPath, Permission: authz.Authenticated(), Handler: s.links})
	h.Services.Provide(ServiceName, Reader(s))
	return nil
}

func newClient(cfg MasterConfig, creds objstore.Credentials) (*objstore.Client, error) {
	if cfg.Endpoint == "" {
		return objstore.R2(cfg.AccountID, cfg.Bucket, creds)
	}
	c := &objstore.Client{Endpoint: cfg.Endpoint, Bucket: cfg.Bucket, Region: "auto", Creds: creds}
	return c, c.Validate()
}

type service struct {
	writer, reader *objstore.Client
	// linker signs the browser links; it is reader unless a public endpoint is configured.
	linker      *objstore.Client
	clusters    plugin.Clusters
	stale, link time.Duration
	now         func() time.Time
}

// ServiceName registers the snapshot reader in plugin.Services (used by diagnostics).
const ServiceName = "current-status"

// Snapshot is what diagnostics needs from a cluster's current status: when it was observed and the
// health of its node (cluster_status) and queue (resource_status) sections ("" = section absent).
type Snapshot struct {
	ObservedAt     time.Time
	ClusterHealth  string
	ResourceHealth string
}

// Reader returns a cluster's latest snapshot; found is false when the cluster never published.
type Reader interface {
	Latest(ctx context.Context, cluster string) (snap Snapshot, found bool, err error)
}

// maxSnapshot bounds what Latest downloads; a snapshot is tens of kilobytes.
const maxSnapshot = 4 << 20

func (s *service) Latest(ctx context.Context, cluster string) (Snapshot, bool, error) {
	body, err := s.reader.Get(ctx, Key(cluster), maxSnapshot)
	if err != nil || body == nil {
		return Snapshot{}, false, err
	}
	var doc struct {
		ObservedAt    time.Time `json:"observed_at"`
		ClusterStatus *struct {
			Health string `json:"health"`
		} `json:"cluster_status"`
		ResourceStatus *struct {
			Health string `json:"health"`
		} `json:"resource_status"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return Snapshot{}, false, err
	}
	out := Snapshot{ObservedAt: doc.ObservedAt}
	if doc.ClusterStatus != nil {
		out.ClusterHealth = doc.ClusterStatus.Health
	}
	if doc.ResourceStatus != nil {
		out.ResourceHealth = doc.ResourceStatus.Health
	}
	return out, true, nil
}

// Key is the only object a cluster's status lives in.
func Key(cluster string) string { return "current-status/v1/" + cluster + "/current.json" }

var errInvalid = apperr.BadRequest("invalid current status report")

// Check validates both sections with the per-plugin contracts; complete lists only.
func (r *Report) Check(cluster string, now time.Time) error {
	if r.Version != 1 || r.ClusterID != cluster || r.ObservedAt.IsZero() || r.ResourceStatus == nil || r.ClusterStatus == nil ||
		!r.ResourceStatus.ObservedAt.Equal(r.ObservedAt) || !r.ClusterStatus.ObservedAt.Equal(r.ObservedAt) {
		return errInvalid
	}
	if err := r.ResourceStatus.Check(cluster, now, MaxItems); err != nil {
		return err
	}
	if err := r.ClusterStatus.Check(cluster, now, MaxItems); err != nil {
		return err
	}
	if r.ResourceStatus.JobsTruncated || r.ClusterStatus.NodesTruncated {
		return apperr.BadRequest("current status requires complete lists")
	}
	return nil
}

// stored is the object body; ReceivedAt is excluded from the duplicate hash.
type stored struct {
	Report
	ReceivedAt        time.Time `json:"received_at"`
	StaleAfterSeconds int       `json:"stale_after_seconds"`
}

func (s *service) ingest(w http.ResponseWriter, r *http.Request) error {
	cl, err := statusreport.PeerCluster(r, s.clusters)
	if err != nil {
		return err
	}
	var rep Report
	if err := httpx.DecodeJSONLimit(w, r, &rep, MaxPayloadBytes); err != nil {
		return err
	}
	now := s.now().UTC()
	if err := rep.Check(cl.ID, now); err != nil {
		return err
	}
	rep.ObservedAt = rep.ObservedAt.UTC().Truncate(time.Millisecond)
	logical, _ := json.Marshal(rep)
	sum := sha256.Sum256(logical)
	body, _ := json.Marshal(stored{rep, now, int(s.stale.Seconds())})
	if len(body) > MaxPayloadBytes {
		return apperr.ErrTooLarge
	}
	outcome, err := s.put(r.Context(), cl.ID, rep.ObservedAt.UnixMilli(), hex.EncodeToString(sum[:]), body)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"outcome": outcome})
	return nil
}

var errStorage = apperr.New(http.StatusServiceUnavailable, "storage_unavailable", "current status storage unavailable")

// put is a monotonic compare-and-swap: an older or equal observation never replaces a newer one,
// and every write is conditioned on the ETag it read.
func (s *service) put(ctx context.Context, cluster string, observed int64, hash string, body []byte) (string, error) {
	key := Key(cluster)
	for range casAttempts {
		prev, err := s.writer.Head(ctx, key)
		if err != nil {
			return "", errStorage
		}
		opts := objstore.PutOptions{ContentType: "application/json; charset=utf-8", CacheControl: "no-store, max-age=0",
			Meta: map[string]string{"version": "1", "cluster-id": cluster, "observed-ms": strconv.FormatInt(observed, 10), "sha256": hash}}
		if prev == nil {
			opts.IfNoneMatch = true
		} else {
			stamp, err := strconv.ParseInt(prev.Meta["observed-ms"], 10, 64)
			if prev.Meta["version"] != "1" || prev.Meta["cluster-id"] != cluster || err != nil || stamp < 0 {
				return "", errStorage
			}
			if stamp >= observed {
				if stamp == observed && prev.Meta["sha256"] == hash {
					return "duplicate", nil
				}
				return "ignored", nil
			}
			opts.IfMatch = prev.ETag
		}
		switch err := s.writer.Put(ctx, key, body, opts); {
		case err == nil:
			return "accepted", nil
		case !errors.Is(err, objstore.ErrPrecondition):
			return "", errStorage
		}
	}
	return "", apperr.New(http.StatusServiceUnavailable, "storage_contention", "current status write contention")
}

type link struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// links signs one GET per configured cluster for any active authenticated account, the same read
// scope as the cluster/resource status APIs. Clients cannot choose keys, hosts or methods.
func (s *service) links(w http.ResponseWriter, r *http.Request) error {
	if r.URL.RawQuery != "" {
		return apperr.BadRequest("current status links take no parameters")
	}
	out := make([]link, 0, len(s.clusters))
	var expires *time.Time
	for _, c := range s.clusters {
		u, exp, err := s.linker.PresignGET(Key(c.ID), s.link)
		if err != nil {
			return errStorage
		}
		out = append(out, link{c.ID, c.Name, u, exp})
		expires = &exp
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"version": 1, "refreshAfterSeconds": refreshAfter,
		"staleAfterSeconds": int(s.stale.Seconds()), "expiresAt": expires, "clusters": out})
	return nil
}
