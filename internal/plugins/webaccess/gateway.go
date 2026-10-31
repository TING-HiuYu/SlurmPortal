package webaccess

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/TING-HiuYu/SlurmPortal/internal/apperr"
	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
	"github.com/TING-HiuYu/SlurmPortal/internal/httpx"
	"github.com/TING-HiuYu/SlurmPortal/internal/mail"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
	"github.com/google/uuid"
)

const sendingLease = time.Minute

var (
	capabilityID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	errorCode    = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
	bootID       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
)

// binding resolves the calling gateway from its mTLS fingerprint; payload identities are never trusted.
func (s *service) binding(r *http.Request) (Gateway, error) {
	p := authn.FromContext(r.Context())
	g, ok := s.gateways[strings.ToLower(p.TokenID)]
	if !ok {
		return Gateway{}, apperr.New(http.StatusForbidden, "gateway_unknown", "client certificate is not bound to a gateway")
	}
	httpx.SetTarget(r.Context(), "gateway:"+g.GatewayID)
	return g, nil
}

type registration struct {
	Version    int      `json:"version"`
	GatewayID  string   `json:"gateway_id"`
	InstanceID string   `json:"instance_id"`
	BootID     string   `json:"boot_id"`
	ClusterID  string   `json:"cluster_id"`
	Plugins    []string `json:"plugins"`
	UserCAKeys []string `json:"trusted_user_ca_keys"`
}

type gatewayCluster struct {
	ID           string `json:"id"`
	Enabled      bool   `json:"enabled"`
	GatewayID    string `json:"gateway_id"`
	Address      string `json:"address"`
	SSHHost      string `json:"ssh_host"`
	SSHPort      int    `json:"ssh_port"`
	SSHLocalPort int    `json:"ssh_local_port"`
}

// register records the gateway's boot and the user CA keys its login node trusts, and returns the
// cluster addresses and ticket verification keys the gateway cross-checks against its own config.
func (s *service) register(w http.ResponseWriter, r *http.Request) error {
	g, err := s.binding(r)
	if err != nil {
		return err
	}
	var in registration
	if err := httpx.DecodeJSONLimit(w, r, &in, 64<<10); err != nil {
		return err
	}
	if in.Version != 1 || in.GatewayID != g.GatewayID || in.InstanceID != g.InstanceID || in.ClusterID != g.ClusterID || !bootID.MatchString(in.BootID) ||
		len(in.Plugins) > 32 || len(in.UserCAKeys) > 32 || slices.ContainsFunc(in.Plugins, func(p string) bool { return !capabilityID.MatchString(p) }) {
		return apperr.BadRequest("invalid gateway registration")
	}
	keys := make([]string, 0, len(in.UserCAKeys))
	for _, k := range in.UserCAKeys {
		n := normalizeKey(k)
		if n == "" || len(k) > 16384 {
			return apperr.BadRequest("invalid user CA key")
		}
		keys = append(keys, n)
	}
	slices.Sort(keys)
	keys = slices.Compact(keys)
	cfg, err := s.load(r.Context())
	if err != nil {
		return err
	}
	effective := s.effective(cfg)
	if i := slices.IndexFunc(effective, func(c Cluster) bool { return c.ID == g.ClusterID }); i < 0 || !effective[i].Enabled {
		return apperr.New(http.StatusForbidden, "gateway_disabled", "gateway disabled")
	}
	caps, _ := json.Marshal(in.Plugins)
	trusted, _ := json.Marshal(keys)
	if _, err := s.db.Exec(r.Context(), `INSERT INTO v2_gateway_registrations (gateway_id, cluster_id, instance_id, boot_id, fingerprint, capabilities_json, ca_keys_json, registered_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(gateway_id) DO UPDATE SET cluster_id = excluded.cluster_id, instance_id = excluded.instance_id,
		boot_id = excluded.boot_id, fingerprint = excluded.fingerprint, capabilities_json = excluded.capabilities_json, ca_keys_json = excluded.ca_keys_json,
		registered_ms = excluded.registered_ms`, g.GatewayID, g.ClusterID, g.InstanceID, in.BootID, g.Fingerprint, string(caps), string(trusted), s.now().UnixMilli()); err != nil {
		return err
	}
	clusters := make([]gatewayCluster, 0, len(effective))
	for _, c := range effective {
		clusters = append(clusters, gatewayCluster{c.ID, c.Enabled, c.GatewayID, c.Address, c.SSHHost, c.SSHPort, c.SSHLocalPort})
	}
	verification := []jwk{}
	if k, err := s.publicKey(); err == nil {
		k.Alg, k.Use = "", ""
		verification = append(verification, k)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"clusters": clusters, "verification_keys": verification})
	return nil
}

func decodeStrings(raw string) []string {
	var out []string
	_ = json.Unmarshal([]byte(raw), &out)
	return out
}

