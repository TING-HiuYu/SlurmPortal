package webaccess

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/apperr"
	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
	"github.com/TING-HiuYu/SlurmPortal/internal/httpx"
	"github.com/TING-HiuYu/SlurmPortal/internal/mail"
)

// TransferEmail configures the mail sent when a cross-cluster transfer finishes.
type TransferEmail struct {
	Enabled bool `json:"enabled"`
	// Provider names the mail hub provider; "" uses the default provider.
	Provider string        `json:"provider"`
	TimeZone string        `json:"timeZone"`
	Template mail.Template `json:"template"`
}

const sampleKind = "file-transfer"

var transferRoots = mail.Roots("user", "transfer", "source", "destination", "constants")

var transferVariables = func() []mail.Variable {
	pairs := [][2]string{
		{"user.nickname", "用户昵称"}, {"user.username", "用户名"}, {"user.mail", "用户邮箱"},
		{"transfer.id", "传输编号"}, {"transfer.status", "结果：completed / partial / failed"}, {"transfer.state", "结果（大写）：COMPLETED / PARTIAL / FAILED"},
		{"transfer.state_text", "结果（中文）：完成 / 部分完成 / 失败"}, {"transfer.source_paths", "源路径，逗号分隔"}, {"transfer.source_count", "源路径数量"},
		{"transfer.source_paths_list.0", "第一个源路径（序号从 0 开始）"}, {"transfer.destination_path", "目标目录"},
		{"transfer.size", "传输总量（格式化）"}, {"transfer.total_bytes", "传输总量（字节）"}, {"transfer.bytes_sent", "已发送字节"},
		{"transfer.duration", "耗时（时:分:秒）"}, {"transfer.elapsed_seconds", "耗时（秒）"}, {"transfer.speed", "平均速率（格式化）"},
		{"transfer.average_mb_per_second", "平均速率（MB/s）"}, {"transfer.exit_code", "rsync 退出码"}, {"transfer.error", "错误代码；成功时为空"},
		{"transfer.started_at", "开始时间（RFC3339 UTC）"}, {"transfer.finished_at", "结束时间（RFC3339 UTC）"},
		{"transfer.started_at_local", "开始时间（配置的时区）"}, {"transfer.finished_at_local", "结束时间（配置的时区）"},
		{"source.id", "源集群标识"}, {"source.display_name", "源集群名称"}, {"destination.id", "目标集群标识"}, {"destination.display_name", "目标集群名称"},
		{"constants.<名称>", "模板常量"},
	}
	out := make([]mail.Variable, len(pairs))
	for i, p := range pairs {
		out[i] = mail.Variable{Name: p[0], Description: p[1]}
	}
	return out
}()

