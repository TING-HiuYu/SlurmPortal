// Package jobnotification e-mails job owners when slaves report finished jobs, with admin config/preview/test endpoints.
package jobnotification

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/apperr"
	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
	"github.com/TING-HiuYu/SlurmPortal/internal/authz"
	"github.com/TING-HiuYu/SlurmPortal/internal/httpx"
	"github.com/TING-HiuYu/SlurmPortal/internal/mail"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugin"
	"github.com/TING-HiuYu/SlurmPortal/internal/statusreport"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
)

const (
	ID             = "job-notification"
	CompletionPath = "/sync/v2/job-completions"
	// MinActiveSeconds is exclusive, matching the slave hook and normalizer.
	MinActiveSeconds = 60

	maxEventBytes = 262144
	maxAdminBytes = 393216
	sendingLease  = 2 * time.Minute
)

var (
	errConflict = apperr.New(http.StatusConflict, "conflict", "event identity conflict")
	errBusy     = apperr.New(http.StatusTooManyRequests, "busy", "notification sender busy")
)

// Plugin is the job-notification plugin.
type Plugin struct{}

func (Plugin) ID() string                  { return ID }
func (Plugin) Roles() []plugin.Role        { return []plugin.Role{plugin.RoleStandalone, plugin.RoleMaster} }
func (Plugin) Close(context.Context) error { return nil }

func (Plugin) Init(_ context.Context, h *plugin.Host) error {
	if h.Mail == nil {
		return errors.New("job-notification: no mail hub")
	}
	s := &service{db: h.Store, clusters: h.Clusters, mail: h.Mail, now: time.Now}
	read, update := authz.Policy("notification", "read"), authz.Policy("notification", "update")
	for _, r := range []httpx.Route{
		{Method: http.MethodPost, Path: CompletionPath, Permission: authz.Peer(), Handler: s.ingest},
		{Method: http.MethodGet, Path: "/api/v2/notification/config", Permission: read, Handler: s.getConfig},
		{Method: http.MethodPut, Path: "/api/v2/notification/config", Permission: update, Handler: s.putConfig},
		{Method: http.MethodPost, Path: "/api/v2/notification/preview", Permission: read, Handler: s.preview},
		{Method: http.MethodPost, Path: "/api/v2/notification/test", Permission: update, Handler: s.test},
		{Method: http.MethodGet, Path: "/api/v2/notification/sample", Permission: read, Handler: s.sample},
	} {
		h.Router.Handle(r)
	}
	return nil
}

type service struct {
	db       store.Store
	clusters plugin.Clusters
	mail     *mail.Hub
	now      func() time.Time
}

type outcome struct {
	Outcome   string `json:"outcome"`
	Reason    string `json:"reason,omitempty"`
	Duplicate bool   `json:"duplicate,omitempty"`
}

func skipped(reason string) outcome { return outcome{Outcome: "skipped", Reason: reason} }

func readBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		return nil, apperr.ErrUnsupportedMedia
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return nil, apperr.ErrTooLarge
	}
	if err != nil {
		return nil, errMalformed
	}
	return b, nil
}

func decodeBody(w http.ResponseWriter, r *http.Request, dst any) error {
	b, err := readBody(w, r, maxAdminBytes)
	if err != nil {
		return err
	}
	return decodeStrict(b, dst)
}

// ingest answers with a bare {"outcome": ...} object as the slave sender expects; any non-200 makes it retry.
func (s *service) ingest(w http.ResponseWriter, r *http.Request) error {
	cl, err := statusreport.PeerCluster(r, s.clusters)
	if err != nil {
		return err
	}
	body, err := readBody(w, r, maxEventBytes)
	if err != nil {
		return err
	}
	ev, err := parseEvent(body, cl.ID)
	if err != nil {
		return err
	}
	if ev.ActiveSeconds <= MinActiveSeconds {
		return reply(w, skipped("below_threshold"))
	}
	if ev.UID == 0 {
		return reply(w, skipped("unmanaged_uid"))
	}
	ctx := r.Context()
	s.recordSample(ctx, cl, ev)
	if dup, err := s.claim(ctx, cl.ID, ev); err != nil || dup != nil {
		if err != nil {
			return err
		}
		return reply(w, *dup)
	}
	out, err := s.deliver(ctx, cl, ev)
	bg := context.WithoutCancel(ctx)
	if err != nil {
		reason := "error"
		var se *mail.Error
		if errors.As(err, &se) {
			reason = se.Kind
		}
		if ferr := s.finish(bg, cl.ID, ev.ID, "failed", reason); ferr != nil {
			slog.ErrorContext(ctx, "job notification state update failed", "err", ferr)
		}
		return apperr.ErrUnavailable.Wrap(err)
	}
	status := "sent"
	if out.Outcome != "smtp_accepted" {
		status = "skipped"
	}
	// The outcome is already decided; a lost state update must not trigger a resend.
	if err := s.finish(bg, cl.ID, ev.ID, status, out.Reason); err != nil {
		slog.ErrorContext(ctx, "job notification state update failed", "err", err)
	}
	return reply(w, out)
}