// TransferEvent is a finished rsync transfer as reported by the source gateway.
type TransferEvent struct {
	Version              int      `json:"version"`
	EventID              string   `json:"event_id"`
	TransferID           string   `json:"transfer_id"`
	GatewayID            string   `json:"gateway_id"`
	ClusterID            string   `json:"cluster_id"`
	UserID               int64    `json:"user_id"`
	Username             string   `json:"username"`
	DestinationClusterID string   `json:"destination_cluster_id"`
	SourcePaths          []string `json:"source_paths"`
	DestinationPath      string   `json:"destination_path"`
	Status               string   `json:"status"`
	ExitCode             *int     `json:"exit_code"`
	StartedAt            string   `json:"started_at"`
	FinishedAt           string   `json:"finished_at"`
	BytesSent            *float64 `json:"bytes_sent"`
	BytesReceived        *float64 `json:"bytes_received"`
	TotalBytes           *float64 `json:"total_bytes"`
	ElapsedSeconds       *float64 `json:"elapsed_seconds"`
	AverageMBPerSecond   *float64 `json:"average_mb_per_second"`
	Error                *string  `json:"error,omitempty"`
}

func remotePath(p string) bool {
	return strings.HasPrefix(p, "/") && len(p) <= 4096 && utf8.ValidString(p) && !strings.ContainsAny(p, "\x00\r\n")
}

// Check enforces the gateway transfer contract for the bound gateway g.
func (e *TransferEvent) Check(g Gateway, now time.Time) (started, finished time.Time, err error) {
	bad := apperr.BadRequest("invalid transfer completion")
	id, perr := uuid.Parse(e.TransferID)
	if perr != nil || id.String() != strings.ToLower(e.TransferID) || e.Version != 1 || e.EventID != e.TransferID || e.GatewayID != g.GatewayID || e.ClusterID != g.ClusterID ||
		e.UserID < 1 || e.UserID > 1<<53-1 || !sshUsername.MatchString(e.Username) || strings.EqualFold(e.Username, "root") || !identifier.MatchString(e.DestinationClusterID) ||
		len(e.SourcePaths) < 1 || len(e.SourcePaths) > 200 || slices.ContainsFunc(e.SourcePaths, func(p string) bool { return !remotePath(p) }) || !remotePath(e.DestinationPath) ||
		e.ExitCode == nil || *e.ExitCode < 0 || *e.ExitCode > 255 || e.Error != nil && !errorCode.MatchString(*e.Error) {
		return time.Time{}, time.Time{}, bad
	}
	code := *e.ExitCode
	switch e.Status {
	case "completed", "partial", "failed":
	default:
		return time.Time{}, time.Time{}, bad
	}
	if (e.Status == "completed") != (code == 0) || (e.Status == "partial") != (code == 23 || code == 24) {
		return time.Time{}, time.Time{}, bad
	}
	for _, m := range []*float64{e.BytesSent, e.BytesReceived, e.TotalBytes, e.ElapsedSeconds, e.AverageMBPerSecond} {
		if m != nil && (*m < 0 || *m > 1<<53-1 || *m != *m) {
			return time.Time{}, time.Time{}, bad
		}
	}
	started, err1 := time.Parse(time.RFC3339Nano, e.StartedAt)
	finished, err2 := time.Parse(time.RFC3339Nano, e.FinishedAt)
	if err1 != nil || err2 != nil || !strings.HasSuffix(e.StartedAt, "Z") || !strings.HasSuffix(e.FinishedAt, "Z") || finished.Before(started) || finished.After(now.Add(30*time.Second)) {
		return time.Time{}, time.Time{}, bad
	}
	return started, finished, nil
}

type eventOutcome struct {
	Outcome   string `json:"outcome"`
	Reason    string `json:"reason,omitempty"`
	Duplicate bool   `json:"duplicate,omitempty"`
}

var (
	errEventConflict = apperr.New(http.StatusConflict, "transfer_conflict", "transfer identity conflict")
	errEventBusy     = apperr.New(http.StatusTooManyRequests, "transfer_busy", "transfer notification busy")
)

// events accepts each transfer once (by content fingerprint) and mails the owner when enabled.
// Disabling web access does not strand transfers already running, so only the binding is required.
func (s *service) events(w http.ResponseWriter, r *http.Request) error {
	g, err := s.binding(r)
	if err != nil {
		return err
	}
	var ev TransferEvent
	if err := httpx.DecodeJSONLimit(w, r, &ev, 128<<10); err != nil {
		return err
	}
	now := s.now().UTC()
	if _, _, err := ev.Check(g, now); err != nil {
		return err
	}
	canonical, _ := json.Marshal(ev)
	sum := sha256.Sum256(canonical)
	fp := hex.EncodeToString(sum[:])
	if done, err := s.claim(r.Context(), g, ev, fp, now); err != nil || done != nil {
		if err != nil {
			return err
		}
		httpx.WriteJSON(w, http.StatusOK, done)
		return nil
	}
	out, err := s.deliver(r.Context(), ev)
	if err != nil {
		_, _ = s.db.Exec(r.Context(), "UPDATE v2_transfer_events SET lease_until_ms = 0, updated_at = ? WHERE transfer_id = ?", now.Format(time.RFC3339Nano), ev.TransferID)
		return err
	}
	status := map[string]string{"smtp_accepted": "sent", "skipped": "skipped"}[out.Outcome]
	if _, err := s.db.Exec(r.Context(), "UPDATE v2_transfer_events SET status = ?, reason = ?, lease_until_ms = 0, updated_at = ? WHERE transfer_id = ?",
		status, out.Reason, s.now().UTC().Format(time.RFC3339Nano), ev.TransferID); err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}