// defaultTransferHTML follows the job-notification layout in a blue palette.
const defaultTransferHTML = `<!doctype html>
<html lang="zh-CN">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>文件传输结束通知</title></head>
<body style="margin:0;padding:0;background:#edf1f7;color:#26364d;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI','PingFang SC','Microsoft YaHei',sans-serif;">
<table role="presentation" width="100%" cellspacing="0" cellpadding="0" style="background:#edf1f7;"><tr><td align="center" style="padding:28px 16px;">
<table role="presentation" width="100%" cellspacing="0" cellpadding="0" style="max-width:620px;background:#ffffff;border:1px solid #d9e1ed;border-radius:12px;overflow:hidden;">
<tr><td style="padding:30px 30px 24px;background:#1e3a64;color:#ffffff;">
<p style="margin:0 0 15px;font-size:11px;letter-spacing:2px;color:#b9cbe6;">{{ constants.organization }} / HPC Service</p>
<h1 style="margin:0;font-size:26px;font-weight:600;line-height:1.4;">文件传输结束通知</h1>
<p style="margin:12px 0 0;font-size:13px;color:#c9d7ec;">{{ source.display_name }} → {{ destination.display_name }} · {{ user.nickname }}</p>
</td></tr>
<tr><td style="padding:0px 30px 0;">
<table role="presentation" width="100%" cellspacing="0" cellpadding="0" style="margin:22px 0 0;border-collapse:collapse;">
<tr><td style="padding:14px 16px;background:#edf2f9;border-radius:7px 0 0 7px;width:50%;vertical-align:top;"><p style="margin:0 0 7px;font-size:11px;color:#7d8ba1;">最终状态</p><strong style="font-size:17px;font-weight:600;">{{ transfer.state_text }}（{{ transfer.state }}）</strong></td><td style="padding:14px 16px;background:#edf2f9;border-radius:0 7px 7px 0;vertical-align:top;"><p style="margin:0 0 7px;font-size:11px;color:#7d8ba1;">传输耗时</p><strong style="font-size:17px;font-weight:600;">{{ transfer.duration }}</strong></td></tr>
</table>
</td></tr>
<tr><td style="padding:25px 30px 0;">
<h2 style="font-size:15px;margin:0 0 13px;font-weight:600;">传输内容</h2>
<table role="presentation" width="100%" cellspacing="0" cellpadding="0" style="font-size:12px;line-height:1.8;border-top:1px solid #e3e9f1;">
<tr><td style="padding:10px 0 7px;width:31%;color:#86919f;vertical-align:top;">源路径</td><td style="padding:10px 0 7px;overflow-wrap:anywhere;word-break:break-word;">{{ source.display_name }} · {{ transfer.source_paths }}</td></tr>
<tr><td style="padding:7px 0;color:#86919f;vertical-align:top;">目标目录</td><td style="padding:7px 0;overflow-wrap:anywhere;word-break:break-word;">{{ destination.display_name }} · {{ transfer.destination_path }}</td></tr>
<tr><td style="padding:7px 0;color:#86919f;vertical-align:top;">传输总量</td><td style="padding:7px 0;">{{ transfer.size }}</td></tr>
<tr><td style="padding:7px 0;color:#86919f;vertical-align:top;">平均速率</td><td style="padding:7px 0;">{{ transfer.speed }}</td></tr>
<tr><td style="padding:7px 0;color:#86919f;vertical-align:top;">退出码</td><td style="padding:7px 0;">{{ transfer.exit_code }}</td></tr>
</table>
</td></tr>
<tr><td style="padding:24px 30px 28px;">
<h2 style="font-size:15px;margin:0 0 13px;font-weight:600;">执行时间</h2>
<table role="presentation" width="100%" cellspacing="0" cellpadding="0" style="font-size:12px;line-height:1.8;border-top:1px solid #e3e9f1;">
<tr><td style="padding:10px 0 6px;width:31%;color:#86919f;vertical-align:top;">开始</td><td style="padding:10px 0 6px;">{{ transfer.started_at_local }}</td></tr>
<tr><td style="padding:6px 0;color:#86919f;vertical-align:top;">结束</td><td style="padding:6px 0;">{{ transfer.finished_at_local }}</td></tr>
</table>
</td></tr>
<tr><td style="padding:18px 30px;background:#f6f8fb;border-top:1px solid #e3e9f1;color:#949eac;font-size:10px;line-height:1.9;">{{ constants.footer_note }}<br>传输编号：{{ transfer.id }}<br>支持联系：{{ constants.support_email }}</td></tr>
</table>
<p style="margin:18px 0 0;color:#a3adbb;font-size:10px;letter-spacing:1px;">{{ constants.organization }} · Email Notification Service</p>
</td></tr></table>
</body></html>
`

func defaultTransferEmail() TransferEmail {
	return TransferEmail{TimeZone: mail.DefaultTimeZone, Template: mail.Template{
		Subject:   "[Slurm Notification] 文件传输{{ transfer.state_text }}：{{ source.display_name }} → {{ destination.display_name }}",
		HTML:      defaultTransferHTML,
		Constants: map[string]any{"organization": "HPC", "support_email": "-", "footer_note": "此邮箱不接收回复，感谢使用计算集群。"},
	}}
}