func reply(w http.ResponseWriter, o outcome) error {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	return json.NewEncoder(w).Encode(o)
}

// claim reserves the event for sending, or returns the recorded outcome of an earlier delivery.
func (s *service) claim(ctx context.Context, cluster string, ev event) (*outcome, error) {
	now, fp := s.now().UTC(), ev.fingerprint(cluster)
	res, err := s.db.Exec(ctx, `INSERT INTO v2_job_notifications (cluster_id, event_id, sluid, job_id, uid, fingerprint, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 'sending', ?, ?) ON CONFLICT DO NOTHING`, cluster, ev.ID, ev.SLUID, ev.JobID, ev.UID, fp, now, now)
	if err != nil {
		return nil, err
	}
	if res.RowsAffected > 0 {
		return nil, nil
	}
	var got, status, reason, updated string
	if err := store.QueryRow(ctx, s.db, "SELECT fingerprint, status, reason, updated_at FROM v2_job_notifications WHERE cluster_id = ? AND event_id = ?",
		[]any{cluster, ev.ID}, &got, &status, &reason, &updated); err != nil {
		return nil, err
	}
	if got != fp {
		return nil, errConflict
	}
	switch status {
	case "sent":
		return &outcome{Outcome: "smtp_accepted", Duplicate: true}, nil
	case "skipped":
		return &outcome{Outcome: "skipped", Reason: reason, Duplicate: true}, nil
	case "sending":
		if t, err := time.Parse(time.RFC3339Nano, updated); err == nil && now.Sub(t) < sendingLease {
			return nil, errBusy
		}
	}
	res, err = s.db.Exec(ctx, `UPDATE v2_job_notifications SET status = 'sending', reason = '', attempts = attempts + 1, updated_at = ?
		WHERE cluster_id = ? AND event_id = ? AND status = ? AND updated_at = ?`, now, cluster, ev.ID, status, updated)
	if err != nil {
		return nil, err
	}
	if res.RowsAffected == 0 {
		return nil, errBusy
	}
	return nil, nil
}

func (s *service) finish(ctx context.Context, cluster, id, status, reason string) error {
	_, err := s.db.Exec(ctx, "UPDATE v2_job_notifications SET status = ?, reason = ?, updated_at = ? WHERE cluster_id = ? AND event_id = ?",
		status, reason, s.now().UTC(), cluster, id)
	return err
}

func displayCluster(c plugin.Cluster) clusterView {
	name := c.Name
	if name == "" || jsLen(name) > 200 || strings.ContainsAny(name, "\r\n\x00") {
		name = c.ID
	}
	return clusterView{ID: c.ID, DisplayName: name}
}

func (s *service) deliver(ctx context.Context, cl plugin.Cluster, ev event) (outcome, error) {
	cfg, _, err := s.loadRow(ctx)
	if err != nil {
		return outcome{}, err
	}
	if !cfg.Enabled {
		return skipped("disabled"), nil
	}
	rows, err := s.db.Query(ctx, `SELECT username, COALESCE(nickname, ''), COALESCE(mail, ''), COALESCE(status, 0) FROM users
		WHERE uid_number = ? AND deleted_at IS NULL LIMIT 2`, ev.UID)
	if err != nil {
		return outcome{}, err
	}
	var users []recipient
	var userStatus int64
	for rows.Next() {
		var u recipient
		if err := rows.Scan(&u.Username, &u.Nickname, &u.Mail, &userStatus); err != nil {
			rows.Close()
			return outcome{}, err
		}
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return outcome{}, err
	}
	rows.Close()
	switch {
	case len(users) == 0:
		return skipped("user_missing"), nil
	case len(users) > 1:
		return skipped("ambiguous_uid"), nil
	case userStatus != 1:
		return skipped("user_disabled"), nil
	case !mail.ValidAddress(users[0].Mail):
		return skipped("user_email_invalid"), nil
	}
	u := users[0]
	rendered, err := renderNotification(cfg.Template, ev.Context, u, displayCluster(cl), cfg.TimeZone)
	if err != nil {
		return outcome{}, err
	}
	err = s.mail.Send(ctx, cfg.Provider, message(ev.ID, u.Mail, "job", rendered))
	switch {
	case mail.Unavailable(err):
		return skipped("mail_provider_unavailable"), nil
	case mail.RecipientRejected(err):
		return skipped("smtp_recipient_rejected"), nil
	case err != nil:
		return outcome{}, err
	}
	return outcome{Outcome: "smtp_accepted"}, nil
}