// claim inserts the event or takes over an expired lease; a non-nil outcome means already finished.
func (s *service) claim(ctx context.Context, g Gateway, ev TransferEvent, fp string, now time.Time) (*eventOutcome, error) {
	ts := now.Format(time.RFC3339Nano)
	lease := now.Add(sendingLease).UnixMilli()
	res, err := s.db.Exec(ctx, `INSERT INTO v2_transfer_events (transfer_id, gateway_id, cluster_id, user_id, fingerprint, status, lease_until_ms, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 'sending', ?, ?, ?) ON CONFLICT DO NOTHING`, ev.TransferID, g.GatewayID, g.ClusterID, ev.UserID, fp, lease, ts, ts)
	if err != nil {
		return nil, err
	}
	if res.RowsAffected > 0 {
		return nil, nil
	}
	var got, status, reason string
	if err := store.QueryRow(ctx, s.db, "SELECT fingerprint, status, reason FROM v2_transfer_events WHERE transfer_id = ?", []any{ev.TransferID}, &got, &status, &reason); err != nil {
		return nil, err
	}
	switch {
	case got != fp:
		return nil, errEventConflict
	case status == "sent":
		return &eventOutcome{Outcome: "smtp_accepted", Duplicate: true}, nil
	case status == "skipped":
		return &eventOutcome{Outcome: "skipped", Reason: reason, Duplicate: true}, nil
	}
	res, err = s.db.Exec(ctx, "UPDATE v2_transfer_events SET lease_until_ms = ?, updated_at = ? WHERE transfer_id = ? AND status = 'sending' AND lease_until_ms < ?",
		lease, ts, ev.TransferID, now.UnixMilli())
	if err != nil {
		return nil, err
	}
	if res.RowsAffected == 0 {
		return nil, errEventBusy
	}
	return nil, nil
}

func (s *service) deliver(ctx context.Context, ev TransferEvent) (eventOutcome, error) {
	skip := func(reason string) (eventOutcome, error) {
		return eventOutcome{Outcome: "skipped", Reason: reason}, nil
	}
	cfg, err := s.load(ctx)
	if err != nil {
		return eventOutcome{}, err
	}
	effective := s.effective(cfg)
	find := func(id string) (Cluster, bool) {
		i := slices.IndexFunc(effective, func(c Cluster) bool { return c.ID == id })
		if i < 0 {
			return Cluster{}, false
		}
		return effective[i], true
	}
	src, ok1 := find(ev.ClusterID)
	dst, ok2 := find(ev.DestinationClusterID)
	if !ok1 || !ok2 {
		return skip("cluster_removed")
	}
	u, err := s.identity.GetUser(ctx, ev.UserID)
	if errors.Is(err, apperr.ErrNotFound) || err == nil && (u.Username != ev.Username || u.Status != 1) {
		return skip("user_missing_or_disabled")
	}
	if err != nil {
		return eventOutcome{}, err
	}
	te := cfg.TransferEmail
	data := transferContext(ev, mailUser{u.Username, u.Nickname, u.Mail}, src, dst, te.TimeZone)
	// The latest real transfer is what administrators copy while writing templates; best effort.
	if err := mail.SaveSample(ctx, s.db, sampleKind, data, s.now()); err != nil {
		slog.WarnContext(ctx, "transfer mail sample not saved", "err", err)
	}
	if !te.Enabled {
		return skip("transfer_email_disabled")
	}
	if !mail.ValidAddress(u.Mail) {
		return skip("user_email_invalid")
	}
	data["constants"] = te.Template.Constants
	out, err := mail.Render(te.Template, transferRoots, data, nil)
	if err != nil {
		return eventOutcome{}, err
	}
	err = s.mail.Send(ctx, te.Provider, mail.Message{ID: "file-transfer/" + ev.TransferID, To: u.Mail, Purpose: "transfer", Subject: out.Subject, Text: out.Text, HTML: out.HTML})
	switch {
	case err == nil:
		return eventOutcome{Outcome: "smtp_accepted"}, nil
	case mail.Unavailable(err):
		return skip("mail_provider_unavailable")
	case mail.RecipientRejected(err):
		return skip("smtp_recipient_rejected")
	}
	return eventOutcome{}, apperr.ErrUnavailable.Wrap(err)
}

func bounded(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max] + "…"
}

func formatBytes(b *float64) string {
	switch {
	case b == nil:
		return "Unavailable"
	case *b >= 1e9:
		return fmt.Sprintf("%.2f GB", *b/1e9)
	}
	return fmt.Sprintf("%.2f MB", *b/1e6)
}

func formatDuration(sec *float64) string {
	if sec == nil {
		return "Unavailable"
	}
	n := int64(*sec)
	return fmt.Sprintf("%02d:%02d:%02d", n/3600, n/60%60, n%60)
}