func (t *TransferEmail) validate() error {
	if t.Provider != "" && !identifier.MatchString(t.Provider) {
		return errors.New("invalid mail provider name")
	}
	zone, err := mail.NormalizeTimeZone(cmpOr(t.TimeZone, mail.DefaultTimeZone))
	if err != nil {
		return err
	}
	t.TimeZone = zone
	if t.Template.Subject == "" && t.Template.HTML == "" {
		t.Template = defaultTransferEmail().Template
	}
	t.Template, err = t.Template.Validate(transferRoots)
	return err
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func optional(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}

// transferContext is what transfer templates can reference.
func transferContext(ev TransferEvent, u mailUser, src, dst Cluster, zone string) map[string]any {
	loc, err := time.LoadLocation(zone)
	if err != nil {
		loc, zone = time.UTC, "UTC"
	}
	state := map[string]string{"completed": "COMPLETED", "partial": "PARTIAL", "failed": "FAILED"}[ev.Status]
	text := map[string]string{"completed": "完成", "partial": "部分完成", "failed": "失败"}[ev.Status]
	speed := "Unavailable"
	if ev.AverageMBPerSecond != nil {
		speed = fmt.Sprintf("%.2f MB/s", *ev.AverageMBPerSecond)
	}
	paths := make([]any, len(ev.SourcePaths))
	for i, p := range ev.SourcePaths {
		paths[i] = p
	}
	exit := any(nil)
	if ev.ExitCode != nil {
		exit = *ev.ExitCode
	}
	errCode := ""
	if ev.Error != nil {
		errCode = *ev.Error
	}
	nick := cmpOr(u.Nickname, u.Username)
	return map[string]any{
		"user": map[string]any{"username": u.Username, "nickname": nick, "mail": u.Mail},
		"transfer": map[string]any{
			"id": ev.TransferID, "status": ev.Status, "state": state, "state_text": text,
			"source_paths": bounded(strings.Join(ev.SourcePaths, ", "), 8192), "source_paths_list": paths, "source_count": len(ev.SourcePaths),
			"destination_path": ev.DestinationPath, "size": formatBytes(ev.TotalBytes), "total_bytes": optional(ev.TotalBytes), "bytes_sent": optional(ev.BytesSent),
			"duration": formatDuration(ev.ElapsedSeconds), "elapsed_seconds": optional(ev.ElapsedSeconds), "speed": speed,
			"average_mb_per_second": optional(ev.AverageMBPerSecond), "exit_code": exit, "error": errCode,
			"started_at": ev.StartedAt, "finished_at": ev.FinishedAt,
			"started_at_local": mail.LocalTime(ev.StartedAt, loc, zone), "finished_at_local": mail.LocalTime(ev.FinishedAt, loc, zone),
		},
		"source":      map[string]any{"id": src.ID, "display_name": src.Name},
		"destination": map[string]any{"id": dst.ID, "display_name": dst.Name},
	}
}

type mailUser struct{ Username, Nickname, Mail string }

func exampleTransfer(now time.Time) TransferEvent {
	code, total, elapsed, speed := 0, 5.3e9, 98.0, 54.08
	return TransferEvent{TransferID: "3f0d5d6e-5c1a-4b7e-9c55-0a1b2c3d4e5f", Status: "completed", ExitCode: &code, SourcePaths: []string{"/HOME/alice/data/run-42", "/HOME/alice/data/notes.txt"},
		DestinationPath: "/HOME/alice/incoming", TotalBytes: &total, BytesSent: &total, ElapsedSeconds: &elapsed, AverageMBPerSecond: &speed,
		StartedAt: now.Add(-98 * time.Second).UTC().Format(time.RFC3339), FinishedAt: now.UTC().Format(time.RFC3339)}
}

func (s *service) caller(r *http.Request) (mailUser, error) {
	p := authn.FromContext(r.Context())
	if p == nil {
		return mailUser{}, apperr.ErrUnauthenticated
	}
	u, err := s.identity.GetUser(r.Context(), p.UserID)
	if err != nil {
		return mailUser{}, err
	}
	return mailUser{u.Username, u.Nickname, u.Mail}, nil
}

func (s *service) exampleContext(r *http.Request) (map[string]any, error) {
	u, err := s.caller(r)
	if err != nil {
		return nil, err
	}
	return transferContext(exampleTransfer(s.now()), u, Cluster{ID: "source-cluster", Name: "源集群"}, Cluster{ID: "destination-cluster", Name: "目标集群"}, mail.DefaultTimeZone), nil
}

func (s *service) getTransferEmail(w http.ResponseWriter, r *http.Request) error {
	cfg, err := s.load(r.Context())
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, cfg.TransferEmail)
	return nil
}