func message(id, to, purpose string, r Rendered) mail.Message {
	return mail.Message{ID: id, To: to, Subject: r.Subject, Text: r.Text, HTML: r.HTML, Purpose: purpose}
}

func (s *service) getConfig(w http.ResponseWriter, r *http.Request) error {
	c, _, err := s.loadRow(r.Context())
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, c)
	return nil
}

func (s *service) putConfig(w http.ResponseWriter, r *http.Request) error {
	var in ConfigInput
	if err := decodeBody(w, r, &in); err != nil {
		return err
	}
	v, err := s.save(r.Context(), in)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, v)
	return nil
}

func (s *service) caller(ctx context.Context) (recipient, error) {
	var u recipient
	p := authn.FromContext(ctx)
	if p == nil {
		return u, apperr.ErrUnauthenticated
	}
	err := store.QueryRow(ctx, s.db, "SELECT username, COALESCE(nickname, ''), COALESCE(mail, '') FROM users WHERE id = ? AND deleted_at IS NULL",
		[]any{p.UserID}, &u.Username, &u.Nickname, &u.Mail)
	if errors.Is(err, store.ErrNoRows) {
		return u, apperr.ErrUnauthenticated
	}
	return u, err
}

func (s *service) preview(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Template *Template `json:"template"`
		TimeZone *string   `json:"timeZone"`
	}
	if err := decodeBody(w, r, &in); err != nil {
		return err
	}
	ctx := r.Context()
	cfg, _, err := s.loadRow(ctx)
	if err != nil {
		return err
	}
	if in.Template != nil {
		cfg.Template = *in.Template
	}
	if in.TimeZone != nil {
		cfg.TimeZone = *in.TimeZone
	}
	u, err := s.caller(ctx)
	if err != nil {
		return err
	}
	out, err := renderNotification(cfg.Template, previewEvent(), u, clusterView{ID: previewCluster, DisplayName: previewCluster}, cfg.TimeZone)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}

// test sends the saved template to the caller (or "to") through the configured provider, regardless of the enabled toggle.
func (s *service) test(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		To *string `json:"to"`
	}
	if err := decodeBody(w, r, &in); err != nil {
		return err
	}
	ctx := r.Context()
	cfg, _, err := s.loadRow(ctx)
	if err != nil {
		return err
	}
	u, err := s.caller(ctx)
	if err != nil {
		return err
	}
	to := u.Mail
	if in.To != nil {
		to = *in.To
	}
	if !mail.ValidAddress(to) {
		return invalid("recipient email is invalid")
	}
	rendered, err := renderNotification(cfg.Template, previewEvent(), u, clusterView{ID: previewCluster, DisplayName: previewCluster}, cfg.TimeZone)
	if err != nil {
		return err
	}
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	err = s.mail.Send(ctx, cfg.Provider, message("notification-test/"+hex.EncodeToString(nonce), to, "job-test", rendered))
	var se *mail.Error
	switch {
	case mail.Unavailable(err):
		return invalid("the notification mail provider is missing or disabled")
	case errors.As(err, &se):
		return apperr.New(http.StatusBadGateway, "smtp_failed", se.Kind)
	case err != nil:
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, outcome{Outcome: "smtp_accepted"})
	return nil
}

const sampleKind = "job-completion"

// recordSample keeps the latest finished job's template context for administrators writing
// templates. It is best effort and never affects delivery.
func (s *service) recordSample(ctx context.Context, cl plugin.Cluster, ev event) {
	var u recipient
	_ = store.QueryRow(ctx, s.db, "SELECT username, COALESCE(nickname, ''), COALESCE(mail, '') FROM users WHERE uid_number = ? AND deleted_at IS NULL LIMIT 1",
		[]any{ev.UID}, &u.Username, &u.Nickname, &u.Mail)
	cfg, _, err := s.loadRow(ctx)
	if err != nil {
		return
	}
	data, err := notificationContext(ev.Context, u, displayCluster(cl), nil, cfg.TimeZone)
	if err == nil {
		err = mail.SaveSample(ctx, s.db, sampleKind, data, s.now())
	}
	if err != nil {
		slog.WarnContext(ctx, "job notification sample not saved", "err", err)
	}
}

// sample returns the latest real job context, or the built-in example, for template authors.
func (s *service) sample(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	cfg, _, err := s.loadRow(ctx)
	if err != nil {
		return err
	}
	u, err := s.caller(ctx)
	if err != nil {
		return err
	}
	example, err := notificationContext(previewEvent(), u, clusterView{ID: previewCluster, DisplayName: previewCluster}, nil, cfg.TimeZone)
	if err != nil {
		return err
	}
	out, err := mail.LoadSample(ctx, s.db, sampleKind, example, cfg.Template.Constants, variables)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}
