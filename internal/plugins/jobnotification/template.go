package jobnotification

import (
	"encoding/json"
	"regexp"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/apperr"
	"github.com/TING-HiuYu/SlurmPortal/internal/mail"
)

// The template engine is shared with other mail senders (internal/mail); this file holds what is
// specific to job notifications: the placeholder roots, the default template and the context.
type (
	Template = mail.Template
	Variable = mail.Variable
	Rendered = mail.Rendered
)

const defaultTimeZone = mail.DefaultTimeZone

var (
	roots             = mail.Roots("user", "job", "cluster", "result", "duration", "usage", "raw", "observed_at", "observed_at_local", "constants")
	jsLen             = mail.JSLen
	normalizeTimeZone = mail.NormalizeTimeZone
	set               = mail.Roots
	utcPattern        = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]{1,9})?Z$`)
)

func invalid(msg string) error { return apperr.BadRequest(msg) }

var defaultTemplate = Template{
	Subject: "[{{ cluster.display_name }}] 作业 {{ job.job_id }} 已结束：{{ result.state }}",
	HTML: "<p>{{ user.nickname }}，您好：</p><p>作业 {{ job.job_id }}（{{ job.name }}）已结束：{{ result.state }}。</p>" +
		"<p>最后一次执行的有效运行时长：{{ job.duration }}。</p>" +
		"<p>申请资源：{{ job.requested_tres }}<br>分配资源：{{ job.allocated_tres }}</p>" +
		"<p>分区：{{ job.partition }}<br>批处理主机：{{ job.batch_host }}<br>节点：{{ job.nodes }}</p>" +
		"<p>作业命令或脚本：{{ job.command }}<br>工作目录：{{ job.working_directory }}<br>提交命令：{{ job.submit_line }}</p>" +
		"<p>标准输出（展开路径）：{{ job.stdout_expanded }}<br>标准输出（原始模板）：{{ job.standard_output }}<br>标准错误（展开路径）：{{ job.stderr_expanded }}</p>",
	Constants: map[string]any{},
}

type recipient struct {
	Username string
	Nickname string
	Mail     string
}

type clusterView struct {
	ID          string
	DisplayName string
}

func int64Value(v any) int64 {
	switch x := v.(type) {
	case json.Number:
		n, _ := x.Int64()
		return n
	case float64:
		return int64(x)
	case int64:
		return x
	}
	return 0
}

func notificationContext(ev map[string]any, u recipient, cl clusterView, constants map[string]any, zone string) (map[string]any, error) {
	zone, err := normalizeTimeZone(zone)
	if err != nil {
		return nil, err
	}
	loc, _ := time.LoadLocation(zone)
	ctx := make(map[string]any, len(ev)+4)
	for k, v := range ev {
		ctx[k] = v
	}
	job := map[string]any{}
	if j, ok := ev["job"].(map[string]any); ok {
		for k, v := range j {
			job[k] = v
		}
	}
	var active int64
	if d, ok := ev["duration"].(map[string]any); ok {
		active = int64Value(d["active_seconds"])
	}
	job["duration"] = mail.DurationText(active)
	job["submitted_at_local"] = mail.LocalTime(job["submitted_at"], loc, zone)
	job["started_at_local"] = mail.LocalTime(job["started_at"], loc, zone)
	job["ended_at_local"] = mail.LocalTime(job["ended_at"], loc, zone)
	nick := u.Nickname
	if nick == "" {
		nick = u.Username
	}
	ctx["user"] = map[string]any{"username": u.Username, "nickname": nick, "mail": u.Mail}
	ctx["job"] = job
	ctx["observed_at_local"] = mail.LocalTime(ev["observed_at"], loc, zone)
	ctx["cluster"] = map[string]any{"id": cl.ID, "display_name": cl.DisplayName}
	ctx["constants"] = constants
	return ctx, nil
}

func renderNotification(t Template, ev map[string]any, u recipient, cl clusterView, zone string) (Rendered, error) {
	t, err := t.Validate(roots)
	if err != nil {
		return Rendered{}, err
	}
	ctx, err := notificationContext(ev, u, cl, t.Constants, zone)
	if err != nil {
		return Rendered{}, err
	}
	return mail.Render(t, roots, ctx, variables)
}