func (s *service) putTransferEmail(w http.ResponseWriter, r *http.Request) error {
	var in TransferEmail
	if err := httpx.DecodeJSONLimit(w, r, &in, 256<<10); err != nil {
		return err
	}
	if err := in.validate(); err != nil {
		return apperr.BadRequest(err.Error())
	}
	if in.Enabled || in.Provider != "" {
		ok, err := s.mail.Exists(r.Context(), in.Provider)
		if err != nil {
			return err
		}
		if !ok {
			return apperr.BadRequest("mail provider not found")
		}
	}
	cfg, err := s.load(r.Context())
	if err != nil {
		return err
	}
	cfg.TransferEmail = in
	if err := s.save(r.Context(), cfg); err != nil {
		return err
	}
	httpx.SetTarget(r.Context(), "web-access:transfer-email")
	httpx.WriteJSON(w, http.StatusOK, in)
	return nil
}

// previewTransfer renders the posted (or saved) template with the latest real transfer, else an example.
func (s *service) previewTransfer(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Template *mail.Template `json:"template"`
		TimeZone *string        `json:"timeZone"`
	}
	if err := httpx.DecodeJSONLimit(w, r, &in, 256<<10); err != nil {
		return err
	}
	cfg, err := s.load(r.Context())
	if err != nil {
		return err
	}
	t := cfg.TransferEmail.Template
	if in.Template != nil {
		t = *in.Template
	}
	sample, err := s.sampleFor(r, t.Constants)
	if err != nil {
		return err
	}
	out, err := mail.Render(t, transferRoots, sample.Context, transferVariables)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}

func (s *service) sampleFor(r *http.Request, constants map[string]any) (mail.Sample, error) {
	example, err := s.exampleContext(r)
	if err != nil {
		return mail.Sample{}, err
	}
	return mail.LoadSample(r.Context(), s.db, sampleKind, example, constants, transferVariables)
}

func (s *service) transferSample(w http.ResponseWriter, r *http.Request) error {
	cfg, err := s.load(r.Context())
	if err != nil {
		return err
	}
	out, err := s.sampleFor(r, cfg.TransferEmail.Template.Constants)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}

// testTransfer sends the saved template, filled with the sample, to the caller (or "to").
func (s *service) testTransfer(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		To *string `json:"to"`
	}
	if err := httpx.DecodeJSONLimit(w, r, &in, 4096); err != nil {
		return err
	}
	cfg, err := s.load(r.Context())
	if err != nil {
		return err
	}
	u, err := s.caller(r)
	if err != nil {
		return err
	}
	to := u.Mail
	if in.To != nil {
		to = *in.To
	}
	if !mail.ValidAddress(to) {
		return apperr.BadRequest("recipient email is invalid")
	}
	sample, err := s.sampleFor(r, cfg.TransferEmail.Template.Constants)
	if err != nil {
		return err
	}
	out, err := mail.Render(cfg.TransferEmail.Template, transferRoots, sample.Context, nil)
	if err != nil {
		return err
	}
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	err = s.mail.Send(r.Context(), cfg.TransferEmail.Provider, mail.Message{ID: "transfer-test/" + hex.EncodeToString(nonce), To: to, Purpose: "transfer-test", Subject: out.Subject, Text: out.Text, HTML: out.HTML})
	var me *mail.Error
	switch {
	case mail.Unavailable(err):
		return apperr.BadRequest("the transfer mail provider is missing or disabled")
	case errors.As(err, &me):
		return apperr.New(http.StatusBadGateway, "smtp_failed", me.Kind)
	case err != nil:
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"outcome": "smtp_accepted"})
	return nil
}
